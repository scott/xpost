// Package poster is the only code path that publishes. It holds the Poster
// interface, a fake that never touches the network, and the dispatcher
// that posts approved, due drafts through whichever Poster it is given.
// The real X poster (internal/xauth.XPoster) implements the same interface;
// cmd/xpost starts it only when XPOST_SCHEDULER=on.
//
// No HTTP handler calls into this package: agents (and the owner's browser)
// can never post. Only the server's own loop does, and only for drafts the
// owner approved.
package poster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/weight"
)

// Request is one part to publish. ReplyTo is the previous part's post id
// for thread replies. The dispatcher only ever fills it from its own log of
// the same draft, so replies to anyone else's posts are not possible.
// MediaIDs are X media ids the dispatcher uploaded for this part.
type Request struct {
	Account        string
	Text           string
	MediaIDs       []string
	ReplyTo        string
	IdempotencyKey string
}

// Upload is one image to send to X's media upload.
type Upload struct {
	Account        string
	Mime           string
	Data           []byte
	IdempotencyKey string
}

// Uploaded is X's answer for an upload or a processing check. Processing
// means X is still working on it (check again at CheckAt); Failed (a
// reason) means X could not process it.
type Uploaded struct {
	MediaID    string
	ExpiresAt  time.Time // zero: X did not say
	Processing bool
	CheckAt    time.Time
	Failed     string
}

// MediaPoster is the optional Poster side for images. A Poster without it
// cannot post drafts with images; they wait. Failures are *Error, classified
// like posts.
type MediaPoster interface {
	Upload(ctx context.Context, u Upload) (Uploaded, error)
	MediaStatus(ctx context.Context, account, mediaID string) (Uploaded, error)
	SetAlt(ctx context.Context, account, mediaID, alt string) error
}

// Result identifies the published post.
type Result struct {
	PostID string
	URL    string
}

// Poster publishes one part.
type Poster interface {
	Post(ctx context.Context, r Request) (Result, error)
}

// Readier is an optional Poster method: whether account can post at all
// (connected, tokens present). Drafts for an account that is not ready stay
// approved and are not attempted.
type Readier interface {
	Ready(ctx context.Context, account string) error
}

// Kind classifies a failed Post by what it says about X's state.
type Kind int

const (
	// Ambiguous: the post may or may not exist (timeout, dropped
	// connection, 5xx, unreadable reply). Never retried automatically.
	Ambiguous Kind = iota
	// Refused: X answered with a definite refusal (4xx). Not posted.
	Refused
	// RateLimited: X answered 429. Not posted; retry at RetryAt.
	RateLimited
	// NotSent: the request never reached X (no connection, a refresh
	// that failed, a dial error). Not posted; retried with backoff.
	NotSent
)

func (k Kind) String() string {
	return [...]string{"ambiguous", "refused", "rate limited", "not sent"}[k]
}

// Error is a classified Post failure. Msg must never contain a token.
type Error struct {
	Kind    Kind
	Status  int       // HTTP status, if any
	Msg     string    // short, safe to log and show
	RetryAt time.Time // RateLimited: when X said to come back (zero: unknown)
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s (HTTP %d)", e.Msg, e.Status)
	}
	return e.Msg
}

// KindOf classifies err. Anything that is not an *Error is treated as
// Ambiguous: when in doubt, assume it may have posted.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return Ambiguous
}

