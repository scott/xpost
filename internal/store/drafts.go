package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scott/xpost/internal/weight"
)

// Draft statuses. Agents only ever create pending drafts. "scheduled" is
// reserved; nothing sets it (the scheduler uses approved -> posting).
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusScheduled = "scheduled"
	StatusPosting   = "posting"
	StatusPosted    = "posted"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
)

// ValidStatus reports whether s is a draft status.
func ValidStatus(s string) bool {
	switch s {
	case StatusPending, StatusApproved, StatusScheduled, StatusPosting, StatusPosted, StatusFailed, StatusRejected:
		return true
	}
	return false
}

// MediaRef is an image attached to a part.
type MediaRef struct {
	ID   int64
	Ref  string
	Mime string
	Alt  string
}

// Part is one post of a thread. Parts[0] is the post; later parts reply.
type Part struct {
	Text  string
	Media []MediaRef
}

// Count is the part's X-weighted length.
func (p Part) Count() weight.Result { return weight.Count(p.Text) }

// Draft is a proposed post or thread.
type Draft struct {
	ID             int64
	Kind           string // KindX or KindBlog
	Account        string // an X handle, or a blog's destination ("blog:<id>")
	Status         string
	ProposedAt     time.Time
	ScheduledAt    time.Time // the working time; starts as ProposedAt
	Note           string
	RejectReason   string
	Error          string
	TokenID        int64 // 0: created by the owner
	Source         string
	IdempotencyKey string
	Version        int
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ApprovedAt     *time.Time
	LeaseUntil     time.Time // posting: the dispatcher owns it until then
	Attempts       int       // automatic retries so far (429, no connection)
	Parts          []Part    // X drafts
	Post           *BlogPost // blog drafts
	Promotes       int64     // X drafts: the blog draft this promotes, 0 for none
	PromoOffset    *int      // minutes after the blog's publish time; nil is a fixed time
	PromoReturned  bool      // sent back to pending because the blog was not live
	Reply          *Reply    // set on replies (X drafts posted by hand on x.com)
}

// IsPromo reports whether the draft is an X post promoting a blog post.
func (d *Draft) IsPromo() bool { return d.Promotes != 0 }

// IsBlog reports whether the draft is a blog post.
func (d *Draft) IsBlog() bool { return d.Kind == KindBlog }

// Images counts images across parts.
func (d *Draft) Images() int {
	n := 0
	for _, p := range d.Parts {
		n += len(p.Media)
	}
	return n
}

// Alts counts the images that carry alt text (one alt text call each).
func (d *Draft) Alts() int {
	n := 0
	for _, p := range d.Parts {
		for _, m := range p.Media {
			if strings.TrimSpace(m.Alt) != "" {
				n++
			}
		}
	}
	return n
}

// HasURL reports whether any part has a link.
func (d *Draft) HasURL() bool {
	for _, p := range d.Parts {
		if weight.HasURL(p.Text) {
			return true
		}
	}
	return false
}

// PartIn is one part as submitted: text plus media by ref or id.
type PartIn struct {
	Text  string
	Media []MediaIn
}

// MediaIn names an image by ref (agents) or id (owner edits) with alt text.
type MediaIn struct {
	Ref string
	ID  int64
	Alt string
}

// NewDraft is an agent's proposal.
type NewDraft struct {
	Account        string
	Parts          []PartIn
	ProposedAt     time.Time
	Note           string
	IdempotencyKey string
	Promotes       int64     // a blog draft of the same token; 0 for none
	PromoOffset    *int      // minutes after the blog; nil with Promotes set: ProposedAt is a fixed time
	Reply          *NewReply // a reply: posted by hand on x.com, never through the API
}

// ConflictError explains why a draft cannot be approved at its time.
type ConflictError struct {
	Msg      string
	NextFree time.Time
}

func (e *ConflictError) Error() string { return e.Msg }
func (e *ConflictError) Unwrap() error { return ErrConflict }

