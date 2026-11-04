package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
)

// Blog post limits.
const (
	MaxTitle       = 200
	MaxDescription = 1000
	MaxTags        = 10
	MaxTagLen      = 40
	MaxMarkdown    = 256 << 10
)

// BlogPost is the blog half of a blog draft.
type BlogPost struct {
	BlogID        int64
	Slug          string
	Title         string
	Description   string
	Tags          []string
	Markdown      string
	Cover         *MediaRef // nil: no cover
	StagedAt      *time.Time
	PublishedURL  string
	Ref           string
	LiveCheckedAt *time.Time
	ImportedAt    *time.Time // imported from the live site (no body kept)
	AdoptedAt     *time.Time // a blocked draft the import matched to the live post
}

// Staged reports whether the post was staged on its blog: its slug is
// locked from then on.
func (p *BlogPost) Staged() bool { return p.StagedAt != nil }

func (s *Store) loadPost(ctx context.Context, d *Draft) error {
	var p BlogPost
	var tags string
	var coverID sql.NullInt64
	var coverRef, coverMime sql.NullString
	var alt string
	var staged, live, imported, adopted sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT bp.blog_id, bp.slug, bp.title, bp.description, bp.tags, bp.markdown, bp.cover_media_id, m.ref, m.mime, bp.cover_alt,
		bp.staged_at, bp.published_url, bp.ref, bp.live_checked_at, bp.imported_at, bp.adopted_at FROM blog_posts bp LEFT JOIN media m ON m.id = bp.cover_media_id WHERE bp.draft_id = ?`, d.ID).
		Scan(&p.BlogID, &p.Slug, &p.Title, &p.Description, &tags, &p.Markdown, &coverID, &coverRef, &coverMime, &alt, &staged, &p.PublishedURL, &p.Ref, &live, &imported, &adopted)
	if err != nil {
		return err
	}
	json.Unmarshal([]byte(tags), &p.Tags)
	if coverID.Valid {
		p.Cover = &MediaRef{ID: coverID.Int64, Ref: coverRef.String, Mime: coverMime.String, Alt: alt}
	}
	p.StagedAt, p.LiveCheckedAt, p.ImportedAt, p.AdoptedAt = tptr(staged), tptr(live), tptr(imported), tptr(adopted)
	d.Post = &p
	return nil
}

// LintError refuses a post whose lint found something that blocks.
type LintError struct{ Findings []blogtext.Finding }

func (e *LintError) Error() string {
	n, _ := blogtext.Counts(e.Findings)
	if n == 1 {
		return "Fix 1 lint error to approve or publish."
	}
	return fmt.Sprintf("Fix %d lint errors to approve or publish.", n)
}

func (e *LintError) Unwrap() error { return ErrConflict }

// LintPost runs the blog's lint over a post.
func LintPost(b *Blog, p *BlogPost) []blogtext.Finding {
	return blogtext.Lint(p.Title, p.Description, p.Tags, p.Markdown, b.Rules)
}

func oneLine(s string) bool { return !strings.ContainsAny(s, "\r\n") }

func cleanTags(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.Join(strings.Fields(t), " ")
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if len([]rune(t)) > MaxTagLen {
			return nil, badInput(fmt.Sprintf("a tag can be up to %d characters", MaxTagLen))
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, badInput(fmt.Sprintf("up to %d tags", MaxTags))
	}
	return out, nil
}

// cleanPost checks the fields both intake and the owner's edit set.
func cleanPost(p *BlogPost) error {
	p.Title = strings.TrimSpace(p.Title)
	p.Description = strings.TrimSpace(p.Description)
	p.Slug = strings.TrimPrefix(strings.TrimSpace(p.Slug), "/")
	switch {
	case p.Title == "" || !oneLine(p.Title) || len([]rune(p.Title)) > MaxTitle:
		return badInput(fmt.Sprintf("title: one line, 1 to %d characters", MaxTitle))
	case p.Description == "" || !oneLine(p.Description) || len([]rune(p.Description)) > MaxDescription:
		return badInput(fmt.Sprintf("description: one line, 1 to %d characters", MaxDescription))
	case !blogtext.ValidSlug(p.Slug):
		return badInput("slug: lowercase letters, digits and single hyphens, up to 80 characters (e.g. sample-build-week)")
	case strings.TrimSpace(p.Markdown) == "":
		return badInput("markdown is empty")
	case len(p.Markdown) > MaxMarkdown:
		return badInput("markdown is over 256 KiB")
	}
	tags, err := cleanTags(p.Tags)
	if err != nil {
		return err
	}
	p.Tags = tags
	return nil
}

func slugTaken(ctx context.Context, q querier, blogID int64, slug string, exclude int64) bool {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM blog_posts WHERE blog_id = ? AND slug = ? AND draft_id != ?`, blogID, slug, exclude).Scan(&n)
	return n > 0
}