// Fake records requests in memory and optionally logs them. It makes no
// network calls. Fail, if set, decides which posts fail; UploadFail and
// AltFail do the same for image uploads and alt text. ProcessChecks makes
// each upload report "processing" for that many status checks.
type Fake struct {
	mu            sync.Mutex
	calls         []Request
	uploads       []Upload
	alts          map[string]string
	pending       map[string]int
	n, m          int
	Log           io.Writer
	Fail          func(Request) error
	UploadFail    func(Upload) error
	AltFail       func(mediaID, alt string) error
	ProcessChecks int
	MediaTTL      time.Duration // default 24h
	Now           func() time.Time
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Post implements Poster without any network access.
func (f *Fake) Post(_ context.Context, r Request) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	if f.Fail != nil {
		if err := f.Fail(r); err != nil {
			return Result{}, err
		}
	}
	f.n++
	id := fmt.Sprintf("fake-%d", f.n)
	if f.Log != nil {
		fmt.Fprintf(f.Log, "fake poster: @%s part %s (%d chars weighted, %d images) -> %s\n", r.Account, r.IdempotencyKey, weight.Count(r.Text).Weighted, len(r.MediaIDs), id)
	}
	// .invalid is reserved (RFC 2606): this URL can never resolve.
	return Result{PostID: id, URL: "https://x.invalid/" + r.Account + "/status/" + id}, nil
}

// Upload implements MediaPoster without any network access.
func (f *Fake) Upload(_ context.Context, u Upload) (Uploaded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, u)
	if f.UploadFail != nil {
		if err := f.UploadFail(u); err != nil {
			return Uploaded{}, err
		}
	}
	f.m++
	id := fmt.Sprintf("%d", 1700000000000000000+f.m)
	ttl := f.MediaTTL
	if ttl == 0 {
		ttl = 24 * time.Hour
	}
	out := Uploaded{MediaID: id, ExpiresAt: f.now().Add(ttl)}
	if f.ProcessChecks > 0 {
		if f.pending == nil {
			f.pending = map[string]int{}
		}
		f.pending[id] = f.ProcessChecks
		out.Processing, out.CheckAt = true, f.now().Add(5*time.Second)
	}
	return out, nil
}

// MediaStatus implements MediaPoster.
func (f *Fake) MediaStatus(_ context.Context, _, id string) (Uploaded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending[id] > 0 {
		f.pending[id]--
		if f.pending[id] > 0 {
			return Uploaded{MediaID: id, Processing: true, CheckAt: f.now().Add(5 * time.Second)}, nil
		}
	}
	return Uploaded{MediaID: id}, nil
}

// SetAlt implements MediaPoster.
func (f *Fake) SetAlt(_ context.Context, _, id, alt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AltFail != nil {
		if err := f.AltFail(id, alt); err != nil {
			return err
		}
	}
	if f.alts == nil {
		f.alts = map[string]string{}
	}
	f.alts[id] = alt
	return nil
}

// Calls returns a copy of every post request so far.
func (f *Fake) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.calls...)
}

// Uploads returns a copy of every upload so far.
func (f *Fake) Uploads() []Upload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Upload(nil), f.uploads...)
}

// Alt returns the alt text set on a media id.
func (f *Fake) Alt(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alts[id]
}

// Defaults for Dispatcher.
const (
	DefaultLease       = 2 * time.Minute // longer than one X call, refresh included
	DefaultMaxAttempts = 8               // automatic retries (429, not sent) before failing
	MaxBackoff         = 30 * time.Minute
	MaxRateLimitWait   = 24 * time.Hour
	// MediaMargin: an uploaded image is used only if its X media id is
	// valid for at least this long; otherwise it is uploaded again.
	MediaMargin = 15 * time.Minute
	// DefaultMediaTTL is assumed when X does not say when an upload
	// expires (short, so a stale id is re-uploaded rather than trusted).
	DefaultMediaTTL = time.Hour
	// MaxProcessingWait bounds how long one processing check waits.
	MaxProcessingWait = 10 * time.Minute
)

// Dispatcher posts approved drafts whose time has come.
type Dispatcher struct {
	Store       *store.Store
	Poster      Poster
	Log         *log.Logger
	Loc         *time.Location // display zone default, for the monthly cap
	Lease       time.Duration
	MaxAttempts int

	mu      sync.Mutex
	waitFor map[string]time.Time // account -> rate-limited until
}