func validateParts(parts []PartIn) error {
	if len(parts) == 0 {
		return badInput("a draft needs at least one part")
	}
	if len(parts) > MaxParts {
		return badInput(fmt.Sprintf("a thread can have at most %d parts", MaxParts))
	}
	for i, p := range parts {
		n := i + 1
		if len(p.Text) > 4000 {
			return badInput(fmt.Sprintf("part %d is too long", n))
		}
		if strings.TrimSpace(p.Text) == "" && len(p.Media) == 0 {
			return badInput(fmt.Sprintf("part %d is empty", n))
		}
		if c := weight.Count(p.Text); c.Weighted > weight.Limit {
			return badInput(fmt.Sprintf("part %d is %d / %d (weighted). Trim it or split it into a new part", n, c.Weighted, weight.Limit))
		}
		if len(p.Media) > MaxImages {
			return badInput(fmt.Sprintf("part %d has more than %d images", n, MaxImages))
		}
		for _, m := range p.Media {
			if len([]rune(m.Alt)) > MaxAltLen {
				return badInput(fmt.Sprintf("alt text in part %d is over %d characters", n, MaxAltLen))
			}
		}
	}
	return nil
}

func cleanNote(s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > max {
		return "", badInput(fmt.Sprintf("keep it under %d characters", max))
	}
	return s, nil
}

// ---- Agent side ----

