package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The owner's review actions. Each takes the owner's *User (only a full,
// password + passkey session yields one) and the draft version the owner
// was looking at, so a click never applies to content they did not see.

const ownerActor = "you"

type draftRow struct {
	kind    string
	status  string
	version int
	account string
	when    int64
}

func (s *Store) begin(ctx context.Context, u *User, id int64, version int) (*sql.Tx, *draftRow, error) {
	if u == nil {
		return nil, nil, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	var r draftRow
	if err := tx.QueryRowContext(ctx, `SELECT kind, status, version, account, scheduled_at FROM drafts WHERE id = ?`, id).Scan(&r.kind, &r.status, &r.version, &r.account, &r.when); err != nil {
		tx.Rollback()
		return nil, nil, ErrNotFound
	}
	if version != r.version {
		tx.Rollback()
		return nil, nil, ErrStale
	}
	return tx, &r, nil
}

// changeable reports whether the owner may reschedule or reject the
// draft: pending or approved, or a blog post whose publish failed.
func (r *draftRow) changeable() bool {
	return r.status == StatusPending || r.status == StatusApproved || r.kind == KindBlog && r.status == StatusFailed
}

// checkApprovable refuses times in the past and times within MinSpacing of
// another approved (or posting/posted) item on the same account.
func (s *Store) checkApprovable(ctx context.Context, q querier, id int64, account string, when time.Time) error {
	if !when.After(s.Now()) {
		return &ConflictError{Msg: "That time has passed. Reschedule it first.", NextFree: s.nextFree(ctx, q, account, s.Now(), id)}
	}
	if strings.HasPrefix(account, BlogAccountPrefix) {
		return nil // the 15 minute spacing is an X rule
	}
	for _, n := range s.neighbors(ctx, q, account, when, id) {
		if n.Status != StatusPending {
			gap := n.At.Sub(when)
			if gap < 0 {
				gap = -gap
			}
			return &ConflictError{
				Msg:      fmt.Sprintf("Only %d min from an approved @%s post. Minimum is %d min.", int(gap.Minutes()), account, int(MinSpacing.Minutes())),
				NextFree: s.nextFree(ctx, q, account, when, id),
			}
		}
	}
	return nil
}

// Approve approves a pending draft at its current time.
func (s *Store) Approve(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status != StatusPending {
		return ErrConflict
	}
	if r.kind == KindX && isReply(ctx, tx, id) {
		return s.approveReply(ctx, tx, id)
	}
	if err := s.rewriteBlock(ctx, tx, id); err != nil {
		return err
	}
	if r.kind == KindBlog {
		if err := s.blogApprovable(ctx, id); err != nil {
			return err
		}
	} else if err := s.promoApprovable(ctx, tx, id); err != nil {
		return err
	}
	if err := s.checkApprovable(ctx, tx, id, r.account, time.Unix(r.when, 0)); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'approved', approved_at = ?, updated_at = ?, version = version + 1, reject_reason = '', promo_returned = 0 WHERE id = ?`, now, now, id); err != nil {
		return err
	}
	if err := event(ctx, tx, id, now, ownerActor, "approved", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// Edit is the owner's change to a draft's content and time.
type Edit struct {
	Parts []PartIn // media by ID
	When  time.Time
}

// Save replaces a pending or approved draft's parts and time. Saving an
// approved draft sends it back to pending unless approve is set ("Save and
// approve"), which runs the same checks as Approve. A reply keeps its
// time, may be saved over 280 (the limit blocks Approve), always goes back
// to pending and has no Save and approve.
func (s *Store) Save(ctx context.Context, u *User, id int64, version int, e Edit, approve bool) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.kind != KindX || r.status != StatusPending && r.status != StatusApproved {
		return ErrConflict // blog posts are edited with SaveBlogPost
	}
	reply := isReply(ctx, tx, id)
	if reply {
		if approve {
			return &ConflictError{Msg: "Save it, then approve the reply from the queue."}
		}
		if err := validateReplyParts(e.Parts); err != nil {
			return err
		}
		e.When = time.Unix(r.when, 0)
		if _, err := tx.ExecContext(ctx, `UPDATE draft_replies SET opened_at = NULL WHERE draft_id = ?`, id); err != nil {
			return err
		}
	} else if err := validateParts(e.Parts); err != nil {
		return err
	}
	if HasBlogURL(e.Parts) && promoCountOf(ctx, tx, id) == 0 {
		return badInput(BlogURLToken + " works only in a promo post")
	}
	if err := s.rewriteBlock(ctx, tx, id); err != nil {
		return err
	}
	// The owner's edit ends what the card shows about past rewrites.
	if _, err := tx.ExecContext(ctx, `UPDATE rewrite_requests SET dismissed = 1 WHERE draft_id = ?`, id); err != nil {
		return err
	}
	for i, p := range e.Parts {
		for _, m := range p.Media {
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM media WHERE id = ?`, m.ID).Scan(&n)
			if n != 1 {
				return badInput(fmt.Sprintf("part %d: unknown image", i+1))
			}
		}
	}
	if err := checkMediaMix(ctx, tx, e.Parts); err != nil {
		return err
	}
	if err := writeParts(ctx, tx, id, e.Parts); err != nil {
		return err
	}
	return s.finishChange(ctx, tx, id, r, e.When, approve, "edited")
}