// Key is the idempotency key for one part of one approved version.
func Key(d *store.Draft, part int) string {
	return fmt.Sprintf("d%d-v%d-p%d", d.ID, d.Version, part)
}

// MediaKey is the key for image slot (1-based) of a part.
func MediaKey(d *store.Draft, part, slot int) string {
	return fmt.Sprintf("%s-m%d", Key(d, part), slot)
}

// Report is what one run did.
type Report struct {
	Posted, Failed, Review, Waiting, Skipped int
	Block                                    string // why nothing could post, if so
}

func (d *Dispatcher) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

func (d *Dispatcher) lease() time.Duration {
	if d.Lease > 0 {
		return d.Lease
	}
	return DefaultLease
}

func (d *Dispatcher) maxAttempts() int {
	if d.MaxAttempts > 0 {
		return d.MaxAttempts
	}
	return DefaultMaxAttempts
}

func (d *Dispatcher) loc(ctx context.Context) *time.Location {
	def := d.Loc
	if def == nil {
		def = time.UTC
	}
	return d.Store.Zone(ctx, def)
}

func (d *Dispatcher) limitedUntil(account string, now time.Time) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.waitFor[account]
	return t, ok && now.Before(t)
}

func (d *Dispatcher) setLimited(account string, until time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waitFor == nil {
		d.waitFor = map[string]time.Time{}
	}
	d.waitFor[account] = until
}

// Backoff is the wait before automatic retry n (1-based): 1, 2, 4 ... min,
// at most MaxBackoff.
func Backoff(n int) time.Duration {
	b := time.Minute
	for i := 1; i < n && b < MaxBackoff; i++ {
		b *= 2
	}
	return min(b, MaxBackoff)
}

// Reconcile runs once at startup; see store.Reconcile.
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	review, resumable, err := d.Store.Reconcile(ctx, d.Store.Now())
	if err == nil && review+resumable > 0 {
		d.logf("startup reconcile: %d draft(s) need review (outcome unknown), %d will resume", review, resumable)
	}
	return err
}