// CreateDraft stores an agent's proposal as pending. With an idempotency key
// that token already used, it returns the existing draft untouched and
// created=false: a retry can never change, approve or duplicate a draft.
func (s *Store) CreateDraft(ctx context.Context, tok *AgentToken, nd NewDraft) (*Draft, bool, error) {
	if tok == nil || !tok.Has(ScopeDraftsWrite) {
		return nil, false, ErrNotFound
	}
	nd.IdempotencyKey = strings.TrimSpace(nd.IdempotencyKey)
	if len(nd.IdempotencyKey) > 200 {
		return nil, false, badInput("idempotency_key is too long")
	}
	if nd.IdempotencyKey != "" {
		var id int64
		err := s.DB.QueryRowContext(ctx, `SELECT id FROM drafts WHERE token_id = ? AND idempotency_key = ?`, tok.ID, nd.IdempotencyKey).Scan(&id)
		if err == nil {
			d, err := s.Draft(ctx, id)
			return d, false, err
		}
	}
	if !s.accountExists(ctx, nd.Account) {
		return nil, false, badInput("unknown account " + nd.Account)
	}
	if nd.Reply != nil {
		// A reply has no time: it posts by hand on X after approval. Its
		// working time is when it arrived (it orders the queue).
		if nd.Promotes != 0 || nd.PromoOffset != nil {
			return nil, false, badInput("a reply can't promote a blog post")
		}
		if HasBlogURL(nd.Parts) {
			return nil, false, badInput(BlogURLToken + " works only in a promo post")
		}
		if err := nd.Reply.clean(); err != nil {
			return nil, false, err
		}
		if err := validateReplyParts(nd.Parts); err != nil {
			return nil, false, err
		}
		nd.ProposedAt = s.Now()
	} else if nd.Promotes != 0 {
		at, err := s.promoTime(ctx, tok, &nd)
		if err != nil {
			return nil, false, err
		}
		nd.ProposedAt = at
	} else if HasBlogURL(nd.Parts) {
		return nil, false, badInput(BlogURLToken + " needs promotes: the id of the blog post this promotes")
	}
	if nd.Reply == nil {
		if !nd.ProposedAt.After(s.Now()) {
			return nil, false, badInput("proposed_at must be in the future")
		}
		if err := validateParts(nd.Parts); err != nil {
			return nil, false, err
		}
	}
	note, err := cleanNote(nd.Note, MaxNoteLen)
	if err != nil {
		return nil, false, badInput("note: " + err.Error())
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	// Resolve media: agents may attach only images they uploaded.
	for i := range nd.Parts {
		for j := range nd.Parts[i].Media {
			m := &nd.Parts[i].Media[j]
			if err := tx.QueryRowContext(ctx, `SELECT id FROM media WHERE ref = ? AND token_id = ?`, m.Ref, tok.ID).Scan(&m.ID); err != nil {
				return nil, false, badInput(fmt.Sprintf("part %d: unknown media_ref", i+1))
			}
		}
	}
	if err := checkMediaMix(ctx, tx, nd.Parts); err != nil {
		return nil, false, err
	}
	now := s.now()
	when := nd.ProposedAt.Unix()
	var promotes, offset any
	if nd.Promotes != 0 {
		promotes = nd.Promotes
		if nd.PromoOffset != nil {
			offset = *nd.PromoOffset
		}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO drafts(account, status, proposed_at, scheduled_at, note, token_id, source, idempotency_key, created_at, updated_at, promotes, promo_offset)
		VALUES (?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, nd.Account, when, when, note, tok.ID, tok.Name, nd.IdempotencyKey, now, now, promotes, offset)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && nd.IdempotencyKey != "" {
			tx.Rollback()
			var id int64
			if s.DB.QueryRowContext(ctx, `SELECT id FROM drafts WHERE token_id = ? AND idempotency_key = ?`, tok.ID, nd.IdempotencyKey).Scan(&id) == nil {
				d, err := s.Draft(ctx, id)
				return d, false, err
			}
		}
		return nil, false, err
	}
	id, _ := res.LastInsertId()
	if err := writeParts(ctx, tx, id, nd.Parts); err != nil {
		return nil, false, err
	}
	if nd.Reply != nil {
		if err := insertReply(ctx, tx, id, nd.Reply, now); err != nil {
			return nil, false, err
		}
	}
	if err := event(ctx, tx, id, now, "agent:"+tok.Name, "proposed", ""); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	d, err := s.Draft(ctx, id)
	return d, true, err
}

// AgentDraft returns one of the token's own drafts.
func (s *Store) AgentDraft(ctx context.Context, tok *AgentToken, id int64) (*Draft, error) {
	if tok == nil {
		return nil, ErrNotFound
	}
	d, err := s.Draft(ctx, id)
	if err != nil || d.TokenID != tok.ID {
		return nil, ErrNotFound
	}
	return d, nil
}

// AgentDrafts lists the token's own drafts, newest first.
func (s *Store) AgentDrafts(ctx context.Context, tok *AgentToken, status, account string) ([]*Draft, error) {
	if tok == nil {
		return nil, ErrNotFound
	}
	q := `SELECT id FROM drafts WHERE token_id = ?`
	args := []any{tok.ID}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if account != "" {
		q += ` AND account = ?`
		args = append(args, account)
	}
	return s.draftsByQuery(ctx, q+` ORDER BY created_at DESC, id DESC LIMIT 200`, args...)
}

// AgentDeleteDraft deletes the token's own draft while it is still pending.
func (s *Store) AgentDeleteDraft(ctx context.Context, tok *AgentToken, id int64) error {
	if tok == nil || !tok.Has(ScopeDraftsWrite) {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM drafts WHERE id = ? AND token_id = ?`, id, tok.ID).Scan(&status); err != nil {
		return ErrNotFound
	}
	if status != StatusPending {
		return ErrConflict
	}
	if n := promoCount(ctx, tx, id); n > 0 {
		return &ConflictError{Msg: fmt.Sprintf("%d promo post(s) link to this blog post. Delete them first.", n)}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drafts WHERE id = ? AND status = 'pending'`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Reading ----

const draftCols = `id, kind, account, status, proposed_at, scheduled_at, note, reject_reason, error, COALESCE(token_id, 0), source, idempotency_key, version, created_at, updated_at, approved_at, lease_until, attempts, COALESCE(promotes, 0), promo_offset, promo_returned`

func scanDraft(r scanner) (*Draft, error) {
	var d Draft
	var prop, created, updated, lease int64
	var sched, appr, off sql.NullInt64
	if err := r.Scan(&d.ID, &d.Kind, &d.Account, &d.Status, &prop, &sched, &d.Note, &d.RejectReason, &d.Error, &d.TokenID, &d.Source, &d.IdempotencyKey, &d.Version, &created, &updated, &appr, &lease, &d.Attempts,
		&d.Promotes, &off, &d.PromoReturned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	d.ProposedAt, d.CreatedAt, d.UpdatedAt = time.Unix(prop, 0), time.Unix(created, 0), time.Unix(updated, 0)
	d.ScheduledAt = d.ProposedAt
	if sched.Valid {
		d.ScheduledAt = time.Unix(sched.Int64, 0)
	}
	d.ApprovedAt = tptr(appr)
	d.LeaseUntil = time.Unix(lease, 0)
	if off.Valid {
		m := int(off.Int64)
		d.PromoOffset = &m
	}
	return &d, nil
}

// Draft loads a draft with its parts and media.
func (s *Store) Draft(ctx context.Context, id int64) (*Draft, error) {
	d, err := scanDraft(s.DB.QueryRowContext(ctx, `SELECT `+draftCols+` FROM drafts WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if d.IsBlog() {
		return d, s.loadPost(ctx, d)
	}
	if err := s.loadParts(ctx, d); err != nil {
		return d, err
	}
	return d, s.loadReply(ctx, d)
}

func (s *Store) loadParts(ctx context.Context, d *Draft) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT p.position, p.text, m.id, m.ref, m.mime, pm.alt
		FROM draft_parts p LEFT JOIN part_media pm ON pm.part_id = p.id LEFT JOIN media m ON m.id = pm.media_id
		WHERE p.draft_id = ? ORDER BY p.position, pm.position`, d.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	d.Parts = nil
	last := -1
	for rows.Next() {
		var pos int
		var text string
		var mid sql.NullInt64
		var ref, mime, alt sql.NullString
		if err := rows.Scan(&pos, &text, &mid, &ref, &mime, &alt); err != nil {
			return err
		}
		if pos != last {
			d.Parts = append(d.Parts, Part{Text: text})
			last = pos
		}
		if mid.Valid {
			p := &d.Parts[len(d.Parts)-1]
			p.Media = append(p.Media, MediaRef{ID: mid.Int64, Ref: ref.String, Mime: mime.String, Alt: alt.String})
		}
	}
	return rows.Err()
}

func (s *Store) draftsByQuery(ctx context.Context, q string, args ...any) ([]*Draft, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]*Draft, 0, len(ids))
	for _, id := range ids {
		d, err := s.Draft(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// Filter selects drafts for the queue.
type Filter struct {
	Status  string // default pending
	Account string // "" for all
	Kind    string // "", KindX (posts, not replies), KindReply or KindBlog
}

// notReply and isReplySQL test a drafts row (named drafts) for a reply.
const (
	notReply   = `NOT EXISTS (SELECT 1 FROM draft_replies rp WHERE rp.draft_id = drafts.id)`
	isReplySQL = `EXISTS (SELECT 1 FROM draft_replies rp WHERE rp.draft_id = drafts.id)`
)

// tabWhere is the status condition for a queue tab. A failed blog post
// waits in Pending (it needs Retry publish or a change) and a publishing
// one shows in Approved; failed and posting X drafts live on the Log page.
func tabWhere(status string) (string, []any) {
	switch status {
	case StatusPending:
		return `(status = 'pending' OR (kind = 'blog' AND status = 'failed'))`, nil
	case StatusApproved:
		return `(status = 'approved' OR (kind = 'blog' AND status = 'posting'))`, nil
	}
	return `status = ?`, []any{status}
}

// postedAt is when a draft finished posting: the time its last part was
// recorded as posted, or (no posted rows) the draft's last update, which
// FinishDraft sets when it marks the draft posted.
const postedAt = `COALESCE((SELECT MAX(l.at) FROM post_log l WHERE l.draft_id = drafts.id AND l.status = 'posted'), drafts.updated_at)`

// Queue lists drafts for the review UI: soonest first; rejected newest
// first; posted newest first by posted time.
func (s *Store) Queue(ctx context.Context, f Filter) ([]*Draft, error) {
	if f.Status == "" {
		f.Status = StatusPending
	}
	where, args := tabWhere(f.Status)
	q := `SELECT id FROM drafts WHERE ` + where
	if f.Account != "" {
		q += ` AND account = ?`
		args = append(args, f.Account)
	}
	if s.NoBlogs {
		q += ` AND kind != 'blog'`
	}
	switch f.Kind {
	case "":
	case KindReply:
		q += ` AND kind = 'x' AND ` + isReplySQL
	case KindX:
		q += ` AND kind = 'x' AND ` + notReply
	default:
		q += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	order := ` ORDER BY scheduled_at, id`
	switch f.Status {
	case StatusRejected:
		order = ` ORDER BY updated_at DESC, id DESC`
	case StatusPosted:
		order = ` ORDER BY ` + postedAt + ` DESC, id DESC`
	}
	return s.draftsByQuery(ctx, q+order+` LIMIT 200`, args...)
}

// Counts are queue tallies: by tab (pending includes failed blog posts,
// approved includes publishing ones), X drafts posting or failed (shown on
// the Log page), and pending by account and by kind (replies count as
// KindReply, not KindX).
type Counts struct {
	ByStatus         map[string]int
	PendingByAccount map[string]int
	PendingByKind    map[string]int
}

// QueueCounts tallies drafts for the filter chips and the badge. account
// and kind narrow the status tallies ("" for all).
func (s *Store) QueueCounts(ctx context.Context, account, kind string) (Counts, error) {
	c := Counts{ByStatus: map[string]int{}, PendingByAccount: map[string]int{}, PendingByKind: map[string]int{}}
	rows, err := s.DB.QueryContext(ctx, `SELECT account, CASE WHEN kind = 'x' AND `+isReplySQL+` THEN 'reply' ELSE kind END AS k, status, COUNT(*) FROM drafts GROUP BY account, k, status`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, k, st string
		var n int
		if err := rows.Scan(&a, &k, &st, &n); err != nil {
			return c, err
		}
		if s.NoBlogs && k == KindBlog {
			continue
		}
		tab := st
		if k == KindBlog && st == StatusFailed {
			tab = StatusPending
		} else if k == KindBlog && st == StatusPosting {
			tab = StatusApproved
		}
		if tab == StatusPending {
			c.PendingByKind[k] += n
			if kind == "" || kind == k {
				c.PendingByAccount[a] += n
			}
		}
		if (account == "" || account == a) && (kind == "" || kind == k) {
			c.ByStatus[tab] += n
		}
	}
	return c, rows.Err()
}

// PendingCount is the review badge number: pending drafts plus failed blog
// posts.
func (s *Store) PendingCount(ctx context.Context) int {
	var n int
	q := `SELECT COUNT(*) FROM drafts WHERE status = 'pending' OR (kind = 'blog' AND status = 'failed')`
	if s.NoBlogs {
		q = `SELECT COUNT(*) FROM drafts WHERE status = 'pending' AND kind != 'blog'`
	}
	s.DB.QueryRowContext(ctx, q).Scan(&n)
	return n
}

// Between lists X drafts (any status but rejected) whose time is in
// [from, to). Replies have no time and never post through the API, so
// they are not on the Schedule and not in the spend estimate.
func (s *Store) Between(ctx context.Context, from, to time.Time) ([]*Draft, error) {
	return s.draftsByQuery(ctx, `SELECT id FROM drafts WHERE kind = 'x' AND `+notReply+` AND status != 'rejected' AND scheduled_at >= ? AND scheduled_at < ? ORDER BY scheduled_at, id`, from.Unix(), to.Unix())
}

// BlogsBetween lists blog posts (not rejected) whose publish time is in
// [from, to), for the Schedule's day banners.
func (s *Store) BlogsBetween(ctx context.Context, from, to time.Time) ([]*Draft, error) {
	if s.NoBlogs {
		return nil, nil
	}
	return s.draftsByQuery(ctx, `SELECT id FROM drafts WHERE kind = 'blog' AND status != 'rejected' AND scheduled_at >= ? AND scheduled_at < ? ORDER BY scheduled_at, id`, from.Unix(), to.Unix())
}

// Event is one history line for a draft.
type Event struct {
	At     time.Time
	Actor  string
	Kind   string
	Detail string
}

// Events returns a draft's history, oldest first.
func (s *Store) Events(ctx context.Context, id int64) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT at, actor, kind, detail FROM draft_events WHERE draft_id = ? ORDER BY at, id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&at, &e.Actor, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func event(ctx context.Context, tx *sql.Tx, id, at int64, actor, kind, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO draft_events(draft_id, at, actor, kind, detail) VALUES (?,?,?,?,?)`, id, at, actor, kind, detail)
	return err
}

func writeParts(ctx context.Context, tx *sql.Tx, draftID int64, parts []PartIn) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM draft_parts WHERE draft_id = ?`, draftID); err != nil {
		return err
	}
	for i, p := range parts {
		res, err := tx.ExecContext(ctx, `INSERT INTO draft_parts(draft_id, position, text) VALUES (?,?,?)`, draftID, i, p.Text)
		if err != nil {
			return err
		}
		pid, _ := res.LastInsertId()
		for j, m := range p.Media {
			if _, err := tx.ExecContext(ctx, `INSERT INTO part_media(part_id, position, media_id, alt) VALUES (?,?,?,?)`, pid, j, m.ID, strings.TrimSpace(m.Alt)); err != nil {
				return err
			}
		}
	}
	return nil
}
