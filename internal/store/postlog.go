package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ---- Cost rates ----

// Rates are the cost estimates, from config (XPOST_COST_POST_USD,
// XPOST_COST_LINK_POST_USD, XPOST_COST_MEDIA_UPLOAD_USD and
// XPOST_COST_MEDIA_METADATA_USD). There are deliberately no defaults: until
// the two post rates are set the rates are unverified, the spend meter says
// so, and the scheduler does not post because the cap could not be
// enforced. The media rates are separate and fail closed the same way:
// until the upload rate is set, drafts with images wait; until the metadata
// rate is set, drafts with alt text wait. Text-only drafts still post. An
// explicit 0 is a valid rate (free).
type Rates struct {
	PostMicros     int64 // per posted part without a link
	LinkPostMicros int64 // per posted part with a link
	Known          bool
	UploadMicros   int64 // per image upload sent to X (its status checks included)
	UploadKnown    bool
	AltMicros      int64 // per alt text (media metadata) call sent to X
	AltKnown       bool
}

// MaxRateMicros bounds a configured rate ($100 per post) to catch typos.
const MaxRateMicros = 100_000_000

// ParseRates reads the two rate values (US dollars, e.g. "0.015"). Both
// empty gives unverified rates; one without the other is an error.
func ParseRates(post, link string) (Rates, error) {
	post, link = strings.TrimSpace(post), strings.TrimSpace(link)
	if post == "" && link == "" {
		return Rates{}, nil
	}
	if post == "" || link == "" {
		return Rates{}, errors.New("set both XPOST_COST_POST_USD and XPOST_COST_LINK_POST_USD, or neither")
	}
	p, err := parseUSD(post)
	if err != nil {
		return Rates{}, fmt.Errorf("XPOST_COST_POST_USD: %w", err)
	}
	l, err := parseUSD(link)
	if err != nil {
		return Rates{}, fmt.Errorf("XPOST_COST_LINK_POST_USD: %w", err)
	}
	return Rates{PostMicros: p, LinkPostMicros: l, Known: true}, nil
}

// Env names of the media rates.
const (
	EnvCostMediaUpload   = "XPOST_COST_MEDIA_UPLOAD_USD"
	EnvCostMediaMetadata = "XPOST_COST_MEDIA_METADATA_USD"
)

// WithMediaRates adds the per-upload rate (XPOST_COST_MEDIA_UPLOAD_USD) and
// the per-alt-text-call rate (XPOST_COST_MEDIA_METADATA_USD). Each is
// independent; empty leaves that one unverified, so drafts that need it
// wait. "0" is valid and means free.
func (r Rates) WithMediaRates(upload, metadata string) (Rates, error) {
	var err error
	if r.UploadMicros, r.UploadKnown, err = optionalUSD(EnvCostMediaUpload, upload); err != nil {
		return r, err
	}
	if r.AltMicros, r.AltKnown, err = optionalUSD(EnvCostMediaMetadata, metadata); err != nil {
		return r, err
	}
	return r, nil
}

func optionalUSD(name, v string) (int64, bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false, nil
	}
	m, err := parseUSD(v)
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", name, err)
	}
	return m, true, nil
}

func parseUSD(v string) (int64, error) {
	f, err := strconv.ParseFloat(strings.TrimPrefix(v, "$"), 64)
	if err != nil || math.IsNaN(f) || f < 0 {
		return 0, errors.New("must be a dollar amount such as 0.5")
	}
	m := int64(math.Round(f * 1e6))
	if m > MaxRateMicros {
		return 0, errors.New("over $100 per post; check the value")
	}
	return m, nil
}

// PartCost is the estimated cost of one part at the configured rates, or 0
// when they are unverified.
func (s *Store) PartCost(hasURL bool) int64 {
	if !s.Rates.Known {
		return 0
	}
	if hasURL {
		return s.Rates.LinkPostMicros
	}
	return s.Rates.PostMicros
}

