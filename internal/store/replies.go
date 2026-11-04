package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/weight"
)

// Replies. A reply is an X draft (kind 'x') with a draft_replies row: one
// part, no images, answering the post in_reply_to. xpost never posts a
// reply through the API. The owner approves it, opens x.com with the text
// filled in, presses Post there and marks it posted here, so the
// dispatcher, the 15 minute spacing, the Schedule and the spend estimate
// all skip replies. The original post's fields are what the agent sent;
// xpost never reads X for them.

// KindReply is the queue filter and the API kind for replies. In the
// database a reply stays kind 'x'.
const KindReply = "reply"

// NotMarkedAfter is how long after opening x.com a reply that is not
// marked posted shows "Not marked yet". It never reverts by itself.
const NotMarkedAfter = 24 * time.Hour

// MaxOrigText bounds the original post's stored text.
const MaxOrigText = 4000

// Reply settings an agent may report for the original post (X's
// reply_settings values). Anything but "everyone" limits who can reply.
var replySettings = map[string]bool{"": true, "everyone": true, "mentionedUsers": true, "following": true, "subscribers": true, "verified": true}

// ValidReplySettings reports whether v is a reply_settings value xpost
// knows ("" for unknown).
func ValidReplySettings(v string) bool { return replySettings[v] }

var replyHandleRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// ValidPostID reports whether id is an X post id: 1 to 30 digits.
func ValidPostID(id string) bool {
	return id != "" && len(id) <= 30 && strings.Trim(id, "0123456789") == ""
}

// Reply is a reply draft's original post and hand-off state.
type Reply struct {
	InReplyTo     string // the original post's id
	AuthorName    string
	AuthorHandle  string
	Text          string // the original post's text
	CreatedAt     *time.Time
	URL           string // the original's link as the agent sent it ("" when none)
	ReplySettings string
	Available     bool // false: the agent could not load the original; Approve is blocked
	TriedAt       time.Time
	OpenedAt      *time.Time // last time the owner opened x.com for it
	PostedURL     string     // the owner's link to the posted reply, "" when none
}

// Limited reports whether the original's author limits who can reply.
func (r *Reply) Limited() bool { return r.ReplySettings != "" && r.ReplySettings != "everyone" }

// NotMarked reports whether the reply was opened on X over a day ago and
// is still not marked posted.
func (r *Reply) NotMarked(now time.Time) bool {
	return r.OpenedAt != nil && now.Sub(*r.OpenedAt) >= NotMarkedAfter
}

// IsReply reports whether the draft is a reply.
func (d *Draft) IsReply() bool { return d.Reply != nil }

// NewReply is the reply half of an agent's proposal.
type NewReply struct {
	InReplyTo     string // digits; the API normalizes a status URL first
	AuthorName    string
	AuthorHandle  string
	Text          string
	CreatedAt     *time.Time
	URL           string
	ReplySettings string
	Unavailable   bool // the agent could not load the original
}

func (nr *NewReply) clean() error {
	nr.InReplyTo = strings.TrimSpace(nr.InReplyTo)
	if !ValidPostID(nr.InReplyTo) {
		return badInput("in_reply_to must be a post id (digits) or an x.com status link")
	}
	nr.AuthorHandle = strings.TrimPrefix(strings.TrimSpace(nr.AuthorHandle), "@")
	nr.AuthorName = strings.Join(strings.Fields(nr.AuthorName), " ")
	nr.Text = strings.TrimSpace(nr.Text)
	if nr.AuthorHandle != "" && !replyHandleRE.MatchString(nr.AuthorHandle) {
		return badInput("original.author_handle must be 1 to 15 letters, digits or _")
	}
	if len([]rune(nr.AuthorName)) > 100 {
		return badInput("original.author_name is too long")
	}
	if len(nr.Text) > MaxOrigText {
		return badInput("original.text is too long")
	}
	if !ValidReplySettings(nr.ReplySettings) {
		return badInput(`reply_settings must be one of everyone, mentionedUsers, following, subscribers, verified`)
	}
	if nr.Unavailable || nr.AuthorHandle == "" || nr.Text == "" {
		// Nothing usable to show: keep only the id.
		nr.Unavailable = true
		nr.AuthorName, nr.AuthorHandle, nr.Text, nr.CreatedAt, nr.URL = "", "", "", nil, ""
	}
	return nil
}

// validateReplyParts checks a reply's text: one part, no images, not
// empty. The 280 limit blocks Approve, not intake or Save, so the owner
// can shorten it here.
func validateReplyParts(parts []PartIn) error {
	if len(parts) != 1 {
		return badInput("a reply has exactly one part")
	}
	p := parts[0]
	if len(p.Media) > 0 {
		return badInput("a reply can't have images")
	}
	if strings.TrimSpace(p.Text) == "" {
		return badInput("the reply is empty")
	}
	if len(p.Text) > 4000 {
		return badInput("the reply is too long")
	}
	return nil
}

