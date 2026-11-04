package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rewrite: the owner sends a pending agent draft (an X post or a blog
// post) back to the agent that wrote it. The draft stays pending throughout; an open request blocks
// approve and edit (reschedule and reject stay available, and reject
// cancels the request). Rewrites call no X endpoint, so they never touch
// the spend meter, and there is no limit on rounds.

// RewriteTimeout is how long xpost waits for the agent's new version.
const RewriteTimeout = 60 * time.Minute

// MaxRewriteNote is the longest note the owner can send with a request.
const MaxRewriteNote = 280

// Rewrite request states as stored. "Rewriting" is RewriteOpen before
// ExpiresAt; an open request past ExpiresAt is "No rewrite yet".
const (
	RewriteOpen      = "open"
	RewriteRevised   = "revised"
	RewriteUnchanged = "unchanged"
	RewriteCancelled = "cancelled"
	RewriteFailed    = "failed"
)

// RewriteRequest is one request to an agent.
type RewriteRequest struct {
	ID, DraftID, TokenID int64
	Note                 string
	SentAt, ExpiresAt    time.Time
	State                string
	AnsweredAt           *time.Time
	RevisedVersion       int
	Dismissed            bool
}

// Waiting reports whether the agent can still answer at now.
func (r *RewriteRequest) Waiting(now time.Time) bool {
	return r.State == RewriteOpen && now.Before(r.ExpiresAt)
}

// TimedOut reports whether the request expired with no reply.
func (r *RewriteRequest) TimedOut(now time.Time) bool {
	return r.State == RewriteOpen && !now.Before(r.ExpiresAt)
}

// Webhook is an agent token's rewrite webhook. The key is never part of it.
type Webhook struct {
	TokenID    int64
	URL        string
	Last4      string
	TestedAt   *time.Time
	TestOK     bool
	TestDetail string
	TestMs     int
	UpdatedAt  time.Time
	HasAuth    bool // an Authorization header value is saved (never readable here)
}

// RevisionPart is one part of a replaced version.
type RevisionPart struct {
	Text   string `json:"text"`
	Images int    `json:"images"`
}

// RevisionPost is a blog post version a rewrite replaced.
type RevisionPost struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Markdown    string   `json:"markdown"`
}

// Revision is a version a rewrite replaced.
type Revision struct {
	ID         int64
	RequestID  int64
	Version    int
	Parts      []RevisionPart
	Post       *RevisionPost // blog posts only
	Author     string
	AuthoredAt time.Time
	CreatedAt  time.Time
}

// RewriteError is a refused agent reply: it answers no open request.
type RewriteError struct{ Msg string }

func (e *RewriteError) Error() string { return e.Msg }
func (e *RewriteError) Unwrap() error { return ErrConflict }

func webhookAAD(tokenID int64) []byte {
	return []byte("agent_webhooks.key:" + strconv.FormatInt(tokenID, 10))
}

// webhookAuthAAD binds a sealed Authorization value to its row and column,
// so it can't be swapped with a sender key or another agent's value.
func webhookAuthAAD(tokenID int64) []byte {
	return []byte("agent_webhooks.auth:" + strconv.FormatInt(tokenID, 10))
}

// MaxWebhookAuth bounds the Authorization header value.
const MaxWebhookAuth = 4096

// authPrefix is the header name some panels include in the value they hand
// out ("Authorization: Bearer ..."). xpost sends the value under that name
// itself, so one leading copy is removed.
const authPrefix = "Authorization:"

// CleanWebhookAuth trims an Authorization header value, removes one leading
// "Authorization:" (any case, no space before the colon) and refuses line
// breaks and other control characters (header injection). Tabs inside are
// allowed, as HTTP allows them in field values. An empty input stays empty
// (the caller keeps the saved value); a bare prefix is refused.
func CleanWebhookAuth(v string) (string, error) {
	v = strings.TrimSpace(v)
	if len(v) >= len(authPrefix) && strings.EqualFold(v[:len(authPrefix)], authPrefix) {
		v = strings.TrimSpace(v[len(authPrefix):])
		if v == "" {
			return "", badInput("paste the Authorization header value, not just the name")
		}
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", badInput("the Authorization header must be one line: remove the line break")
	}
	for _, r := range v {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return "", badInput("the Authorization header contains a control character")
		}
	}
	if len(v) > MaxWebhookAuth {
		return "", badInput("the Authorization header is too long")
	}
	return v, nil
}

