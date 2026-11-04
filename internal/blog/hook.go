package blog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

// Webhook-to-agent publishing (slice 3). The dispatcher freezes the post
// into a publish (the canonical rendered payload and its hash), wakes the
// blog's agent with a thin signed webhook through a Waker, and then checks
// the live page on a fixed schedule. The agent pulls the payload over the
// agent API and reports back; only the live check marks the post Published.

// Wake is the whole body of a wake: it names a publish and carries no
// content. The agent pulls the payload with its own token.
type Wake struct {
	Event      string    // "blog.publish" or "blog.test"
	PublishID  int64     // 0 for blog.test
	Version    int       // the draft version of this attempt; 0 for blog.test
	DeliveryID string    // unique per send
	ExpiresAt  time.Time // when xpost stops waiting for this attempt
}

// Wake events.
const (
	EventPublish = "blog.publish"
	EventTest    = "blog.test"
)

// Waker reaches a webhook blog's agent and its live site.
type Waker interface {
	// Test sends a signed blog.test wake; any 2xx passes.
	Test(ctx context.Context, b *store.Blog) (string, error)
	// Wake sends a signed blog.publish wake; any 2xx means delivered.
	Wake(ctx context.Context, b *store.Blog, w Wake) error
	// Check GETs liveURL exactly (no token, no redirects) and reports
	// whether it is a 200 HTML page carrying marker. note says why not.
	Check(ctx context.Context, b *store.Blog, liveURL, marker string) (ok bool, note string, err error)
}

// HookCover describes the cover; Path (where to fetch it) is added when
// the payload is served and is not part of the hash.
type HookCover struct {
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
	SHA256      string `json:"sha256"`
	Alt         string `json:"alt"`
	Path        string `json:"path,omitempty"`
}