func insertReply(ctx context.Context, tx *sql.Tx, id int64, nr *NewReply, now int64) error {
	var created any
	if nr.CreatedAt != nil {
		created = nr.CreatedAt.Unix()
	}
	avail := 1
	if nr.Unavailable {
		avail = 0
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO draft_replies(draft_id, in_reply_to, author_name, author_handle, orig_text, orig_created_at, orig_url, reply_settings, available, tried_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, id, nr.InReplyTo, nr.AuthorName, nr.AuthorHandle, nr.Text, created, nr.URL, nr.ReplySettings, avail, now)
	return err
}

func (s *Store) loadReply(ctx context.Context, d *Draft) error {
	var r Reply
	var created, opened sql.NullInt64
	var tried int64
	err := s.DB.QueryRowContext(ctx, `SELECT in_reply_to, author_name, author_handle, orig_text, orig_created_at, orig_url, reply_settings, available, tried_at, opened_at, posted_url
		FROM draft_replies WHERE draft_id = ?`, d.ID).Scan(&r.InReplyTo, &r.AuthorName, &r.AuthorHandle, &r.Text, &created, &r.URL, &r.ReplySettings, &r.Available, &tried, &opened, &r.PostedURL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	r.CreatedAt, r.OpenedAt, r.TriedAt = tptr(created), tptr(opened), time.Unix(tried, 0)
	d.Reply = &r
	return nil
}

func isReply(ctx context.Context, q querier, id int64) bool {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM draft_replies WHERE draft_id = ?`, id).Scan(&n)
	return n == 1
}

// ReplyRules are the lint rules for replies on an X account: the banned
// topics and dash rules of every blog that promotes on that account,
// strictest level wins. With no such blog, the defaults (em dash blocks,
// en dash warns, no topics).
func (s *Store) ReplyRules(ctx context.Context, account string) blogtext.Rules {
	r := blogtext.Rules{EnDash: blogtext.Warn, Banned: blogtext.Block}
	bs, _ := s.Blogs(ctx)
	seen := map[string]bool{}
	for _, b := range bs {
		if b.PromoAccount == "" || !strings.EqualFold(b.PromoAccount, account) {
			continue
		}
		if b.Rules.EnDash == blogtext.Block {
			r.EnDash = blogtext.Block
		}
		for _, t := range b.Rules.Topics {
			if k := strings.ToLower(t); !seen[k] {
				seen[k] = true
				r.Topics = append(r.Topics, t)
			}
		}
	}
	return r
}

// LintReply runs the account's lint on a reply's text. Only text rules
// apply (dashes and banned topics); every finding is in "the reply".
func (s *Store) LintReply(ctx context.Context, account, text string) []blogtext.Finding {
	var out []blogtext.Finding
	for _, f := range blogtext.Lint("", "", nil, text, s.ReplyRules(ctx, account)) {
		switch f.Rule {
		case blogtext.RuleEmDash, blogtext.RuleEnDash, blogtext.RuleBanned:
		default:
			continue
		}
		f.Msg = strings.ReplaceAll(f.Msg, f.Where, "the reply")
		f.Where = "the reply"
		out = append(out, f)
	}
	return out
}

func replyText(d *Draft) string {
	if len(d.Parts) == 0 {
		return ""
	}
	return d.Parts[0].Text
}

// ReplyBlock is why a reply can't be approved, or "".
func (s *Store) ReplyBlock(ctx context.Context, d *Draft) string {
	if d.Reply == nil {
		return ""
	}
	if !d.Reply.Available {
		return "Can't approve: the original post didn't load. Retry, or reject this reply."
	}
	if c := weight.Count(replyText(d)); c.Weighted > weight.Limit {
		return fmt.Sprintf("Shorten by %d to approve.", c.Weighted-weight.Limit)
	}
	for _, f := range s.LintReply(ctx, d.Account, replyText(d)) {
		if f.Blocks() {
			return "Can't approve: " + f.Msg
		}
	}
	return ""
}

// ReplyDuplicates counts the account's other replies to the same post
// that are not rejected. It warns; it never blocks.
func (s *Store) ReplyDuplicates(ctx context.Context, d *Draft) int {
	if d.Reply == nil {
		return 0
	}
	var n int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM draft_replies r JOIN drafts d ON d.id = r.draft_id
		WHERE r.in_reply_to = ? AND d.account = ? AND d.id != ? AND d.status != 'rejected'`, d.Reply.InReplyTo, d.Account, d.ID).Scan(&n)
	return n
}

// approveReply approves a pending reply: no time, no spacing, no API. It
// is blocked while the original is unavailable, the text is over 280 or a
// lint error remains.
func (s *Store) approveReply(ctx context.Context, tx *sql.Tx, id int64) error {
	if err := s.rewriteBlock(ctx, tx, id); err != nil {
		return err
	}
	d, err := s.Draft(ctx, id)
	if err != nil {
		return err
	}
	if msg := s.ReplyBlock(ctx, d); msg != "" {
		return &ConflictError{Msg: msg}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'approved', approved_at = ?, updated_at = ?, version = version + 1, reject_reason = '' WHERE id = ?`, now, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET opened_at = NULL WHERE draft_id = ?`, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "approved", "posts by hand on X"); err != nil {
		return err
	}
	return tx.Commit()
}

// beginReply opens a change to one of the owner's replies.
func (s *Store) beginReply(ctx context.Context, u *User, id int64, version int) (*sql.Tx, *draftRow, error) {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return nil, nil, err
	}
	if !isReply(ctx, tx, id) {
		tx.Rollback()
		return nil, nil, ErrConflict
	}
	return tx, r, nil
}