// SetAgentWebhookAuth seals and saves an Authorization header value for a
// saved webhook. Saving resets the last test, like any webhook change.
func (s *Store) SetAgentWebhookAuth(ctx context.Context, u *User, tokenID int64, value string) error {
	if u == nil {
		return ErrNotFound
	}
	v, err := CleanWebhookAuth(value)
	if err != nil {
		return err
	}
	if v == "" {
		return badInput("paste the Authorization header value")
	}
	sealed, err := s.Key.Seal([]byte(v), webhookAuthAAD(tokenID))
	if err != nil {
		return err
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE agent_webhooks SET auth_sealed = ?, tested_at = NULL, test_ok = 0, test_detail = '', test_ms = 0, updated_at = ? WHERE token_id = ?`, sealed, s.now(), tokenID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return badInput("save the URL and sender key first")
	}
	return nil
}

// ClearAgentWebhookAuth removes the Authorization header value.
func (s *Store) ClearAgentWebhookAuth(ctx context.Context, u *User, tokenID int64) error {
	if u == nil {
		return ErrNotFound
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE agent_webhooks SET auth_sealed = NULL, tested_at = NULL, test_ok = 0, test_detail = '', test_ms = 0, updated_at = ? WHERE token_id = ?`, s.now(), tokenID)
	return err
}

// WebhookAuth opens the Authorization header value for sending; "" when
// none is saved.
func (s *Store) WebhookAuth(ctx context.Context, tokenID int64) (string, error) {
	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT auth_sealed FROM agent_webhooks WHERE token_id = ?`, tokenID).Scan(&sealed); err != nil {
		return "", ErrNotFound
	}
	if sealed == nil {
		return "", nil
	}
	v, err := s.Key.Open(sealed, webhookAuthAAD(tokenID))
	if err != nil {
		return "", errors.New("rewrite webhook Authorization value could not be opened")
	}
	return string(v), nil
}

// ---- Webhook settings (owner) ----

const webhookCols = `token_id, url, key_last4, tested_at, test_ok, test_detail, test_ms, updated_at, auth_sealed IS NOT NULL`

func scanWebhook(r scanner) (*Webhook, error) {
	var w Webhook
	var tested sql.NullInt64
	var ok int
	var upd int64
	if err := r.Scan(&w.TokenID, &w.URL, &w.Last4, &tested, &ok, &w.TestDetail, &w.TestMs, &upd, &w.HasAuth); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	w.TestedAt, w.TestOK, w.UpdatedAt = tptr(tested), ok == 1, time.Unix(upd, 0)
	return &w, nil
}

// AgentWebhook is the token's webhook, or ErrNotFound.
func (s *Store) AgentWebhook(ctx context.Context, tokenID int64) (*Webhook, error) {
	return scanWebhook(s.DB.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM agent_webhooks WHERE token_id = ?`, tokenID))
}

// AgentWebhooks maps token id to webhook.
func (s *Store) AgentWebhooks(ctx context.Context) (map[int64]*Webhook, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+webhookCols+` FROM agent_webhooks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out[w.TokenID] = w
	}
	return out, rows.Err()
}

// SetAgentWebhook saves a live token's webhook URL and sender key. An empty
// key keeps the saved one (the form never shows it). Saving clears the
// last test result. The caller validates the URL scheme and host.
func (s *Store) SetAgentWebhook(ctx context.Context, u *User, tokenID int64, url, key string) error {
	if u == nil {
		return ErrNotFound
	}
	t, err := s.AgentTokenByID(ctx, tokenID)
	if err != nil || t.RevokedAt != nil {
		return ErrNotFound
	}
	url, key = strings.TrimSpace(url), strings.TrimSpace(key)
	if url == "" {
		return badInput("enter the rewrite webhook URL")
	}
	if len(url) > 500 {
		return badInput("the webhook URL is too long")
	}
	now := s.now()
	if key == "" {
		res, err := s.DB.ExecContext(ctx, `UPDATE agent_webhooks SET url = ?, tested_at = NULL, test_ok = 0, test_detail = '', test_ms = 0, updated_at = ? WHERE token_id = ?`, url, now, tokenID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return badInput("paste the sender key from " + t.Name)
		}
		return nil
	}
	if len(key) < 16 || len(key) > 512 || strings.ContainsAny(key, " \t\r\n") {
		return badInput("the sender key should be 16 to 512 characters with no spaces")
	}
	sealed, err := s.Key.Seal([]byte(key), webhookAAD(tokenID))
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO agent_webhooks(token_id, url, key_sealed, key_last4, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(token_id) DO UPDATE SET url = excluded.url, key_sealed = excluded.key_sealed, key_last4 = excluded.key_last4,
		tested_at = NULL, test_ok = 0, test_detail = '', test_ms = 0, updated_at = excluded.updated_at`, tokenID, url, sealed, key[len(key)-4:], now)
	return err
}