// Reschedule moves a pending or approved draft (or a failed blog post).
// Moving an approved draft sends it back to pending unless approve is set.
func (s *Store) Reschedule(ctx context.Context, u *User, id int64, version int, when time.Time, approve bool) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !r.changeable() {
		return ErrConflict
	}
	if r.kind == KindX && isReply(ctx, tx, id) {
		return &ConflictError{Msg: "A reply has no time to move: you post it by hand on X."}
	}
	return s.finishChange(ctx, tx, id, r, when, approve, "rescheduled")
}

func (s *Store) finishChange(ctx context.Context, tx *sql.Tx, id int64, r *draftRow, when time.Time, approve bool, kind string) error {
	if when.IsZero() {
		return badInput("pick a date and time")
	}
	now := s.now()
	status, appr := StatusPending, any(nil)
	detail := ""
	if when.Unix() != r.when {
		detail = "to " + when.UTC().Format(time.RFC3339)
	}
	if approve {
		if err := s.rewriteBlock(ctx, tx, id); err != nil {
			return err
		}
		if r.kind == KindBlog {
			if err := s.blogApprovable(ctx, id); err != nil {
				return err
			}
		} else if err := s.promoApprovable(ctx, tx, id); err != nil {
			return err
		}
		if err := s.checkApprovable(ctx, tx, id, r.account, when); err != nil {
			return err
		}
		status, appr = StatusApproved, now
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = ?, scheduled_at = ?, approved_at = ?, updated_at = ?, version = version + 1, error = CASE WHEN status = 'failed' THEN '' ELSE error END,
		promo_returned = CASE WHEN ? THEN 0 ELSE promo_returned END WHERE id = ?`,
		status, when.Unix(), appr, now, approve, id); err != nil {
		return err
	}
	switch {
	case r.kind == KindBlog:
		// Offset promos move with the blog.
		if err := syncPromos(ctx, tx, id, when, now); err != nil {
			return err
		}
	case when.Unix() != r.when:
		// A promo the owner moves keeps its new time: it is fixed now.
		if _, err := tx.ExecContext(ctx, `UPDATE drafts SET promo_offset = NULL WHERE id = ?`, id); err != nil {
			return err
		}
	}
	switch {
	case approve:
		kind += " and approved"
	case r.status == StatusApproved || r.status == StatusFailed:
		kind += "; back to pending"
	}
	if err := event(ctx, tx, id, now, ownerActor, kind, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// Reject rejects a pending or approved draft with an optional reason the
// proposing agent can read.
func (s *Store) Reject(ctx context.Context, u *User, id int64, version int, reason string) error {
	reason, err := cleanNote(reason, MaxReasonLen)
	if err != nil {
		return badInput("reason: " + err.Error())
	}
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !r.changeable() {
		return ErrConflict
	}
	now := s.now()
	// Rejecting also cancels a rewrite request; a later reply is refused.
	if err := cancelOpen(ctx, tx, id, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'rejected', reject_reason = ?, approved_at = NULL, updated_at = ?, version = version + 1 WHERE id = ?`, reason, now, id); err != nil {
		return err
	}
	if r.kind == KindBlog {
		if err := returnPromos(ctx, tx, id, now, "blog post rejected"); err != nil {
			return err
		}
	}
	if err := event(ctx, tx, id, now, ownerActor, "rejected", reason); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a draft that is not posting or posted.
func (s *Store) Delete(ctx context.Context, u *User, id int64, version int) error {
	tx, r, err := s.begin(ctx, u, id, version)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if r.status == StatusPosting || r.status == StatusPosted {
		return ErrConflict
	}
	if n := promoCount(ctx, tx, id); n > 0 {
		return &ConflictError{Msg: fmt.Sprintf("%d promo post(s) link to this blog post. Reject it instead, or delete them first.", n)}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM drafts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- Spacing ----

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// Neighbor is another draft on the same account within MinSpacing.
type Neighbor struct {
	ID     int64
	At     time.Time
	Status string
}

func (s *Store) neighbors(ctx context.Context, q querier, account string, when time.Time, exclude int64) []Neighbor {
	w := int64(MinSpacing / time.Second)
	// Replies are not spaced: they post by hand on X, not on a schedule.
	rows, err := q.QueryContext(ctx, `SELECT id, scheduled_at, status FROM drafts WHERE account = ? AND id != ? AND status IN ('pending','approved','scheduled','posting','posted')
		AND `+notReply+` AND scheduled_at > ? AND scheduled_at < ? ORDER BY scheduled_at`, account, exclude, when.Unix()-w, when.Unix()+w)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Neighbor
	for rows.Next() {
		var n Neighbor
		var at int64
		if rows.Scan(&n.ID, &at, &n.Status) == nil {
			n.At = time.Unix(at, 0)
			out = append(out, n)
		}
	}
	return out
}

// Neighbors lists drafts on account within MinSpacing of when.
func (s *Store) Neighbors(ctx context.Context, account string, when time.Time, exclude int64) []Neighbor {
	return s.neighbors(ctx, s.DB, account, when, exclude)
}

func (s *Store) nextFree(ctx context.Context, q querier, account string, from time.Time, exclude int64) time.Time {
	step := int64(SlotStep / time.Second)
	t := (from.Unix() + step - 1) / step * step
	if t <= s.now() {
		t = (s.now()/step + 1) * step
	}
	for i := 0; i < 7*96; i++ {
		c := time.Unix(t, 0)
		if len(s.neighbors(ctx, q, account, c, exclude)) == 0 {
			return c
		}
		t += step
	}
	return time.Time{}
}

// NextFree is the first 15-minute slot at or after from with no other
// draft on account within MinSpacing.
func (s *Store) NextFree(ctx context.Context, account string, from time.Time, exclude int64) time.Time {
	return s.nextFree(ctx, s.DB, account, from, exclude)
}
