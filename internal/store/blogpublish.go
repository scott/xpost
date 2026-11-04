package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Publishes through a blog's agent (webhook blogs, slice 3). xpost freezes
// the post into a publish, wakes the agent with a thin signed webhook, and
// the agent pulls the payload with its blogs:publish token, publishes, and
// reports back. Only xpost's own live check marks the post Published.

// Publish states.
const (
	PubWaking = "waking"
	PubSent   = "sent"
	PubOpened = "opened"
	PubMerged = "merged"
	PubLive   = "live"
	PubFailed = "failed"
)

// HookWindow is how long xpost waits for an agent: a publish attempt
// expires this long after it starts, like the 60-minute Rewrite window.
const HookWindow = 60 * time.Minute

// Live checks run at these offsets: after a merged report, or after the
// send when no merge was reported. Nothing else polls.
var (
	MergedChecks = []time.Duration{1 * time.Minute, 3 * time.Minute, 10 * time.Minute, 30 * time.Minute}
	SentChecks   = []time.Duration{10 * time.Minute, 30 * time.Minute, 60 * time.Minute}
)

// Report errors.
var (
	ErrNotInFlight    = errors.New("not_in_flight")
	ErrPublishExpired = errors.New("expired")
	ErrReportMismatch = errors.New("mismatch")
)

// BlogPublish is one publish of a blog post through its agent.
type BlogPublish struct {
	ID           int64
	DraftID      int64
	BlogID       int64
	Version      int
	Attempt      int
	ContentKey   string // the content without its dates: a Retry of unchanged content reuses the row
	ContentSHA   string
	Payload      string // the frozen JSON payload (no cover bytes)
	CoverMediaID int64
	LiveURL      string
	State        string
	DeliveryID   string
	StartedAt    time.Time
	SentAt       *time.Time
	ExpiresAt    time.Time
	MergedAt     *time.Time
	Checks       int
	PR           int
	HeadSHA      string
	MergeSHA     string
	Code         string
}

// InFlight reports whether the attempt is still running.
func (p *BlogPublish) InFlight() bool {
	switch p.State {
	case PubWaking, PubSent, PubOpened, PubMerged:
		return true
	}
	return false
}

// Key is the idempotency key the agent dedups on. It stays the same for
// every attempt of one publish.
func (p *BlogPublish) Key() string { return fmt.Sprintf("xpost-blog-%d-%d", p.DraftID, p.ID) }

// NextCheck is the next live check after now, or zero when the schedule
// is over.
func (p *BlogPublish) NextCheck(now time.Time) time.Time {
	base, offs := p.StartedAt, SentChecks
	if p.SentAt != nil {
		base = *p.SentAt
	}
	if p.MergedAt != nil {
		base, offs = *p.MergedAt, MergedChecks
	}
	for _, o := range offs {
		if t := base.Add(o); t.After(now) {
			return t
		}
	}
	return time.Time{}
}

const pubCols = `id, draft_id, blog_id, version, attempt, content_key, content_sha256, payload, COALESCE(cover_media_id, 0), live_url, state, delivery_id,
	started_at, sent_at, expires_at, merged_at, checks, pr, head_sha, merge_sha, code`