// UploadCost is the estimated cost of uploading n images, or 0 when the
// upload rate is unverified.
func (s *Store) UploadCost(n int) int64 {
	if !s.Rates.UploadKnown {
		return 0
	}
	return int64(n) * s.Rates.UploadMicros
}

// AltCost is the estimated cost of n alt text calls, or 0 when the
// metadata rate is unverified.
func (s *Store) AltCost(n int) int64 {
	if !s.Rates.AltKnown {
		return 0
	}
	return int64(n) * s.Rates.AltMicros
}

// ---- Post log ----

// Post log row statuses (see migration 0003).
const (
	LogPosting = "posting" // intent recorded; X call in flight
	LogPosted  = "posted"
	LogFailed  = "failed"  // X refused it; not posted
	LogRetry   = "retry"   // not posted; retried automatically
	LogUnknown = "unknown" // may have posted; the owner reviews it
)

// LogEntry is one post_log row.
type LogEntry struct {
	ID         int64
	DraftID    int64
	Account    string
	Part       int // 1-based
	Parts      int
	At         time.Time
	Status     string
	PostID     string
	URL        string
	Error      string
	HasURL     bool
	CostMicros int64
	Preview    string
	Example    bool
	IdemKey    string
	ReplyTo    string
	Attempts   int
	RetryAt    time.Time
	Images     int
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// RecordPart writes a finished row directly (demo data and tests). A
// posted row is never overwritten.
func (s *Store) RecordPart(ctx context.Context, e LogEntry) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO post_log(draft_id, account, part, parts, at, status, post_id, url, error, has_url, cost_micros, preview, example, idem_key, reply_to, images)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(idem_key) DO UPDATE SET at = excluded.at, status = excluded.status, post_id = excluded.post_id, url = excluded.url, error = excluded.error, cost_micros = excluded.cost_micros
		WHERE post_log.status != 'posted'`,
		e.DraftID, e.Account, e.Part, e.Parts, e.At.Unix(), e.Status, e.PostID, e.URL, e.Error, b2i(e.HasURL), e.CostMicros, e.Preview, b2i(e.Example), e.IdemKey, e.ReplyTo, e.Images)
	return err
}

// LogByKey returns the row for an idempotency key.
func (s *Store) LogByKey(ctx context.Context, key string) (*LogEntry, error) {
	return s.logEntry(ctx, `idem_key = ?`, key)
}

// BeginPart records the intent to post one part and commits it before any
// call to X. It succeeds only for a new key or one whose last attempt
// definitely did not post (failed or retry). A key that is posted, unknown
// or already posting is refused with ErrConflict, so nothing can send the
// same part twice.
func (s *Store) BeginPart(ctx context.Context, e LogEntry) error {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO post_log(draft_id, account, part, parts, at, status, has_url, preview, idem_key, reply_to, attempts, images)
		VALUES (?,?,?,?,?,'posting',?,?,?,?,1,?)
		ON CONFLICT(idem_key) DO UPDATE SET at = excluded.at, status = 'posting', error = '', reply_to = excluded.reply_to, attempts = post_log.attempts + 1, retry_at = 0, images = excluded.images
		WHERE post_log.status IN ('failed','retry')`,
		e.DraftID, e.Account, e.Part, e.Parts, s.now(), b2i(e.HasURL), e.Preview, e.IdemKey, e.ReplyTo, e.Images)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// NotPosted records that a part was not sent because a step before the