// OpenedOnX records that the owner opened x.com to post an approved reply.
// It sends nothing to X.
func (s *Store) OpenedOnX(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.beginReply(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusApproved {
		return ErrConflict
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET opened_at = ? WHERE draft_id = ?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET updated_at = ?, version = version + 1 WHERE id = ?`, now, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "opened on X", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// Unapprove sends an approved reply back to Pending (Unapprove, and Put
// back in queue after opening x.com). The opened time is cleared.
func (s *Store) Unapprove(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.beginReply(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusApproved {
		return ErrConflict
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'pending', approved_at = NULL, updated_at = ?, version = version + 1 WHERE id = ?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET opened_at = NULL WHERE draft_id = ?`, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "back to pending", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkReplyPosted records that the owner posted an opened reply on X by
// hand. link is "" or the normalized link to the reply (the caller checks
// its format; xpost never reads X to confirm it). No post_log row is
// written: nothing went through the API.
func (s *Store) MarkReplyPosted(ctx context.Context, u *User, id int64, version int, link string) error {
	tx, r, err := s.beginReply(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusApproved {
		return ErrConflict
	}
	var opened sql.NullInt64
	tx.QueryRowContext(ctx, `SELECT opened_at FROM draft_replies WHERE draft_id = ?`, id).Scan(&opened)
	if !opened.Valid {
		return &ConflictError{Msg: "Open it on X first, then mark it posted."}
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET posted_url = ? WHERE draft_id = ?`, link, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'posted', updated_at = ?, version = version + 1 WHERE id = ?`, now, id); err != nil {
		return err
	}
	detail := "no link"
	if link != "" {
		detail = link
	}
	if err := event(ctx, tx, id, now, ownerActor, "posted by you on X", detail); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryReplyLoad re-reads the original post for a pending reply whose
// original was unavailable. It reads only what agents already sent: the
// newest available copy of the same post on any stored reply. It never
// calls X, so it costs nothing. ok reports whether the original loaded.
func (s *Store) RetryReplyLoad(ctx context.Context, u *User, id int64, version int) (bool, error) {
	tx, r, err := s.beginReply(ctx, u, id, version)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if r.status != StatusPending {
		return false, ErrConflict
	}
	now := s.now()
	res, err := tx.ExecContext(ctx, `UPDATE draft_replies AS t SET (author_name, author_handle, orig_text, orig_created_at, orig_url, reply_settings, available) =
		(SELECT o.author_name, o.author_handle, o.orig_text, o.orig_created_at, o.orig_url, o.reply_settings, 1 FROM draft_replies o
			WHERE o.in_reply_to = t.in_reply_to AND o.available = 1 AND o.draft_id != t.draft_id ORDER BY o.tried_at DESC, o.draft_id DESC LIMIT 1)
		WHERE t.draft_id = ? AND t.available = 0 AND EXISTS (SELECT 1 FROM draft_replies o WHERE o.in_reply_to = t.in_reply_to AND o.available = 1 AND o.draft_id != t.draft_id)`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET tried_at = ? WHERE draft_id = ?`, now, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET updated_at = ?, version = version + 1 WHERE id = ?`, now, id); err != nil {
		return false, err
	}
	detail := "still unavailable"
	if n == 1 {
		detail = "loaded"
	}
	if err := event(ctx, tx, id, now, ownerActor, "retried loading the original", detail); err != nil {
		return false, err
	}
	var avail bool
	tx.QueryRowContext(ctx, `SELECT available FROM draft_replies WHERE draft_id = ?`, id).Scan(&avail)
	return avail, tx.Commit()
}

// HasReplies reports whether any reply exists (the queue then shows its
// Replies filter).
func (s *Store) HasReplies(ctx context.Context) bool {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM draft_replies)`).Scan(&n)
	return n == 1
}