// RunDue posts what is due at now. Every part's intent is committed before
// X is called and a part is sent only if its key has no posted, unknown or
// in-flight row, so a retry or restart never double-posts.
func (d *Dispatcher) RunDue(ctx context.Context, now time.Time) (Report, error) {
	var rep Report
	sp, err := d.Store.MonthSpend(ctx, d.loc(ctx), now)
	if err != nil {
		return rep, err
	}
	if rep.Block = sp.PostingBlock(); rep.Block != "" {
		return rep, nil
	}
	due, err := d.Store.DueDrafts(ctx, now)
	if err != nil {
		return rep, err
	}
	for _, du := range due {
		if ctx.Err() != nil {
			break
		}
		if err := d.one(ctx, now, du, &sp, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (d *Dispatcher) one(ctx context.Context, now time.Time, du store.Due, sp *store.Spend, rep *Report) error {
	if _, ok := d.limitedUntil(du.Account, now); ok {
		rep.Skipped++
		return nil
	}
	if r, ok := d.Poster.(Readier); ok {
		if err := r.Ready(ctx, du.Account); err != nil {
			rep.Skipped++
			return nil
		}
	}
	dr, err := d.Store.Draft(ctx, du.ID)
	if err != nil {
		return nil // deleted since listing
	}
	if dr.IsReply() {
		// Never reached (DueDrafts skips replies): the owner posts a
		// reply by hand on x.com, so nothing here may send one.
		rep.Skipped++
		return nil
	}
	// A promo posts with its blog post's live URL in place of
	// {{blog_url}}, and only once that post is published.
	parts, live, err := d.Store.PromoText(ctx, dr)
	if err != nil {
		return err
	}
	if !live {
		rep.Skipped++
		return nil
	}
	dr.Parts = parts
	mp, canMedia := d.Poster.(MediaPoster)
	if dr.Images() > 0 && !canMedia {
		rep.Skipped++
		return nil // this poster cannot upload images
	}
	rows, err := d.Store.DraftLog(ctx, dr.ID)
	if err != nil {
		return err
	}
	byKey := map[string]*store.LogEntry{}
	for _, e := range rows {
		byKey[e.IdemKey] = e
	}
	ups, err := d.Store.DraftUploads(ctx, dr.ID)
	if err != nil {
		return err
	}
	started := false
	var remaining int64
	images := 0 // images still to upload in the parts not yet posted
	alts := 0   // alt text calls still to make in those parts
	for i, p := range dr.Parts {
		e := byKey[Key(dr, i+1)]
		if e != nil && (e.Status == store.LogPosted || e.Status == store.LogUnknown || e.Status == store.LogPosting) {
			started = true
			continue
		}
		remaining += d.Store.PartCost(weight.HasURL(p.Text))
		for j, m := range p.Media {
			u := ups[MediaKey(dr, i+1, j+1)]
			usable := u.Usable(now, MediaMargin)
			if !usable {
				images++
			}
			if strings.TrimSpace(m.Alt) != "" && !(usable && u.AltDone) {
				alts++ // a new upload needs its alt text set again
			}
		}
	}
	remaining += d.Store.UploadCost(images) + d.Store.AltCost(alts)
	why := ""
	if images > 0 {
		why = sp.UploadBlock()
	}
	if why == "" && alts > 0 {
		why = sp.AltBlock()
	}
	if why != "" {
		// A media cost is unknown, so the cap can't count it: wait.
		rep.Skipped++
		if !started {
			if du.Status == store.StatusPosting {
				return d.Store.Unclaim(ctx, dr.ID, why)
			}
			return nil
		}
		if ok, err := d.Store.Claim(ctx, du, now.Add(d.lease())); err != nil || !ok {
			return err
		}
		return d.Store.Wait(ctx, dr.ID, now.Add(MaxProcessingWait), why)
	}
	if !started {
		// Spacing at post time: a new item starts no sooner than
		// MinSpacing after the account's previous item started.
		if last, ok := d.Store.LastStart(ctx, dr.Account, dr.ID); ok && now.Before(last.Add(store.MinSpacing)) {
			rep.Skipped++
			return nil
		}
	}
	if sp.Micros+remaining > sp.CapMicros() {
		if !started {
			rep.Skipped++
			if du.Status == store.StatusPosting {
				return d.Store.Unclaim(ctx, dr.ID, "Waiting: posting it would pass the monthly spend cap.")
			}
			return nil
		}
		if ok, err := d.Store.Claim(ctx, du, now.Add(d.lease())); err != nil || !ok {
			return err
		}
		rep.Failed++
		return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, "Stopped by the monthly spend cap before the remaining parts. Raise the cap, then Retry to post the rest.")
	}
	ok, err := d.Store.Claim(ctx, du, now.Add(d.lease()))
	if err != nil || !ok {
		return err
	}
	// A lapsed lease with an in-flight upload: upload it again (an upload
	// posts nothing). With an in-flight post: a run died mid-call.
	if n, err := d.Store.InterruptedUploads(ctx, dr.ID); err != nil {
		return err
	} else if n > 0 {
		d.logf("draft %d @%s: %d image upload(s) interrupted mid-call; they will be uploaded again", dr.ID, dr.Account, n)
		if ups, err = d.Store.DraftUploads(ctx, dr.ID); err != nil {
			return err
		}
	}
	if parts, err := d.Store.Interrupted(ctx, dr.ID); err != nil {
		return err
	} else if len(parts) > 0 {
		rep.Review++
		d.logf("draft %d @%s: part(s) %v interrupted mid-call; outcome unknown, needs review", dr.ID, dr.Account, parts)
		return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, store.ReviewMessage(parts))
	}
	return d.post(ctx, dr, byKey, ups, mp, sp, rep)
}

// stepErr is why an image step stopped a part before it was posted.
type stepErr struct {
	kind Kind      // RateLimited or NotSent: retried (counts an attempt); Refused: the part fails
	free bool      // X is still processing: wait until at without counting an attempt
	at   time.Time // when to come back, if known
	msg  string
}

// retry decides what happens to a part that did not post (kind RateLimited
// or NotSent): wait and retry, counting an attempt, or give up. finish
// records the part's row with the chosen status.
func (d *Dispatcher) retry(ctx context.Context, dr *store.Draft, part int, kind Kind, msg string, retryAt time.Time, rep *Report, finish func(status string, at time.Time) error) error {
	n := len(dr.Parts)
	now := d.Store.Now()
	attempt := dr.Attempts + 1
	if kind != RateLimited || retryAt.IsZero() || !retryAt.After(now) {
		retryAt = now.Add(Backoff(attempt))
	}
	retryAt = minTime(retryAt, now.Add(MaxRateLimitWait))
	if attempt >= d.maxAttempts() {
		if err := finish(store.LogFailed, time.Time{}); err != nil {
			return err
		}
		rep.Failed++
		return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, fmt.Sprintf("Part %d of %d: gave up after %d attempts: %s", part, n, attempt, msg))
	}
	if err := finish(store.LogRetry, retryAt); err != nil {
		return err
	}
	if kind == RateLimited {
		d.setLimited(dr.Account, retryAt)
	}
	rep.Waiting++
	return d.Store.Defer(ctx, dr.ID, retryAt, fmt.Sprintf("Part %d of %d: %s. Attempt %d of %d; retrying automatically.", part, n, msg, attempt, d.maxAttempts()))
}