// post (an image upload or its alt text) did not finish: status is
// LogRetry (retried automatically at e.RetryAt) or LogFailed. Like
// BeginPart it only touches a new key or one that definitely did not post,
// so it can never hide a posted or unknown part.
func (s *Store) NotPosted(ctx context.Context, e LogEntry) error {
	if e.Status != LogRetry && e.Status != LogFailed {
		return ErrBadInput
	}
	var retry int64
	if !e.RetryAt.IsZero() {
		retry = e.RetryAt.Unix()
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO post_log(draft_id, account, part, parts, at, status, has_url, preview, idem_key, reply_to, attempts, images, error, retry_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,1,?,?,?)
		ON CONFLICT(idem_key) DO UPDATE SET at = excluded.at, status = excluded.status, error = excluded.error, retry_at = excluded.retry_at, attempts = post_log.attempts + 1, images = excluded.images
		WHERE post_log.status IN ('failed','retry')`,
		e.DraftID, e.Account, e.Part, e.Parts, s.now(), e.Status, b2i(e.HasURL), e.Preview, e.IdemKey, e.ReplyTo, e.Images, e.Error, retry)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// Outcome is how one call to X ended.
type Outcome struct {
	Status     string // LogPosted, LogFailed, LogRetry or LogUnknown
	PostID     string
	URL        string
	Error      string
	CostMicros int64
	RetryAt    time.Time
}

// FinishPart records the outcome of a part that BeginPart opened.
func (s *Store) FinishPart(ctx context.Context, key string, o Outcome) error {
	switch o.Status {
	case LogPosted, LogFailed, LogRetry, LogUnknown:
	default:
		return ErrBadInput
	}
	var retry int64
	if !o.RetryAt.IsZero() {
		retry = o.RetryAt.Unix()
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE post_log SET status = ?, post_id = ?, url = ?, error = ?, cost_micros = ?, retry_at = ?, at = ?
		WHERE idem_key = ? AND status = 'posting'`, o.Status, o.PostID, o.URL, o.Error, o.CostMicros, retry, s.now(), key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	return nil
}

// DraftLog returns a draft's rows, by part.
func (s *Store) DraftLog(ctx context.Context, id int64) ([]*LogEntry, error) {
	return s.logRows(ctx, `SELECT `+logCols+` FROM post_log WHERE draft_id = ? AND example = 0 ORDER BY part, id`, id)
}

// PostedParts returns a draft's posted rows, by part, and when it finished
// posting (the newest of them; zero when there are none). Example (demo)
// rows are included, so the demo's posted drafts show their fake links.
func (s *Store) PostedParts(ctx context.Context, id int64) ([]*LogEntry, time.Time, error) {
	rows, err := s.logRows(ctx, `SELECT `+logCols+` FROM post_log WHERE draft_id = ? AND status = 'posted' ORDER BY part, id`, id)
	var last time.Time
	for _, e := range rows {
		if e.At.After(last) {
			last = e.At
		}
	}
	return rows, last, err
}

// ---- Dispatch ----

// Due is a draft the dispatcher may work on now: an approved draft whose
// time has come, or a posting draft whose lease lapsed (a rate-limit wait
// ended, or the run that held it died).
type Due struct {
	ID       int64
	Version  int
	Status   string
	Account  string
	Lease    int64
	Schedule time.Time
}

// DueDrafts lists due X work on unpaused accounts, oldest first (blog
// posts are DueBlogs'). A promo is due only once its blog post is
// confirmed live: published, with its live URL recorded. Replies are
// never due: the owner posts them by hand on x.com.
func (s *Store) DueDrafts(ctx context.Context, now time.Time) ([]Due, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.version, d.status, d.account, d.lease_until, d.scheduled_at FROM drafts d JOIN accounts a ON a.handle = d.account
		WHERE d.kind = 'x' AND NOT EXISTS (SELECT 1 FROM draft_replies rp WHERE rp.draft_id = d.id) AND a.paused = 0 AND ((d.status = 'approved' AND d.scheduled_at <= ?
			AND (d.promotes IS NULL OR EXISTS (SELECT 1 FROM drafts b JOIN blog_posts bp ON bp.draft_id = b.id
				WHERE b.id = d.promotes AND b.status = 'posted' AND COALESCE(bp.published_url, '') != ''))) OR (d.status = 'posting' AND d.lease_until <= ?))
		ORDER BY d.scheduled_at, d.id`, now.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Due
	for rows.Next() {
		var d Due
		var at int64
		if err := rows.Scan(&d.ID, &d.Version, &d.Status, &d.Account, &d.Lease, &at); err != nil {
			return nil, err
		}
		d.Schedule = time.Unix(at, 0)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Claim takes a due draft for one run until lease. The update is
// conditional (status and version for an approved draft, the old lease for
// a posting one), so two runs, or a run racing an owner edit, can never
// both own a draft. ok is false when someone else got there first.
func (s *Store) Claim(ctx context.Context, d Due, lease time.Time) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := s.now()
	var res sql.Result
	if d.Status == StatusApproved {
		res, err = tx.ExecContext(ctx, `UPDATE drafts SET status = 'posting', lease_until = ?, attempts = 0, error = '', updated_at = ? WHERE id = ? AND status = 'approved' AND version = ?`, lease.Unix(), now, d.ID, d.Version)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE drafts SET lease_until = ?, updated_at = ? WHERE id = ? AND status = 'posting' AND lease_until = ?`, lease.Unix(), now, d.ID, d.Lease)
	}
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	if d.Status == StatusApproved {
		if err := event(ctx, tx, d.ID, now, "scheduler", "posting", ""); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// Renew extends a held lease.
func (s *Store) Renew(ctx context.Context, id int64, lease time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE drafts SET lease_until = ? WHERE id = ? AND status = 'posting'`, lease.Unix(), id)
	return err
}

// Defer keeps a posting draft waiting until at (a rate limit or a missing
// connection), counting the attempt. The draft's error says why.
func (s *Store) Defer(ctx context.Context, id int64, at time.Time, why string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE drafts SET lease_until = ?, attempts = attempts + 1, error = ?, updated_at = ? WHERE id = ? AND status = 'posting'`, at.Unix(), why, s.now(), id)
	return err
}