// RemoveAgentWebhook deletes a token's webhook (Rewrite turns off for it).
func (s *Store) RemoveAgentWebhook(ctx context.Context, u *User, tokenID int64) error {
	if u == nil {
		return ErrNotFound
	}
	_, err := s.DB.ExecContext(ctx, `DELETE FROM agent_webhooks WHERE token_id = ?`, tokenID)
	return err
}

// WebhookSecret opens the token's sender key, only to sign a request.
func (s *Store) WebhookSecret(ctx context.Context, tokenID int64) (string, string, error) {
	var url string
	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT url, key_sealed FROM agent_webhooks WHERE token_id = ?`, tokenID).Scan(&url, &sealed); err != nil {
		return "", "", ErrNotFound
	}
	k, err := s.Key.Open(sealed, webhookAAD(tokenID))
	if err != nil {
		return "", "", errors.New("rewrite webhook key could not be opened")
	}
	return url, string(k), nil
}

// RecordWebhookTest stores a test result: ok, and a short detail such as
// "HTTP 500" or "timeout".
func (s *Store) RecordWebhookTest(ctx context.Context, u *User, tokenID int64, ok bool, detail string, ms int) error {
	if u == nil {
		return ErrNotFound
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE agent_webhooks SET tested_at = ?, test_ok = ?, test_detail = ?, test_ms = ? WHERE token_id = ?`, s.now(), b2i(ok), detail, ms, tokenID)
	return err
}

// ---- Requests ----

const rewriteCols = `id, draft_id, token_id, note, sent_at, expires_at, state, answered_at, COALESCE(revised_version, 0), dismissed`

func scanRewrite(r scanner) (*RewriteRequest, error) {
	var q RewriteRequest
	var sent, exp int64
	var ans sql.NullInt64
	var dis int
	if err := r.Scan(&q.ID, &q.DraftID, &q.TokenID, &q.Note, &sent, &exp, &q.State, &ans, &q.RevisedVersion, &dis); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	q.SentAt, q.ExpiresAt, q.AnsweredAt, q.Dismissed = time.Unix(sent, 0), time.Unix(exp, 0), tptr(ans), dis == 1
	return &q, nil
}

// LatestRewrite is the draft's newest request that reached the agent
// (failed sends are skipped), or ErrNotFound.
func (s *Store) LatestRewrite(ctx context.Context, draftID int64) (*RewriteRequest, error) {
	return s.latestRewrite(ctx, s.DB, draftID)
}

func (s *Store) latestRewrite(ctx context.Context, q querier, draftID int64) (*RewriteRequest, error) {
	return scanRewrite(q.QueryRowContext(ctx, `SELECT `+rewriteCols+` FROM rewrite_requests WHERE draft_id = ? AND state != 'failed' ORDER BY id DESC LIMIT 1`, draftID))
}