func (d *Dispatcher) post(stop context.Context, dr *store.Draft, byKey map[string]*store.LogEntry, ups map[string]*store.MediaUpload, mp MediaPoster, sp *store.Spend, rep *Report) error {
	// Shutdown (stop) is checked only between parts. Once a part's intent
	// is recorded, the call and its outcome are seen through: abandoning
	// them would only turn a clear answer into an unknown one. The
	// poster's own timeout bounds the call.
	ctx := context.WithoutCancel(stop)
	n := len(dr.Parts)
	replyTo := ""
	for i, p := range dr.Parts {
		part := i + 1
		key := Key(dr, part)
		if e := byKey[key]; e != nil {
			switch e.Status {
			case store.LogPosted:
				replyTo = e.PostID // posted parts, and their images, are never touched again
				continue
			case store.LogUnknown:
				rep.Review++
				return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, store.ReviewMessage([]int{part}))
			}
		}
		if stop.Err() != nil {
			return nil // shutting down between parts; the lease lapses and a later run resumes
		}
		if part > 1 && replyTo == "" {
			rep.Failed++
			return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, fmt.Sprintf("Part %d has no earlier part to reply to.", part))
		}
		hasURL := weight.HasURL(p.Text)
		entry := store.LogEntry{DraftID: dr.ID, Account: dr.Account, Part: part, Parts: n, HasURL: hasURL, Preview: preview(p.Text), IdemKey: key, ReplyTo: replyTo, Images: len(p.Media)}
		var mediaIDs []string
		if len(p.Media) > 0 {
			ids, se, err := d.media(ctx, mp, dr, part, p, ups, sp)
			if err != nil {
				return err
			}
			if se != nil {
				return d.notPosted(ctx, dr, part, entry, se, rep)
			}
			mediaIDs = ids
		}
		now := d.Store.Now()
		if err := d.Store.Renew(ctx, dr.ID, now.Add(d.lease())); err != nil {
			return err
		}
		if err := d.Store.BeginPart(ctx, entry); err != nil {
			if errors.Is(err, store.ErrConflict) {
				rep.Review++
				return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, store.ReviewMessage([]int{part}))
			}
			return err
		}
		res, perr := d.Poster.Post(ctx, Request{Account: dr.Account, Text: p.Text, MediaIDs: mediaIDs, ReplyTo: replyTo, IdempotencyKey: key})
		if perr == nil {
			cost := d.Store.PartCost(hasURL)
			if err := d.Store.FinishPart(ctx, key, store.Outcome{Status: store.LogPosted, PostID: res.PostID, URL: res.URL, CostMicros: cost}); err != nil {
				return err
			}
			sp.Micros += cost
			d.logf("draft %d @%s part %d/%d posted: id %s %s", dr.ID, dr.Account, part, n, res.PostID, res.URL)
			replyTo = res.PostID
			continue
		}
		msg := perr.Error()
		var pe *Error
		retryAt := time.Time{}
		kind := KindOf(perr)
		if errors.As(perr, &pe) {
			retryAt = pe.RetryAt
		}
		d.logf("draft %d @%s part %d/%d: %s: %s", dr.ID, dr.Account, part, n, kind, msg)
		switch kind {
		case RateLimited, NotSent:
			return d.retry(ctx, dr, part, kind, msg, retryAt, rep, func(status string, at time.Time) error {
				return d.Store.FinishPart(ctx, key, store.Outcome{Status: status, Error: msg, RetryAt: at})
			})
		case Refused:
			if err := d.Store.FinishPart(ctx, key, store.Outcome{Status: store.LogFailed, Error: msg}); err != nil {
				return err
			}
			rep.Failed++
			return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, fmt.Sprintf("Part %d of %d failed: %s", part, n, msg))
		default:
			if err := d.Store.FinishPart(ctx, key, store.Outcome{Status: store.LogUnknown, Error: msg}); err != nil {
				return err
			}
			rep.Review++
			return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, store.ReviewMessage([]int{part}))
		}
	}
	rep.Posted++
	d.logf("draft %d @%s: posted (%d part(s))", dr.ID, dr.Account, n)
	return d.Store.FinishDraft(ctx, dr.ID, store.StatusPosted, "")
}