// Wait keeps a posting draft waiting until at without counting an attempt
// (X is still processing an image).
func (s *Store) Wait(ctx context.Context, id int64, at time.Time, why string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE drafts SET lease_until = ?, error = ?, updated_at = ? WHERE id = ? AND status = 'posting'`, at.Unix(), why, s.now(), id)
	return err
}

// Unclaim returns a posting draft that has posted nothing back to approved
// (the cap or spacing stopped it before its first part). Version is kept,
// so the idempotency keys stay the same.
func (s *Store) Unclaim(ctx context.Context, id int64, why string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE drafts SET status = 'approved', lease_until = 0, error = ?, updated_at = ? WHERE id = ? AND status = 'posting'
		AND NOT EXISTS (SELECT 1 FROM post_log WHERE draft_id = drafts.id AND example = 0 AND status IN ('posted','posting','unknown'))`, why, s.now(), id)
	return err
}

// FinishDraft sets a posting draft to posted or failed and drops its lease.
func (s *Store) FinishDraft(ctx context.Context, id int64, status, errText string) error {
	if status != StatusPosted && status != StatusFailed {
		return ErrBadInput
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	res, err := tx.ExecContext(ctx, `UPDATE drafts SET status = ?, error = ?, lease_until = 0, updated_at = ? WHERE id = ? AND status = 'posting' AND kind = 'x'`, status, errText, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if err := event(ctx, tx, id, now, "scheduler", status, errText); err != nil {
		return err
	}
	return tx.Commit()
}

// Interrupted turns a draft's in-flight rows (left 'posting' by a run that
// died mid-call) into 'unknown'. Whether X created those posts cannot be
// known from here, so they are never retried automatically: the draft
// fails with a review message. It returns the parts it changed.
func (s *Store) Interrupted(ctx context.Context, id int64) ([]int, error) {
	rows, err := s.logRows(ctx, `SELECT `+logCols+` FROM post_log WHERE draft_id = ? AND status = 'posting' ORDER BY part`, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var parts []int
	for _, e := range rows {
		if _, err := s.DB.ExecContext(ctx, `UPDATE post_log SET status = 'unknown', error = ? WHERE id = ? AND status = 'posting'`,
			"Interrupted during the call to X: it may or may not have posted.", e.ID); err != nil {
			return nil, err
		}
		parts = append(parts, e.Part)
	}
	return parts, nil
}

// ---- Startup reconcile ----

// Reconcile runs when the dispatcher starts. Drafts left 'posting' whose
// lease has lapsed are checked: in-flight rows become 'unknown' and the
// draft fails for review; a draft with no in-flight row (it stopped
// between parts) is simply resumable, because every part is either posted
// with an id or was never sent. A lease that has not lapsed may belong to
// a process still shutting down, so it is left for a later run.
func (s *Store) Reconcile(ctx context.Context, now time.Time) (review, resumable int, err error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM drafts WHERE kind = 'x' AND status = 'posting' AND lease_until <= ?`, now.Unix())
	if err != nil {
		return 0, 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		// An upload cut off mid-call is only re-uploaded: no review.
		if _, err := s.InterruptedUploads(ctx, id); err != nil {
			return review, resumable, err
		}
		parts, err := s.Interrupted(ctx, id)
		if err != nil {
			return review, resumable, err
		}
		if len(parts) == 0 {
			resumable++
			continue
		}
		if err := s.FinishDraft(ctx, id, StatusFailed, ReviewMessage(parts)); err != nil && !errors.Is(err, ErrConflict) {
			return review, resumable, err
		}
		review++
	}
	return review, resumable, nil
}

// ReviewMessage is the draft error for parts with an unknown outcome.
func ReviewMessage(parts []int) string {
	var p []string
	for _, n := range parts {
		p = append(p, strconv.Itoa(n))
	}
	return "Needs review: part " + strings.Join(p, ", ") + " may or may not have posted. Check X, then mark it posted or not posted on the Log page."
}

// ---- Owner actions on failed drafts ----

// RetryDraft sends a failed draft back to the dispatcher. Parts already
// posted are skipped (same idempotency keys), so only the remaining parts
// post, replying to the last posted id. Refused while any part's outcome
// is unknown: the owner resolves those first.
func (s *Store) RetryDraft(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusFailed {
		return ErrConflict
	}
	var unknown int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM post_log WHERE draft_id = ? AND example = 0 AND status IN ('unknown','posting')`, id).Scan(&unknown)
	if unknown > 0 {
		return &ConflictError{Msg: "A part may already be on X. Mark it posted or not posted first."}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'posting', lease_until = 0, attempts = 0, error = '', updated_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "retry", "remaining parts"); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveUnknown records the owner's answer for a part whose outcome was
// unknown: posted (with the post id and URL they found on X) or not posted
// (so a retry may send it).
func (s *Store) ResolveUnknown(ctx context.Context, u *User, id int64, version, part int, posted bool, postID, url string) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusFailed {
		return ErrConflict
	}
	now := s.now()
	var res sql.Result
	if posted {
		if postID == "" || len(postID) > 30 || strings.Trim(postID, "0123456789") != "" {
			return badInput("Paste the post's link or its numeric id.")
		}
		var hasURL int
		tx.QueryRowContext(ctx, `SELECT has_url FROM post_log WHERE draft_id = ? AND part = ? AND status = 'unknown'`, id, part).Scan(&hasURL)
		res, err = tx.ExecContext(ctx, `UPDATE post_log SET status = 'posted', post_id = ?, url = ?, error = '', cost_micros = ? WHERE draft_id = ? AND part = ? AND status = 'unknown'`,
			postID, url, s.PartCost(hasURL == 1), id, part)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE post_log SET status = 'failed', error = 'You confirmed it did not post.' WHERE draft_id = ? AND part = ? AND status = 'unknown'`, id, part)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	detail := fmt.Sprintf("part %d not posted", part)
	if posted {
		detail = fmt.Sprintf("part %d posted as %s", part, postID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET updated_at = ? WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "resolved", detail); err != nil {
		return err
	}
	return tx.Commit()
}

// LastStart is when the newest other item on account began posting (its
// first part's intent), for spacing at post time.
func (s *Store) LastStart(ctx context.Context, account string, exclude int64) (time.Time, bool) {
	var at sql.NullInt64
	s.DB.QueryRowContext(ctx, `SELECT MAX(at) FROM post_log WHERE account = ? AND draft_id != ? AND part = 1 AND example = 0 AND status IN ('posted','posting','unknown')`, account, exclude).Scan(&at)
	if !at.Valid {
		return time.Time{}, false
	}
	return time.Unix(at.Int64, 0), true
}

// ---- Reading ----

const logCols = `id, draft_id, account, part, parts, at, status, post_id, url, error, has_url, cost_micros, preview, example, idem_key, reply_to, attempts, retry_at, images`

func scanLog(r scanner) (*LogEntry, error) {
	var e LogEntry
	var at, retry int64
	var hu, ex int
	if err := r.Scan(&e.ID, &e.DraftID, &e.Account, &e.Part, &e.Parts, &at, &e.Status, &e.PostID, &e.URL, &e.Error, &hu, &e.CostMicros, &e.Preview, &ex, &e.IdemKey, &e.ReplyTo, &e.Attempts, &retry, &e.Images); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.At, e.HasURL, e.Example = time.Unix(at, 0), hu == 1, ex == 1
	if retry > 0 {
		e.RetryAt = time.Unix(retry, 0)
	}
	return &e, nil
}

func (s *Store) logEntry(ctx context.Context, where string, args ...any) (*LogEntry, error) {
	return scanLog(s.DB.QueryRowContext(ctx, `SELECT `+logCols+` FROM post_log WHERE `+where, args...))
}

func (s *Store) logRows(ctx context.Context, q string, args ...any) ([]*LogEntry, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*LogEntry
	for rows.Next() {
		e, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PostLog lists log rows at or after since, newest first.
func (s *Store) PostLog(ctx context.Context, since time.Time, status string, limit int) ([]*LogEntry, error) {
	q := `SELECT ` + logCols + ` FROM post_log WHERE at >= ?`
	args := []any{since.Unix()}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	return s.logRows(ctx, q+` ORDER BY at DESC, id DESC LIMIT ?`, append(args, limit)...)
}

// ---- Spend ----

// Spend is the month's estimated spend from calls the app actually made,
// at the configured rates. Parts with an unknown outcome are counted as
// billed, so the cap errs on the side of stopping early.
type Spend struct {
	Month          time.Time
	Posts          int // posted (or unknown) parts without a link
	LinkPosts      int // posted (or unknown) parts with a link
	UnknownParts   int // included above
	FailedParts    int // assumed not billed (unverified)
	Images         int // image uploads sent (uploaded, unknown or in flight)
	UnknownUploads int // included above
	Alts           int // alt text calls sent (done, unknown or in flight)
	UnknownAlts    int // included above
	Micros         int64
	PrevMicros     int64
	CapCents       int64
	CapSet         bool
	Rates          Rates
	Example        bool // includes demo rows
}

func (r Rates) cost(posts, links int, m mediaCount) int64 {
	return int64(posts)*r.PostMicros + int64(links)*r.LinkPostMicros + int64(m.uploads)*r.UploadMicros + int64(m.alts)*r.AltMicros
}

type mediaCount struct{ uploads, unknownUploads, alts, unknownAlts, example int }

// monthMedia counts billed media calls (in flight, accepted or unknown) by
// kind.
func (s *Store) monthMedia(ctx context.Context, start, end time.Time) (m mediaCount, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(kind = 'upload' AND status IN ('uploading','uploaded','unknown')), 0), COALESCE(SUM(kind = 'upload' AND status IN ('uploading','unknown')), 0),
		COALESCE(SUM(kind = 'metadata' AND status IN ('uploading','uploaded','unknown')), 0), COALESCE(SUM(kind = 'metadata' AND status IN ('uploading','unknown')), 0),
		COALESCE(MAX(example), 0)
		FROM media_calls WHERE at >= ? AND at < ?`, start.Unix(), end.Unix()).Scan(&m.uploads, &m.unknownUploads, &m.alts, &m.unknownAlts, &m.example)
	return
}

// MonthSpend sums the parts in now's month in loc.
func (s *Store) MonthSpend(ctx context.Context, loc *time.Location, now time.Time) (Spend, error) {
	n := now.In(loc)
	start := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 1, 0)
	prev := start.AddDate(0, -1, 0)
	sp := Spend{Month: start, Rates: s.Rates}
	sp.CapCents, sp.CapSet = s.CapCents(ctx)
	var ex int
	err := s.DB.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status IN ('posted','unknown','posting') AND has_url = 0), 0), COALESCE(SUM(status IN ('posted','unknown','posting') AND has_url = 1), 0),
		COALESCE(SUM(status IN ('unknown','posting')), 0), COALESCE(SUM(status = 'failed'), 0), COALESCE(MAX(example), 0)
		FROM post_log WHERE at >= ? AND at < ?`, start.Unix(), end.Unix()).Scan(&sp.Posts, &sp.LinkPosts, &sp.UnknownParts, &sp.FailedParts, &ex)
	if err != nil {
		return sp, err
	}
	mc, err := s.monthMedia(ctx, start, end)
	if err != nil {
		return sp, err
	}
	sp.Images, sp.UnknownUploads, sp.Alts, sp.UnknownAlts = mc.uploads, mc.unknownUploads, mc.alts, mc.unknownAlts
	sp.Example = ex == 1 || mc.example == 1
	sp.Micros = s.Rates.cost(sp.Posts, sp.LinkPosts, mc)
	var pp, pl int
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(has_url = 0), 0), COALESCE(SUM(has_url = 1), 0) FROM post_log WHERE status IN ('posted','unknown') AND at >= ? AND at < ?`, prev.Unix(), start.Unix()).Scan(&pp, &pl)
	if err != nil {
		return sp, err
	}
	pm, err := s.monthMedia(ctx, prev, start)
	sp.PrevMicros = s.Rates.cost(pp, pl, pm)
	return sp, err
}