// RewriteRequests is the draft's full request history, oldest first.
func (s *Store) RewriteRequests(ctx context.Context, draftID int64) ([]*RewriteRequest, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+rewriteCols+` FROM rewrite_requests WHERE draft_id = ? ORDER BY id`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RewriteRequest
	for rows.Next() {
		q, err := scanRewrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// waitingRewrite is the draft's open, unexpired request, if any.
func (s *Store) waitingRewrite(ctx context.Context, q querier, draftID int64) *RewriteRequest {
	r, err := s.latestRewrite(ctx, q, draftID)
	if err != nil || !r.Waiting(s.Now()) {
		return nil
	}
	return r
}

// rewriteBlock is the conflict for approving or editing while the agent
// is rewriting.
func (s *Store) rewriteBlock(ctx context.Context, q querier, draftID int64) error {
	r := s.waitingRewrite(ctx, q, draftID)
	if r == nil {
		return nil
	}
	name := "the agent"
	if t, err := s.AgentTokenByID(ctx, r.TokenID); err == nil {
		name = t.Name
	}
	return &ConflictError{Msg: "Waiting on " + name + " for a rewrite. Cancel the request first."}
}

// StartRewrite opens a request for a pending agent draft. The caller then
// delivers it to the webhook and calls FailRewrite if that fails. It needs
// a live token with drafts:revise and a saved webhook, and no request
// still waiting.
func (s *Store) StartRewrite(ctx context.Context, u *User, id int64, version int, note string) (*RewriteRequest, error) {
	note, err := cleanNote(note, MaxRewriteNote)
	if err != nil {
		return nil, badInput("note: " + err.Error())
	}
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if r.status != StatusPending {
		return nil, ErrConflict
	}
	if r.kind == KindBlog {
		// Pending only, so no publish is underway; checked again in case a
		// publish row is still in flight.
		if p, err := s.LatestBlogPublish(ctx, id); err == nil && p.InFlight() {
			return nil, &ConflictError{Msg: "A publish is underway for this post. Rewrite is off until it finishes."}
		}
	}
	var tokID sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT token_id FROM drafts WHERE id = ?`, id).Scan(&tokID)
	if !tokID.Valid {
		return nil, badInput("You wrote this post, so there is no agent to send it back to.")
	}
	t, err := s.AgentTokenByID(ctx, tokID.Int64)
	if err != nil || t.RevokedAt != nil {
		return nil, badInput("The token that wrote this draft is revoked, so it can't rewrite it.")
	}
	if !t.Has(ScopeDraftsRevise) {
		return nil, badInput(t.Name + "'s token lacks drafts:revise, so it can't send a rewrite back.")
	}
	var n int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_webhooks WHERE token_id = ?`, t.ID).Scan(&n)
	if n == 0 {
		return nil, badInput("Rewrite is off for " + t.Name + ". Set up a rewrite webhook for " + t.Name + " in Settings.")
	}
	if w := s.waitingRewrite(ctx, tx, id); w != nil {
		return nil, &ConflictError{Msg: "Already waiting on " + t.Name + " for a rewrite."}
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `INSERT INTO rewrite_requests(draft_id, token_id, note, sent_at, expires_at, state) VALUES (?,?,?,?,?, 'open')`,
		id, t.ID, note, now, now+int64(RewriteTimeout/time.Second))
	if err != nil {
		return nil, err
	}
	rid, _ := res.LastInsertId()
	if err := event(ctx, tx, id, now, ownerActor, "sent for rewrite", "to "+t.Name); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return scanRewrite(s.DB.QueryRowContext(ctx, `SELECT `+rewriteCols+` FROM rewrite_requests WHERE id = ?`, rid))
}

// FailRewrite marks a request the webhook did not accept. detail is short
// ("HTTP 500", "timeout"); never a response body.
func (s *Store) FailRewrite(ctx context.Context, reqID int64, detail string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var draftID int64
	if err := tx.QueryRowContext(ctx, `SELECT draft_id FROM rewrite_requests WHERE id = ? AND state = 'open'`, reqID).Scan(&draftID); err != nil {
		return nil // already answered or cancelled
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET state = 'failed', answered_at = ? WHERE id = ?`, s.now(), reqID); err != nil {
		return err
	}
	if err := event(ctx, tx, draftID, s.now(), "xpost", "rewrite not delivered", detail); err != nil {
		return err
	}
	return tx.Commit()
}

// CancelRewrite cancels the draft's waiting request. A reply that comes
// later is refused.
func (s *Store) CancelRewrite(ctx context.Context, u *User, id int64) error {
	if u == nil {
		return ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	w := s.waitingRewrite(ctx, tx, id)
	if w == nil {
		return &ConflictError{Msg: "There is no rewrite request waiting for this draft."}
	}
	if err := cancelOpen(ctx, tx, id, s.now()); err != nil {
		return err
	}
	if err := event(ctx, tx, id, s.now(), ownerActor, "rewrite request cancelled", ""); err != nil {
		return err
	}
	return tx.Commit()
}

func cancelOpen(ctx context.Context, tx *sql.Tx, draftID, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET state = 'cancelled', answered_at = ? WHERE draft_id = ? AND state = 'open'`, now, draftID)
	return err
}

// replyTo checks an agent reply against its request: the newest request
// for this draft, still open, unexpired, and the draft still pending. A
// refused reply is recorded in the history. The caller owns tx.
func (s *Store) replyTo(ctx context.Context, tx *sql.Tx, tok *AgentToken, d *Draft, reqID int64) (*RewriteRequest, error) {
	id := d.ID
	req, err := scanRewrite(tx.QueryRowContext(ctx, `SELECT `+rewriteCols+` FROM rewrite_requests WHERE id = ? AND draft_id = ? AND token_id = ?`, reqID, id, tok.ID))
	if err != nil {
		return nil, &RewriteError{Msg: "no rewrite request " + strconv.FormatInt(reqID, 10) + " for this draft"}
	}
	now := s.Now()
	refuse := func(msg string) (*RewriteRequest, error) {
		tx.Rollback()
		s.DB.ExecContext(ctx, `INSERT INTO draft_events(draft_id, at, actor, kind, detail) VALUES (?,?,?,?,?)`, id, now.Unix(), "agent:"+tok.Name, "late rewrite reply refused", req.State)
		return nil, &RewriteError{Msg: msg}
	}
	if latest, err := s.latestRewrite(ctx, tx, id); err == nil && latest.ID != req.ID {
		return refuse("rewrite request " + strconv.FormatInt(reqID, 10) + " was replaced by a newer request (" + strconv.FormatInt(latest.ID, 10) + "); answer that one")
	}
	switch {
	case req.State == RewriteCancelled:
		return refuse("rewrite request " + strconv.FormatInt(reqID, 10) + " was cancelled by the owner; this reply was not applied")
	case req.TimedOut(now):
		return refuse("rewrite request " + strconv.FormatInt(reqID, 10) + " expired at " + req.ExpiresAt.UTC().Format(time.RFC3339) + " (60 minutes after it was sent); this reply was not applied")
	case req.State == RewriteFailed:
		return refuse("rewrite request " + strconv.FormatInt(reqID, 10) + " was not delivered; this reply was not applied")
	case req.State != RewriteOpen:
		return refuse("rewrite request " + strconv.FormatInt(reqID, 10) + " was already answered")
	}
	if d.Status != StatusPending {
		return refuse("this draft is no longer pending; this reply was not applied")
	}
	return req, nil
}

// replyAuthor is who wrote the version a rewrite replaces, and when: the
// agent (or the source) at creation or its last rewrite, unless the owner
// edited it after that.
func (s *Store) replyAuthor(ctx context.Context, tx *sql.Tx, tok *AgentToken, d *Draft) (string, int64) {
	author, authoredAt := d.Source, d.CreatedAt.Unix()
	var lastRev sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT MAX(answered_at) FROM rewrite_requests WHERE draft_id = ? AND state = 'revised'`, d.ID).Scan(&lastRev)
	if lastRev.Valid {
		authoredAt = lastRev.Int64
	}
	if d.TokenID != 0 {
		author = tok.Name
	}
	var lastEdit sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT MAX(at) FROM draft_events WHERE draft_id = ? AND actor = 'you' AND kind LIKE 'edited%'`, d.ID).Scan(&lastEdit)
	if lastEdit.Valid && lastEdit.Int64 >= authoredAt {
		author, authoredAt = ownerActor, lastEdit.Int64
	}
	return author, authoredAt
}