// NewBlogDraft is an agent's blog post proposal.
type NewBlogDraft struct {
	Site           string // the blog's name, e.g. "notes.example.com"
	Slug           string
	Title          string
	Description    string
	Tags           []string
	Markdown       string
	Cover          *MediaIn // by ref; nil for none
	ProposedAt     time.Time
	Note           string
	IdempotencyKey string
}

// CreateBlogDraft stores an agent's blog post as pending. Without a
// proposed time it gets the blog's default time (the next one after now in
// loc, the owner's display zone). Lint that blocks is refused with a
// *LintError, so the agent hears about it at once. The idempotency rules
// are CreateDraft's.
func (s *Store) CreateBlogDraft(ctx context.Context, tok *AgentToken, nb NewBlogDraft, loc *time.Location) (*Draft, bool, error) {
	if tok == nil || !tok.Has(ScopeDraftsWrite) {
		return nil, false, ErrNotFound
	}
	nb.IdempotencyKey = strings.TrimSpace(nb.IdempotencyKey)
	if len(nb.IdempotencyKey) > 200 {
		return nil, false, badInput("idempotency_key is too long")
	}
	if nb.IdempotencyKey != "" {
		var id int64
		if s.DB.QueryRowContext(ctx, `SELECT id FROM drafts WHERE token_id = ? AND idempotency_key = ?`, tok.ID, nb.IdempotencyKey).Scan(&id) == nil {
			d, err := s.Draft(ctx, id)
			return d, false, err
		}
	}
	b, err := s.BlogByName(ctx, nb.Site)
	if err != nil {
		var names []string
		if bs, _ := s.Blogs(ctx); len(bs) > 0 {
			for _, x := range bs {
				names = append(names, x.Name)
			}
		}
		if len(names) == 0 {
			return nil, false, badInput("unknown site " + strings.TrimSpace(nb.Site) + ": no blogs are set up")
		}
		return nil, false, badInput("unknown site " + strings.TrimSpace(nb.Site) + "; blogs: " + strings.Join(names, ", "))
	}
	p := &BlogPost{BlogID: b.ID, Slug: nb.Slug, Title: nb.Title, Description: nb.Description, Tags: nb.Tags, Markdown: nb.Markdown}
	if err := cleanPost(p); err != nil {
		return nil, false, err
	}
	if nb.ProposedAt.IsZero() {
		nb.ProposedAt = DefaultPublishAt(s.Now(), b.PublishClock, b.Location(loc))
	} else if !nb.ProposedAt.After(s.Now()) {
		return nil, false, badInput("proposed_at must be in the future")
	}
	note, err := cleanNote(nb.Note, MaxNoteLen)
	if err != nil {
		return nil, false, badInput("note: " + err.Error())
	}
	var coverID any
	var alt string
	if nb.Cover != nil && strings.TrimSpace(nb.Cover.Ref) != "" {
		var id int64
		if err := s.DB.QueryRowContext(ctx, `SELECT id FROM media WHERE ref = ? AND token_id = ?`, nb.Cover.Ref, tok.ID).Scan(&id); err != nil {
			return nil, false, badInput("cover: unknown media_ref")
		}
		alt = strings.TrimSpace(nb.Cover.Alt)
		if len([]rune(alt)) > MaxAltLen {
			return nil, false, badInput(fmt.Sprintf("cover alt text is over %d characters", MaxAltLen))
		}
		coverID = id
	}
	if b.CoverAltRequired && (coverID == nil || alt == "") {
		return nil, false, badInput(b.Name + " needs a cover with alt text: send cover {ref, alt}")
	}
	if fs := LintPost(b, p); func() bool { e, _ := blogtext.Counts(fs); return e > 0 }() {
		return nil, false, &LintError{Findings: fs}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if slugTaken(ctx, tx, b.ID, p.Slug, 0) {
		return nil, false, badInput("slug " + p.Slug + " is already used on " + b.Name)
	}
	now := s.now()
	when := nb.ProposedAt.Unix()
	res, err := tx.ExecContext(ctx, `INSERT INTO drafts(kind, account, status, proposed_at, scheduled_at, note, token_id, source, idempotency_key, created_at, updated_at)
		VALUES ('blog', ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?)`, b.Account, when, when, note, tok.ID, tok.Name, nb.IdempotencyKey, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && nb.IdempotencyKey != "" {
			tx.Rollback()
			var id int64
			if s.DB.QueryRowContext(ctx, `SELECT id FROM drafts WHERE token_id = ? AND idempotency_key = ?`, tok.ID, nb.IdempotencyKey).Scan(&id) == nil {
				d, err := s.Draft(ctx, id)
				return d, false, err
			}
		}
		return nil, false, err
	}
	id, _ := res.LastInsertId()
	tags, _ := json.Marshal(p.Tags)
	if _, err := tx.ExecContext(ctx, `INSERT INTO blog_posts(draft_id, blog_id, slug, title, description, tags, markdown, cover_media_id, cover_alt) VALUES (?,?,?,?,?,?,?,?,?)`,
		id, b.ID, p.Slug, p.Title, p.Description, string(tags), p.Markdown, coverID, alt); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, false, badInput("slug " + p.Slug + " is already used on " + b.Name)
		}
		return nil, false, err
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

// blogApprovable refuses approving or publishing a blog post whose lint
// blocks, whose blog has not passed its last test, or whose blog is gone.
func (s *Store) blogApprovable(ctx context.Context, id int64) error {
	d, err := s.Draft(ctx, id)
	if err != nil {
		return err
	}
	b, err := s.BlogForAccount(ctx, d.Account)
	if err != nil {
		return err
	}
	var removed sql.NullInt64
	s.DB.QueryRowContext(ctx, `SELECT removed_at FROM blogs WHERE id = ?`, b.ID).Scan(&removed)
	if removed.Valid {
		return &ConflictError{Msg: b.Name + " was removed, so this post can't publish."}
	}
	if fs := LintPost(b, d.Post); func() bool { e, _ := blogtext.Counts(fs); return e > 0 }() {
		le := &LintError{Findings: fs}
		return &ConflictError{Msg: le.Error()}
	}
	if !b.Ready() {
		return &ConflictError{Msg: b.Name + " cannot publish until its last test passes. Drafts can still be reviewed."}
	}
	if b.CoverAltRequired && (d.Post.Cover == nil || strings.TrimSpace(d.Post.Cover.Alt) == "") {
		return &ConflictError{Msg: b.Name + " needs cover alt text. Add it in Edit."}
	}
	return nil
}

// BlogEdit is the owner's change to a blog post.
type BlogEdit struct {
	Title       string
	Description string
	Slug        string
	Tags        []string
	Markdown    string
	CoverAlt    string
	When        time.Time
}

// SaveBlogPost replaces a pending, approved or failed blog post's text and
// time. It always goes back to Pending (approve again to publish). The
// slug can't change once the post was staged.
func (s *Store) SaveBlogPost(ctx context.Context, u *User, id int64, version int, e BlogEdit) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.kind != KindBlog {
		return ErrConflict
	}
	if r.status != StatusPending && r.status != StatusApproved && r.status != StatusFailed {
		return ErrConflict
	}
	if err := s.rewriteBlock(ctx, tx, id); err != nil {
		return err
	}
	// The owner's edit ends what the card shows about past rewrites.
	if _, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET dismissed = 1 WHERE draft_id = ?`, id); err != nil {
		return err
	}
	d, err := s.Draft(ctx, id)
	if err != nil {
		return err
	}
	b, err := s.BlogForAccount(ctx, d.Account)
	if err != nil {
		return err
	}
	p := &BlogPost{BlogID: d.Post.BlogID, Slug: e.Slug, Title: e.Title, Description: e.Description, Tags: e.Tags, Markdown: e.Markdown}
	if err := cleanPost(p); err != nil {
		return err
	}
	if d.Post.Staged() && p.Slug != d.Post.Slug {
		return badInput("The slug is locked: this post is already staged on " + b.Name + ".")
	}
	if slugTaken(ctx, tx, p.BlogID, p.Slug, id) {
		return badInput("slug " + p.Slug + " is already used on " + b.Name)
	}
	alt := strings.TrimSpace(e.CoverAlt)
	if len([]rune(alt)) > MaxAltLen {
		return badInput(fmt.Sprintf("cover alt text is over %d characters", MaxAltLen))
	}
	if d.Post.Cover != nil && b.CoverAltRequired && alt == "" {
		return badInput("Alt text is required for " + b.Name + ".")
	}
	if e.When.IsZero() {
		return badInput("pick a date and time")
	}
	tags, _ := json.Marshal(p.Tags)
	if _, err := tx.ExecContext(ctx, `UPDATE blog_posts SET slug = ?, title = ?, description = ?, tags = ?, markdown = ?, cover_alt = ? WHERE draft_id = ?`,
		p.Slug, p.Title, p.Description, string(tags), p.Markdown, alt, id); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'pending', scheduled_at = ?, approved_at = NULL, error = '', updated_at = ?, version = version + 1 WHERE id = ?`,
		e.When.Unix(), now, id); err != nil {
		return err
	}
	if err := syncPromos(ctx, tx, id, e.When, now); err != nil {
		return err
	}
	kind := "edited"
	if r.status != StatusPending {
		kind += "; back to pending"
	}
	detail := ""
	if e.When.Unix() != r.when {
		detail = "to " + e.When.UTC().Format(time.RFC3339)
	}
	if err := event(ctx, tx, id, now, ownerActor, kind, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishNow approves a blog post for right now, replacing its slot. It
// runs Approve's checks (lint, a passed blog test, spacing is not needed:
// the slot is now). From failed it is "Retry publish". The caller then
// publishes it through the blog publisher.
func (s *Store) PublishNow(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.kind != KindBlog {
		return ErrConflict
	}
	if r.status != StatusPending && r.status != StatusApproved && r.status != StatusFailed {
		return ErrConflict
	}
	if err := s.rewriteBlock(ctx, tx, id); err != nil {
		return err
	}
	if err := s.blogApprovable(ctx, id); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'approved', scheduled_at = ?, approved_at = ?, error = '', attempts = 0, updated_at = ?, version = version + 1, reject_reason = '' WHERE id = ?`,
		now, now, now, id); err != nil {
		return err
	}
	// Offset promos keep their offset from the new slot.
	if err := syncPromos(ctx, tx, id, time.Unix(now, 0), now); err != nil {
		return err
	}
	kind := "publish now"
	if r.status == StatusFailed {
		kind = "retry publish"
	}
	if err := event(ctx, tx, id, now, ownerActor, kind, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkStaged records that the post was staged on its blog (the slug locks)
// with the blog's reference for it.
func (s *Store) MarkStaged(ctx context.Context, id int64, ref string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blog_posts SET staged_at = COALESCE(staged_at, ?), ref = CASE WHEN ? != '' THEN ? ELSE ref END WHERE draft_id = ?`, s.now(), ref, ref, id)
	return err
}