// CapMicros is the cap in micro-dollars.
func (sp Spend) CapMicros() int64 { return sp.CapCents * 10_000 }

// PostingBlock says why the scheduler may not post right now, or "" when
// it may. It fails closed: no cap, or no verified rates (so the cap can't
// be enforced), means no posting.
func (sp Spend) PostingBlock() string {
	switch {
	case !sp.CapSet:
		return "No monthly spend cap is set. Set one in Settings; nothing posts until then."
	case !sp.Rates.Known:
		return "Cost rates are not configured (XPOST_COST_POST_USD and XPOST_COST_LINK_POST_USD), so the cap can't be enforced. Nothing posts until they are set."
	case sp.Micros >= sp.CapMicros():
		return "Monthly spend cap reached. Posting is paused until next month or a higher cap."
	}
	return ""
}

// UploadBlock says why drafts with images may not post even when
// PostingBlock allows text: the upload cost is unknown, so the cap could
// not count it. "" when images may post.
func (sp Spend) UploadBlock() string {
	if !sp.Rates.UploadKnown {
		return "The image upload cost rate (XPOST_COST_MEDIA_UPLOAD_USD) is not configured, so drafts with images wait. Text-only drafts are not affected."
	}
	return ""
}

// AltBlock says why drafts whose images have alt text may not post: the
// alt text (media metadata) call cost is unknown. "" when they may.
func (sp Spend) AltBlock() string {
	if !sp.Rates.AltKnown {
		return "The alt text cost rate (XPOST_COST_MEDIA_METADATA_USD) is not configured, so drafts with alt text wait. Text-only drafts are not affected."
	}
	return ""
}

// MediaBlocks lists UploadBlock and AltBlock, whichever apply.
func (sp Spend) MediaBlocks() []string {
	var out []string
	for _, b := range []string{sp.UploadBlock(), sp.AltBlock()} {
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}