// answer closes the request as unchanged, or as revised at the draft's
// new version (bumped here), with its history event.
func answer(ctx context.Context, tx *sql.Tx, tok *AgentToken, id int64, req *RewriteRequest, now int64, revised bool) error {
	if !revised {
		if _, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET state = 'unchanged', answered_at = ? WHERE id = ?`, now, req.ID); err != nil {
			return err
		}
		return event(ctx, tx, id, now, "agent:"+tok.Name, "rewrite: no changes", "")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET updated_at = ?, version = version + 1 WHERE id = ? AND status = 'pending'`, now, id); err != nil {
		return err
	}
	var newV int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM drafts WHERE id = ?`, id).Scan(&newV); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET state = 'revised', answered_at = ?, revised_version = ? WHERE id = ?`, now, newV, req.ID); err != nil {
		return err
	}
	return event(ctx, tx, id, now, "agent:"+tok.Name, "revised", "")
}

// ReviseDraft is the agent's answer to an open request: new text for each
// part. The part count may change; images may not: every image stays on
// its part, so a part that carries images can't be dropped. Identical text
// leaves the draft as it was (result "unchanged"); otherwise the replaced
// version goes to draft_revisions, the draft gets the new parts and a new
// version, and stays pending (result "revised").
func (s *Store) ReviseDraft(ctx context.Context, tok *AgentToken, id, reqID int64, texts []string) (*Draft, string, error) {
	if tok == nil || !tok.Has(ScopeDraftsRevise) {
		return nil, "", ErrNotFound
	}
	d, err := s.Draft(ctx, id)
	if err != nil || d.TokenID != tok.ID {
		return nil, "", ErrNotFound
	}
	if d.IsBlog() {
		return nil, "", badInput(`this draft is a blog post: send "post" with title, description, tags and markdown instead of "parts"`)
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
	now := s.Now()
	// Images stay where they are.
	parts := make([]PartIn, len(texts))
	for i, t := range texts {
		parts[i].Text = t
	}
	for i, p := range d.Parts {
		if len(p.Media) == 0 {
			continue
		}
		if i >= len(parts) {
			return nil, "", badInput(fmt.Sprintf("part %d has images and a rewrite can't change images, so keep at least %d parts", i+1, i+1))
		}
		for _, m := range p.Media {
			parts[i].Media = append(parts[i].Media, MediaIn{ID: m.ID, Alt: m.Alt})
		}
	}
	// A reply stays one part with no images; over 280 is allowed here
	// (it blocks Approve, like an owner's edit).
	if d.IsReply() {
		if err := validateReplyParts(parts); err != nil {
			return nil, "", err
		}
	} else if err := validateParts(parts); err != nil {
		return nil, "", err
	}
	if !d.IsPromo() && HasBlogURL(parts) {
		return nil, "", badInput(BlogURLToken + " works only in a promo post")
	}
	same := len(parts) == len(d.Parts)
	for i := 0; same && i < len(parts); i++ {
		same = parts[i].Text == d.Parts[i].Text
	}
	nowU := now.Unix()
	if same {
		if err := answer(ctx, tx, tok, id, req, nowU, false); err != nil {
			return nil, "", err
		}
		if err := tx.Commit(); err != nil {
			return nil, "", err
		}
		d, err := s.Draft(ctx, id)
		return d, RewriteUnchanged, err
	}
	prev := make([]RevisionPart, len(d.Parts))
	for i, p := range d.Parts {
		prev[i] = RevisionPart{Text: p.Text, Images: len(p.Media)}
	}
	pj, _ := json.Marshal(prev)
	author, authoredAt := s.replyAuthor(ctx, tx, tok, d)
	var curV int
	tx.QueryRowContext(ctx, `SELECT version FROM drafts WHERE id = ?`, id).Scan(&curV)
	if _, err := tx.ExecContext(ctx, `INSERT INTO draft_revisions(draft_id, request_id, version, parts_json, author, authored_at, created_at) VALUES (?,?,?,?,?,?,?)`,
		id, req.ID, curV, string(pj), author, authoredAt, nowU); err != nil {
		return nil, "", err
	}
	if err := checkMediaMix(ctx, tx, parts); err != nil {
		return nil, "", err
	}
	if err := writeParts(ctx, tx, id, parts); err != nil {
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

// Revisions lists the versions rewrites replaced, newest first.
func (s *Store) Revisions(ctx context.Context, draftID int64) ([]Revision, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, COALESCE(request_id, 0), version, parts_json, post_json, author, authored_at, created_at FROM draft_revisions WHERE draft_id = ? ORDER BY id DESC`, draftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var v Revision
		var pj, bj string
		var a, c int64
		if err := rows.Scan(&v.ID, &v.RequestID, &v.Version, &pj, &bj, &v.Author, &a, &c); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(pj), &v.Parts); err != nil {
			return nil, err
		}
		if bj != "" {
			v.Post = &RevisionPost{}
			if err := json.Unmarshal([]byte(bj), v.Post); err != nil {
				return nil, err
			}
		}
		v.AuthoredAt, v.CreatedAt = time.Unix(a, 0), time.Unix(c, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}