// DueBlogs lists blog posts the publisher may work on now: approved and
// due, or publishing with a lapsed lease, on blogs that are not paused.
func (s *Store) DueBlogs(ctx context.Context, now time.Time) ([]Due, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.version, d.status, d.account, d.lease_until, d.scheduled_at FROM drafts d JOIN accounts a ON a.handle = d.account
		WHERE d.kind = 'blog' AND a.paused = 0 AND ((d.status = 'approved' AND d.scheduled_at <= ?) OR (d.status = 'posting' AND d.lease_until <= ?))
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

// FinishBlog ends a publishing blog post: published at url (the live
// check passed), or failed with a short, owner-facing reason.
func (s *Store) FinishBlog(ctx context.Context, id int64, ok bool, url, ref, why string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now()
	status, detail := StatusFailed, why
	if ok {
		status, detail = StatusPosted, url
		if _, err := tx.ExecContext(ctx, `UPDATE blog_posts SET published_url = ?, ref = CASE WHEN ? != '' THEN ? ELSE ref END, live_checked_at = ?, staged_at = COALESCE(staged_at, ?) WHERE draft_id = ?`,
			url, ref, ref, now, now, id); err != nil {
			return err
		}
		why = ""
	}
	res, err := tx.ExecContext(ctx, `UPDATE drafts SET status = ?, error = ?, lease_until = 0, updated_at = ? WHERE id = ? AND status = 'posting' AND kind = 'blog'`, status, why, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrConflict
	}
	if ok {
		// Offset promos count from the real publish time (now, once the
		// post is confirmed live), not the slot: a late publish moves them.
		// Fixed-time promos keep their time.
		if err := syncPromos(ctx, tx, id, time.Unix(now, 0), now); err != nil {
			return err
		}
	}
	kind := "published"
	if !ok {
		kind = "publish failed"
	}
	if err := event(ctx, tx, id, now, "scheduler", kind, detail); err != nil {
		return err
	}
	return tx.Commit()
}