// HookPayload is the frozen, authoritative publish payload the agent
// pulls. HTML is blogtext.Render of Markdown, the same sanitized HTML the
// preview shows: the agent only wraps it in the site template.
type HookPayload struct {
	Site        string     `json:"site"`
	Slug        string     `json:"slug"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Tags        []string   `json:"tags"`
	Markdown    string     `json:"markdown"`
	HTML        string     `json:"html"`
	DateUTC     string     `json:"date_utc"`
	DateLocal   string     `json:"date_local"`
	Zone        string     `json:"zone"`
	PublishedAt string     `json:"published_at"`
	LiveURL     string     `json:"live_url"`
	Cover       *HookCover `json:"cover"`
	ContentSHA  string     `json:"content_sha256"`
	Canonical   string     `json:"canonical"`
	Marker      string     `json:"marker"`
}

// CanonicalVersion starts the canonical form.
const CanonicalVersion = "xpost-blog-publish-v1"

func hexsum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Canonical is the exact text content_sha256 is the SHA-256 of: one field
// per line, each ending in "\n", free text hashed (see WEBHOOK-PUBLISH.md).
func Canonical(p HookPayload) string {
	return canonical(p, true)
}

func canonical(p HookPayload, dates bool) string {
	cover, ctype, alt := "none", "none", "none"
	if p.Cover != nil {
		cover, ctype, alt = p.Cover.SHA256, p.Cover.ContentType, hexsum(p.Cover.Alt)
	}
	du, dl := p.DateUTC, p.DateLocal
	if !dates {
		du = "" // the content key keeps the blog-local day only
	}
	var b strings.Builder
	for _, l := range [][2]string{
		{"slug", p.Slug}, {"live_url", p.LiveURL}, {"date_utc", du}, {"date_local", dl}, {"zone", p.Zone},
		{"title", hexsum(p.Title)}, {"description", hexsum(p.Description)}, {"tags", hexsum(strings.Join(p.Tags, "\n"))},
		{"markdown", hexsum(p.Markdown)}, {"html", hexsum(p.HTML)}, {"cover", cover}, {"cover_type", ctype}, {"cover_alt", alt},
	} {
		b.WriteString(l[0] + "=" + l[1] + "\n")
	}
	return CanonicalVersion + "\n" + b.String()
}

// Marker is the meta tag the live page must carry for hash.
func Marker(hash string) string {
	return `<meta name="xpost:content" content="sha256:` + hash + `">`
}

// HookCoverTypes are the cover types a webhook blog accepts.
var HookCoverTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// MaxHookCover bounds the cover a webhook blog accepts.
const MaxHookCover = 2 << 20

// ErrHookCover is a cover a webhook blog can't take.
var ErrHookCover = errors.New("The cover must be a PNG, JPEG, WebP or GIF of at most 2 MiB. Replace it in Edit, then retry.")

// BuildHookPayload freezes p for b at the publish time at, with dates in
// UTC and in the blog's zone (loc). It returns the payload, its JSON and
// the content key: the canonical form without date_utc. The key keeps
// date_local, so a Retry on the same day in the blog's zone reuses the
// publish (same key, hash and dates) and a Retry on a later day starts a
// new one with the new dates, as gander dates a post by when it goes live.
func BuildHookPayload(b *store.Blog, p Post, at time.Time, loc *time.Location) (HookPayload, string, string, error) {
	tags := p.Tags
	if tags == nil {
		tags = []string{}
	}
	pl := HookPayload{Site: b.Name, Slug: p.Slug, Title: p.Title, Description: p.Description, Tags: tags, Markdown: p.Markdown,
		HTML: string(blogtext.Render(p.Markdown)), DateUTC: at.UTC().Format("2006-01-02"), DateLocal: at.In(loc).Format("2006-01-02"), Zone: loc.String(),
		PublishedAt: at.UTC().Format(time.RFC3339), LiveURL: b.LiveURL(p.Slug)}
	if p.HasCover {
		if !HookCoverTypes[p.CoverMIME] || len(p.Cover) == 0 || len(p.Cover) > MaxHookCover {
			return pl, "", "", ErrHookCover
		}
		h := sha256.Sum256(p.Cover)
		pl.Cover = &HookCover{ContentType: p.CoverMIME, Bytes: len(p.Cover), SHA256: hex.EncodeToString(h[:]), Alt: p.CoverAlt}
	}
	pl.Canonical = Canonical(pl)
	pl.ContentSHA = hexsum(pl.Canonical)
	pl.Marker = Marker(pl.ContentSHA)
	js, err := json.Marshal(pl)
	if err != nil {
		return pl, "", "", err
	}
	return pl, string(js), hexsum(canonical(pl, false)), nil
}

func (d *Dispatcher) waker(b *store.Blog) Waker {
	if h, ok := d.Publisher.(interface{ HookFor(*store.Blog) Waker }); ok {
		return h.HookFor(b)
	}
	return nil
}

func (d *Dispatcher) newID() string {
	if d.NewID != nil {
		return d.NewID()
	}
	var b [8]byte
	rand.Read(b[:])
	return "d_" + hex.EncodeToString(b[:])
}

// hookStart freezes the post into a publish and wakes the agent. The post
// stays Publishing; the next pass is its first live check.
func (d *Dispatcher) hookStart(ctx context.Context, now time.Time, dr *store.Draft, b *store.Blog, p Post, w Waker, fail func(string) (bool, error)) (bool, error) {
	if b.PublishTokenID == 0 {
		return fail("Pick a publishing token for " + b.Name + " in Settings > Blogs, then retry.")
	}
	def := time.UTC
	if d.Loc != nil {
		def = d.Loc
	}
	loc := b.Location(d.Store.Zone(ctx, def))
	pl, js, key, err := BuildHookPayload(b, p, now, loc)
	if err != nil {
		return fail(short(err))
	}
	var cover int64
	if dr.Post.Cover != nil {
		cover = dr.Post.Cover.ID
	}
	pub, err := d.Store.StartBlogPublish(ctx, store.PublishStart{DraftID: dr.ID, BlogID: b.ID, Version: dr.Version, ContentKey: key, ContentSHA: pl.ContentSHA,
		Payload: js, CoverMediaID: cover, LiveURL: pl.LiveURL, DeliveryID: d.newID()})
	if err != nil {
		return false, err
	}
	if err := d.Store.MarkStaged(ctx, dr.ID, store.HookRef(dr.Post.Slug, dr.ID)); err != nil {
		return false, err
	}
	err = w.Wake(ctx, b, Wake{Event: EventPublish, PublishID: pub.ID, Version: pub.Version, DeliveryID: pub.DeliveryID, ExpiresAt: pub.ExpiresAt})
	if err != nil {
		d.logf("blog %d publish %d attempt %d: wake failed", b.ID, pub.ID, pub.Attempt)
		d.Store.FailBlogPublish(ctx, pub.ID)
		return fail("Publish webhook failed: " + short(err))
	}
	if err := d.Store.MarkBlogPublishSent(ctx, pub.ID); err != nil {
		return false, err
	}
	d.logf("blog %d publish %d attempt %d: wake delivered %s", b.ID, pub.ID, pub.Attempt, pub.DeliveryID)
	if pub, err = d.Store.BlogPublishByID(ctx, pub.ID); err != nil {
		return false, err
	}
	return false, d.Store.Wait(ctx, dr.ID, pub.NextCheck(now), "")
}

// hookCheck runs one live check for a publish in flight: Published when
// the page carries the marker, else wait for the next check or fail when
// the schedule is over.
func (d *Dispatcher) hookCheck(ctx context.Context, now time.Time, dr *store.Draft, b *store.Blog, pub *store.BlogPublish, w Waker, fail func(string) (bool, error)) (bool, error) {
	if pub.State == store.PubWaking {
		// xpost stopped while sending the wake.
		d.Store.FailBlogPublish(ctx, pub.ID)
		return fail("The publish webhook may not have reached the agent. Retry publish to send it again.")
	}
	ok, note, err := w.Check(ctx, b, pub.LiveURL, Marker(pub.ContentSHA))
	d.Store.NoteBlogPublishCheck(ctx, pub.ID)
	if err == nil && ok {
		if err := d.Store.MarkBlogPublishLive(ctx, pub.ID); err != nil {
			return false, err
		}
		d.logf("blog %d publish %d: live page confirmed", b.ID, pub.ID)
		return true, d.Store.FinishBlog(ctx, dr.ID, true, pub.LiveURL, store.HookRef(dr.Post.Slug, dr.ID), "")
	}
	if err != nil {
		note = short(err)
	}
	// A report may have arrived during the check: read the schedule fresh.
	if p2, err := d.Store.BlogPublishByID(ctx, pub.ID); err == nil {
		pub = p2
	}
	if !pub.InFlight() {
		return false, nil // a failed report already ended it
	}
	if next := pub.NextCheck(now); !next.IsZero() {
		return false, d.Store.Wait(ctx, dr.ID, next, "")
	}
	d.Store.FailBlogPublish(ctx, pub.ID)
	why := "No answer from the publishing agent within 60 minutes. Retry publish to send it again."
	switch pub.State {
	case store.PubMerged:
		why = "Merged, but the live page did not show this version yet. Retry publish to check again."
	case store.PubOpened:
		why = "The agent opened a pull request but did not report a merge within 60 minutes. Retry publish to send it again."
	}
	if note != "" {
		why += " Last check: " + note
	}
	return fail(why)
}