func scanPublish(r scanner) (*BlogPublish, error) {
	var p BlogPublish
	var started, expires int64
	var sent, merged sql.NullInt64
	if err := r.Scan(&p.ID, &p.DraftID, &p.BlogID, &p.Version, &p.Attempt, &p.ContentKey, &p.ContentSHA, &p.Payload, &p.CoverMediaID, &p.LiveURL, &p.State, &p.DeliveryID,
		&started, &sent, &expires, &merged, &p.Checks, &p.PR, &p.HeadSHA, &p.MergeSHA, &p.Code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	p.StartedAt, p.ExpiresAt = time.Unix(started, 0), time.Unix(expires, 0)
	p.SentAt, p.MergedAt = tptr(sent), tptr(merged)
	return &p, nil
}

// BlogPublishByID loads one publish.
func (s *Store) BlogPublishByID(ctx context.Context, id int64) (*BlogPublish, error) {
	return scanPublish(s.DB.QueryRowContext(ctx, `SELECT `+pubCols+` FROM blog_publishes WHERE id = ?`, id))
}

// LatestBlogPublish is the draft's newest publish, or ErrNotFound.
func (s *Store) LatestBlogPublish(ctx context.Context, draftID int64) (*BlogPublish, error) {
	return scanPublish(s.DB.QueryRowContext(ctx, `SELECT `+pubCols+` FROM blog_publishes WHERE draft_id = ? ORDER BY id DESC LIMIT 1`, draftID))
}

// PublishStart is a new publish attempt.
type PublishStart struct {
	DraftID, BlogID int64
	Version         int
	ContentKey      string
	ContentSHA      string
	Payload         string
	CoverMediaID    int64
	LiveURL         string
	DeliveryID      string
}

// StartBlogPublish begins an attempt in state waking. When the draft's
// last publish failed with the same content key (same content and the
// same day in the blog's zone), that publish is reused: the attempt goes up
// and the key, payload, hash and dates stay. Otherwise (an edit, or a
// later local day) a new publish starts with a new key. A publish still in flight is ErrConflict.
func (s *Store) StartBlogPublish(ctx context.Context, in PublishStart) (*BlogPublish, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.now()
	expires := now + int64(HookWindow/time.Second)
	last, err := scanPublish(tx.QueryRowContext(ctx, `SELECT `+pubCols+` FROM blog_publishes WHERE draft_id = ? ORDER BY id DESC LIMIT 1`, in.DraftID))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	var id int64
	switch {
	case last != nil && last.InFlight():
		return nil, ErrConflict
	case last != nil && last.State == PubFailed && last.ContentKey == in.ContentKey && last.BlogID == in.BlogID:
		id = last.ID
		if _, err := tx.ExecContext(ctx, `UPDATE blog_publishes SET attempt = attempt + 1, version = ?, state = 'waking', delivery_id = ?, started_at = ?, sent_at = NULL, expires_at = ?,
			merged_at = NULL, checks = 0, code = '', merge_sha = '', updated_at = ? WHERE id = ?`, in.Version, in.DeliveryID, now, expires, now, id); err != nil {
			return nil, err
		}
	default:
		res, err := tx.ExecContext(ctx, `INSERT INTO blog_publishes(draft_id, blog_id, version, content_key, content_sha256, payload, cover_media_id, live_url, state, delivery_id,
			started_at, expires_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,'waking',?,?,?,?,?)`,
			in.DraftID, in.BlogID, in.Version, in.ContentKey, in.ContentSHA, in.Payload, nullID(in.CoverMediaID), in.LiveURL, in.DeliveryID, now, expires, now, now)
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
	}
	p, err := scanPublish(tx.QueryRowContext(ctx, `SELECT `+pubCols+` FROM blog_publishes WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

// MarkBlogPublishSent records the agent's 2xx. A report that already
// arrived (opened, merged) keeps its state.
func (s *Store) MarkBlogPublishSent(ctx context.Context, id int64) error {
	now := s.now()
	_, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = CASE WHEN state = 'waking' THEN 'sent' ELSE state END, sent_at = COALESCE(sent_at, ?), updated_at = ?
		WHERE id = ? AND state IN ('waking','sent','opened','merged')`, now, now, id)
	return err
}

// FailBlogPublish ends an attempt that is still in flight.
func (s *Store) FailBlogPublish(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = 'failed', updated_at = ? WHERE id = ? AND state IN ('waking','sent','opened','merged')`, s.now(), id)
	return err
}

// NoteBlogPublishCheck counts one live check.
func (s *Store) NoteBlogPublishCheck(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET checks = checks + 1, updated_at = ? WHERE id = ?`, s.now(), id)
	return err
}

// MarkBlogPublishLive records that xpost found the marker on the page.
func (s *Store) MarkBlogPublishLive(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = 'live', updated_at = ? WHERE id = ? AND state IN ('waking','sent','opened','merged')`, s.now(), id)
	return err
}

// OwnBlogPublish loads a publish for an agent token. "Own" is strict: the
// publish's blog is a webhook blog that is not removed and whose Publishing
// token is exactly this token. Anything else is ErrNotFound, the same as
// an id that does not exist.
func (s *Store) OwnBlogPublish(ctx context.Context, tok *AgentToken, id int64) (*BlogPublish, *Blog, error) {
	if tok == nil || tok.RevokedAt != nil || !tok.Has(ScopeBlogsPublish) {
		return nil, nil, ErrNotFound
	}
	p, err := s.BlogPublishByID(ctx, id)
	if err != nil {
		return nil, nil, ErrNotFound
	}
	b, err := s.BlogByID(ctx, p.BlogID)
	if err != nil || b.Method != MethodWebhook || b.PublishTokenID != tok.ID {
		return nil, nil, ErrNotFound
	}
	return p, b, nil
}

// Serveable reports whether the agent may still pull the publish.
func (p *BlogPublish) Serveable(now time.Time) error {
	if !p.InFlight() {
		return ErrNotInFlight
	}
	if now.After(p.ExpiresAt) {
		return ErrPublishExpired
	}
	return nil
}

// PublishReport is what the agent reports.
type PublishReport struct {
	Status     string `json:"status"`
	Attempt    int    `json:"attempt"`
	Key        string `json:"idempotency_key"`
	ContentSHA string `json:"content_sha256"`
	PR         int    `json:"pr"`
	HeadSHA    string `json:"head_sha"`
	MergeSHA   string `json:"merge_sha"`
	Code       string `json:"code"`
}

// hookFailText is the card text for each failure code an agent may
// report. Nothing the agent writes is shown.
var hookFailText = map[string]string{
	"content_mismatch":    "The agent found the post did not match what was approved, so it published nothing.",
	"stale_request":       "The agent ignored an out-of-date request. Retry publish to send it again.",
	"expired":             "The agent got the request after its 60-minute window. Retry publish to send it again.",
	"slug_exists":         "",
	"url_mismatch":        "The site's layout does not match the expected live address.",
	"repo_check_failed":   "The agent could not check the site's repository.",
	"branch_moved":        "The site's main branch changed while publishing.",
	"merge_conflict":      "The agent's pull request has a merge conflict.",
	"merge_failed":        "The agent could not merge its pull request.",
	"unsupported_version": "The agent does not support this publish format.",
	"cover_rejected":      "The agent refused the cover image.",
	"render_failed":       "The agent could not build the page.",
}

// HookFailText is the owner-facing reason for an agent's failure code.
func HookFailText(code string, b *Blog) string {
	if code == "slug_exists" {
		return BlogSlugTakenPrefix + b.Name + ". The agent never touches a post xpost did not publish."
	}
	return hookFailText[code]
}

var hexSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (r *PublishReport) check() error {
	switch r.Status {
	case "opened":
		if r.PR <= 0 {
			return badInput("opened needs pr")
		}
	case "merged":
		if r.PR <= 0 || !hexSHA.MatchString(r.MergeSHA) {
			return badInput("merged needs pr and a 40-character lowercase hex merge_sha")
		}
	case "failed":
		if _, ok := hookFailText[r.Code]; !ok {
			return badInput("failed needs a known code")
		}
	default:
		return badInput("status must be opened, merged or failed")
	}
	if r.HeadSHA != "" && !hexSHA.MatchString(r.HeadSHA) {
		return badInput("head_sha must be 40 lowercase hex characters")
	}
	if r.Status != "failed" && r.Code != "" {
		return badInput("code is only for failed")
	}
	if r.PR < 0 || r.PR > 1<<30 {
		return badInput("pr must be a positive number")
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// ReportBlogPublish applies an agent's report to its own publish and
// returns the publish's state afterwards. A report repeated after it was
// applied is a no-op. A failed report fails the post at once.
func (s *Store) ReportBlogPublish(ctx context.Context, tok *AgentToken, id int64, r PublishReport) (string, error) {
	p, b, err := s.OwnBlogPublish(ctx, tok, id)
	if err != nil {
		return "", err
	}
	if err := r.check(); err != nil {
		return "", err
	}
	if !p.InFlight() {
		return p.State, ErrNotInFlight
	}
	if r.Attempt != p.Attempt || r.Key != p.Key() || r.ContentSHA != p.ContentSHA {
		return p.State, ErrReportMismatch
	}
	now := s.now()
	switch r.Status {
	case "opened":
		if p.State == PubMerged || (p.State == PubOpened && p.PR == r.PR) {
			return p.State, nil
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = 'opened', pr = ?, head_sha = CASE WHEN ? != '' THEN ? ELSE head_sha END, updated_at = ? WHERE id = ? AND state IN ('waking','sent','opened')`,
			r.PR, r.HeadSHA, r.HeadSHA, now, id); err != nil {
			return "", err
		}
		s.publishEvent(ctx, p.DraftID, now, "PR #"+strconv.Itoa(r.PR)+" opened")
		return PubOpened, nil
	case "merged":
		if p.State == PubMerged {
			if p.MergeSHA == r.MergeSHA && p.PR == r.PR {
				return p.State, nil
			}
			return p.State, ErrReportMismatch
		}
		if _, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = 'merged', pr = ?, head_sha = CASE WHEN ? != '' THEN ? ELSE head_sha END, merge_sha = ?, merged_at = ?, updated_at = ?
			WHERE id = ? AND state IN ('waking','sent','opened')`, r.PR, r.HeadSHA, r.HeadSHA, r.MergeSHA, now, now, id); err != nil {
			return "", err
		}
		// The first live check runs a minute after the merge.
		if _, err := s.DB.ExecContext(ctx, `UPDATE drafts SET lease_until = ?, updated_at = ? WHERE id = ? AND status = 'posting'`, now+int64(MergedChecks[0]/time.Second), now, p.DraftID); err != nil {
			return "", err
		}
		s.publishEvent(ctx, p.DraftID, now, "PR #"+strconv.Itoa(r.PR)+" merged as "+shortSHA(r.MergeSHA))
		return PubMerged, nil
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE blog_publishes SET state = 'failed', code = ?, updated_at = ? WHERE id = ? AND state IN ('waking','sent','opened','merged')`, r.Code, now, id); err != nil {
		return "", err
	}
	if err := s.FinishBlog(ctx, p.DraftID, false, "", "", HookFailText(r.Code, b)); err != nil && !errors.Is(err, ErrConflict) {
		return "", err
	}
	return PubFailed, nil
}

// publishEvent adds an "agent report" line to the draft's history.
func (s *Store) publishEvent(ctx context.Context, draftID, now int64, detail string) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if event(ctx, tx, draftID, now, "agent", "agent report", detail) == nil {
		tx.Commit()
	}
}

// HookRef is the ref xpost records for a post its agent published.
func HookRef(slug string, draftID int64) string {
	return "hook:" + slug + "@" + strconv.FormatInt(draftID, 10)
}

// PublishCover is the publish's cover: its type and bytes.
func (s *Store) PublishCover(ctx context.Context, p *BlogPublish) (string, []byte, error) {
	if p.CoverMediaID == 0 {
		return "", nil, ErrNotFound
	}
	var ref string
	if err := s.DB.QueryRowContext(ctx, `SELECT ref FROM media WHERE id = ?`, p.CoverMediaID).Scan(&ref); err != nil {
		return "", nil, ErrNotFound
	}
	return s.Media(ctx, ref)
}
