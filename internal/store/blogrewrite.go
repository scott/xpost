package store

import (
	"context"
	"encoding/json"
	"slices"
)

// BlogRevise is an agent's new version of a blog post in answer to a
// rewrite request. Only the text changes: the slug, cover, cover alt, time
// and blog stay as they are.
type BlogRevise struct {
	Title       string
	Description string
	Tags        []string
	Markdown    string
}

// ReviseBlogPost is ReviseDraft for a blog post. The new text passes the
// same checks as intake (one-line title and description, tag limits,
// markdown size); lint is not a reason to refuse it: the draft comes back
// Pending with its lint shown, and blocking lint still stops Approve and
// Publish now. A publish in flight refuses the reply and is left frozen.
func (s *Store) ReviseBlogPost(ctx context.Context, tok *AgentToken, id, reqID int64, in BlogRevise) (*Draft, string, error) {
	if tok == nil || !tok.Has(ScopeDraftsRevise) {
		return nil, "", ErrNotFound
	}
	d, err := s.Draft(ctx, id)
	if err != nil || d.TokenID != tok.ID {
		return nil, "", ErrNotFound
	}
	if !d.IsBlog() {
		return nil, "", badInput(`this draft is an X post: send "parts", not "post"`)
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	req, err := s.replyTo(ctx, tx, tok, d, reqID)
	if err != nil {
		return nil, "", err
	}
	if p, err := s.LatestBlogPublish(ctx, id); err == nil && p.InFlight() {
		return nil, "", &RewriteError{Msg: "a publish is underway for this post; this reply was not applied"}
	}
	old := d.Post
	p := &BlogPost{BlogID: old.BlogID, Slug: old.Slug, Title: in.Title, Description: in.Description, Tags: in.Tags, Markdown: in.Markdown}
	if err := cleanPost(p); err != nil {
		return nil, "", err
	}
	nowU := s.Now().Unix()
	oldTags := old.Tags
	if oldTags == nil {
		oldTags = []string{}
	}
	if p.Title == old.Title && p.Description == old.Description && p.Markdown == old.Markdown && slices.Equal(p.Tags, oldTags) {
		if err := answer(ctx, tx, tok, id, req, nowU, false); err != nil {
			return nil, "", err
		}
		if err := tx.Commit(); err != nil {
			return nil, "", err
		}
		d, err := s.Draft(ctx, id)
		return d, RewriteUnchanged, err
	}
	pj, _ := json.Marshal(RevisionPost{Title: old.Title, Description: old.Description, Tags: oldTags, Markdown: old.Markdown})
	author, authoredAt := s.replyAuthor(ctx, tx, tok, d)
	var curV int
	tx.QueryRowContext(ctx, `SELECT version FROM drafts WHERE id = ?`, id).Scan(&curV)
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_revisions(draft_id, request_id, version, parts_json, post_json, author, authored_at, created_at) VALUES (?,?,?,'[]',?,?,?,?)`,
		id, req.ID, curV, string(pj), author, authoredAt, nowU); err != nil {
		return nil, "", err
	}
	tags, _ := json.Marshal(p.Tags)
	if _, err := tx.ExecContext(ctx, `UPDATE blog_posts SET title = ?, description = ?, tags = ?, markdown = ? WHERE draft_id = ?`,
		p.Title, p.Description, string(tags), p.Markdown, id); err != nil {
		return nil, "", err
	}
	if err := answer(ctx, tx, tok, id, req, nowU, true); err != nil {
		return nil, "", err
	}
	if err := tx.Commit(); err != nil {
		return nil, "", err
	}
	d, err = s.Draft(ctx, id)
	return d, RewriteRevised, err
}