// notPosted records a part stopped by an image step: nothing was posted.
func (d *Dispatcher) notPosted(ctx context.Context, dr *store.Draft, part int, e store.LogEntry, se *stepErr, rep *Report) error {
	n := len(dr.Parts)
	d.logf("draft %d @%s part %d/%d not posted: %s", dr.ID, dr.Account, part, n, se.msg)
	record := func(status string, at time.Time) error {
		e.Status, e.Error, e.RetryAt = status, se.msg, at
		return d.Store.NotPosted(ctx, e)
	}
	switch {
	case se.free:
		if err := record(store.LogRetry, se.at); err != nil {
			return err
		}
		rep.Waiting++
		return d.Store.Wait(ctx, dr.ID, se.at, fmt.Sprintf("Part %d of %d: %s.", part, n, se.msg))
	case se.kind == Refused:
		if err := record(store.LogFailed, time.Time{}); err != nil {
			return err
		}
		rep.Failed++
		return d.Store.FinishDraft(ctx, dr.ID, store.StatusFailed, fmt.Sprintf("Part %d of %d failed: %s", part, n, se.msg))
	}
	return d.retry(ctx, dr, part, se.kind, se.msg, se.at, rep, record)
}

// media makes sure every image of a part is on X, processed and has its alt
// text, and returns the X media ids in order. Each upload's intent is
// committed before the call. An upload creates nothing visible on X, so an
// unknown, failed-to-send or expired upload is simply sent again; the post
// itself is guarded by post_log as before.
func (d *Dispatcher) media(ctx context.Context, mp MediaPoster, dr *store.Draft, part int, p store.Part, ups map[string]*store.MediaUpload, sp *store.Spend) ([]string, *stepErr, error) {
	var ids []string
	for j, m := range p.Media {
		slot := j + 1
		key := MediaKey(dr, part, slot)
		u := ups[key]
		now := d.Store.Now()
		if !(u != nil && u.Status == store.UpUploaded && u.XMediaID != "" && u.ExpiresAt.After(now.Add(MediaMargin))) {
			mime, data, err := d.Store.MediaData(ctx, m.ID)
			if err != nil {
				return nil, &stepErr{kind: Refused, msg: fmt.Sprintf("image %d is missing from the database", slot)}, nil
			}
			if err := d.Store.Renew(ctx, dr.ID, now.Add(d.lease())); err != nil {
				return nil, nil, err
			}
			if err := d.Store.BeginUpload(ctx, store.MediaUpload{DraftID: dr.ID, Account: dr.Account, Part: part, Slot: slot, MediaID: m.ID, IdemKey: key}, now, MediaMargin); err != nil {
				return nil, nil, err
			}
			sp.Micros += d.Store.UploadCost(1)
			res, uerr := mp.Upload(ctx, Upload{Account: dr.Account, Mime: mime, Data: data, IdempotencyKey: key})
			now = d.Store.Now()
			if uerr == nil && res.MediaID == "" {
				uerr = &Error{Kind: Ambiguous, Msg: "X accepted the upload but returned no media id"}
			}
			if uerr != nil {
				kind := KindOf(uerr)
				var pe *Error
				var at time.Time
				if errors.As(uerr, &pe) {
					at = pe.RetryAt
				}
				msg := fmt.Sprintf("image %d upload: %s", slot, uerr.Error())
				st := map[Kind]string{RateLimited: store.UpRetry, NotSent: store.UpRetry, Refused: store.UpFailed, Ambiguous: store.UpUnknown}[kind]
				if kind == Ambiguous {
					// Unknown, but harmless: nothing was posted. Upload again.
					msg += "; it will be uploaded again (an upload alone posts nothing)"
					kind = NotSent
				}
				if err := d.Store.FinishUpload(ctx, key, store.UploadOutcome{Status: st, Error: msg}); err != nil {
					return nil, nil, err
				}
				return nil, &stepErr{kind: kind, at: at, msg: msg}, nil
			}
			if res.Failed != "" {
				msg := fmt.Sprintf("X could not process image %d: %s", slot, res.Failed)
				if err := d.Store.FinishUpload(ctx, key, store.UploadOutcome{Status: store.UpFailed, Error: msg}); err != nil {
					return nil, nil, err
				}
				return nil, &stepErr{kind: Refused, msg: msg}, nil
			}
			exp := res.ExpiresAt
			if exp.IsZero() {
				exp = now.Add(DefaultMediaTTL)
			}
			if err := d.Store.FinishUpload(ctx, key, store.UploadOutcome{Status: store.UpUploaded, XMediaID: res.MediaID, ExpiresAt: exp, Processing: res.Processing, CheckAt: checkAt(res.CheckAt, now)}); err != nil {
				return nil, nil, err
			}
			d.logf("draft %d @%s part %d image %d uploaded: media id %s", dr.ID, dr.Account, part, slot, res.MediaID)
			if u, err = d.Store.Upload(ctx, key); err != nil {
				return nil, nil, err
			}
		}
		if u.Processing {
			if now.Sub(u.At) > MaxRateLimitWait {
				msg := fmt.Sprintf("X did not finish processing image %d", slot)
				if err := d.Store.UploadProcessed(ctx, key, false, time.Time{}, msg); err != nil {
					return nil, nil, err
				}
				return nil, &stepErr{kind: Refused, msg: msg}, nil
			}
			if now.Before(u.CheckAt) {
				return nil, &stepErr{free: true, at: u.CheckAt, msg: fmt.Sprintf("image %d is still processing on X", slot)}, nil
			}
			res, serr := mp.MediaStatus(ctx, dr.Account, u.XMediaID)
			now = d.Store.Now()
			switch {
			case serr != nil:
				kind := KindOf(serr)
				msg := fmt.Sprintf("image %d processing check: %s", slot, serr.Error())
				if kind == Refused {
					// X no longer knows the upload: mark it failed so the
					// next run uploads it again.
					if err := d.Store.UploadProcessed(ctx, key, false, time.Time{}, msg); err != nil {
						return nil, nil, err
					}
				}
				if kind != RateLimited {
					kind = NotSent
				}
				var pe *Error
				var at time.Time
				if errors.As(serr, &pe) {
					at = pe.RetryAt
				}
				return nil, &stepErr{kind: kind, at: at, msg: msg}, nil
			case res.Failed != "":
				msg := fmt.Sprintf("X could not process image %d: %s", slot, res.Failed)
				if err := d.Store.UploadProcessed(ctx, key, false, time.Time{}, msg); err != nil {
					return nil, nil, err
				}
				return nil, &stepErr{kind: Refused, msg: msg}, nil
			case res.Processing:
				at := checkAt(res.CheckAt, now)
				if err := d.Store.UploadProcessed(ctx, key, true, at, ""); err != nil {
					return nil, nil, err
				}
				return nil, &stepErr{free: true, at: at, msg: fmt.Sprintf("image %d is still processing on X", slot)}, nil
			}
			if err := d.Store.UploadProcessed(ctx, key, false, time.Time{}, ""); err != nil {
				return nil, nil, err
			}
		}
		if alt := strings.TrimSpace(m.Alt); alt != "" && !u.AltDone {
			call, err := d.Store.BeginAlt(ctx, key) // billed from here (XPOST_COST_MEDIA_METADATA_USD)
			if err != nil {
				return nil, nil, err
			}
			sp.Micros += d.Store.AltCost(1)
			aerr := mp.SetAlt(ctx, dr.Account, u.XMediaID, alt)
			st := store.UpUploaded
			if aerr != nil {
				st = map[Kind]string{RateLimited: store.UpRetry, NotSent: store.UpRetry, Refused: store.UpFailed, Ambiguous: store.UpUnknown}[KindOf(aerr)]
			}
			if err := d.Store.FinishAlt(ctx, call, st); err != nil {
				return nil, nil, err
			}
			if aerr != nil {
				kind := KindOf(aerr)
				msg := fmt.Sprintf("image %d alt text: %s", slot, aerr.Error())
				if err := d.Store.UploadNote(ctx, key, msg); err != nil {
					return nil, nil, err
				}
				var pe *Error
				var at time.Time
				if errors.As(aerr, &pe) {
					at = pe.RetryAt
				}
				if kind == Ambiguous {
					kind = NotSent // setting the same alt text again is harmless
				}
				return nil, &stepErr{kind: kind, at: at, msg: msg}, nil
			}
			if err := d.Store.UploadAltDone(ctx, key); err != nil {
				return nil, nil, err
			}
		}
		ids = append(ids, u.XMediaID)
	}
	return ids, nil, nil
}

// checkAt is when to check processing again: X's hint, kept between 1
// second and MaxProcessingWait from now.
func checkAt(hint, now time.Time) time.Time {
	if hint.IsZero() || hint.Before(now.Add(time.Second)) {
		return now.Add(5 * time.Second)
	}
	return minTime(hint, now.Add(MaxProcessingWait))
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Loop reconciles once, then runs RunDue every interval until ctx is done.
// It returns after the run in progress (at most one X call) finishes.
func (d *Dispatcher) Loop(ctx context.Context, every time.Duration) {
	if err := d.Reconcile(ctx); err != nil {
		d.logf("startup reconcile: %v", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	lastBlock := ""
	for {
		rep, err := d.RunDue(ctx, d.Store.Now())
		if err != nil {
			d.logf("dispatch: %v", err)
		}
		if rep.Block != lastBlock {
			if rep.Block != "" {
				d.logf("posting held: %s", rep.Block)
			} else {
				d.logf("posting allowed")
			}
			lastBlock = rep.Block
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func preview(s string) string {
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
