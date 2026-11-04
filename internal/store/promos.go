package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/scott/xpost/internal/weight"
)

// Promo posts: X drafts that link to a blog draft (drafts.promotes). The
// text may carry BlogURLToken, which counts as a link (23) and is replaced
// by the blog post's live URL when the promo posts. A promo never posts
// before its blog post is published.

// BlogURLToken stands for the promoted blog post's live URL.
const BlogURLToken = weight.BlogURL

// DefaultPromoOffset is how long after the blog a promo posts by default.
const DefaultPromoOffset = 2 * time.Hour

// MaxPromoOffset bounds promo_offset_minutes.
const MaxPromoOffset = 7 * 24 * time.Hour

// PromoReturnAfter is how long an approved promo may wait past its due
// time for its blog before it goes back to pending.
const PromoReturnAfter = 24 * time.Hour

// Promo states, as the queue shows them.
const (
	PromoEarly   = "early"   // approved, the blog is not live yet and the promo is not due
	PromoWaiting = "waiting" // approved and due, the blog is not live yet
	PromoHeld    = "held"    // approved, the blog's publish failed
	PromoReady   = "ready"   // approved and the blog is live: posts when due
)

// HasBlogURL reports whether any part uses BlogURLToken.
func HasBlogURL(parts []PartIn) bool {
	for _, p := range parts {
		if strings.Contains(p.Text, BlogURLToken) {
			return true
		}
	}
	return false
}

func promoCount(ctx context.Context, q querier, blogID int64) int {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM drafts WHERE promotes = ?`, blogID).Scan(&n)
	return n
}

// promoCountOf is 1 when the draft is a promo.
func promoCountOf(ctx context.Context, q querier, id int64) int {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM drafts WHERE id = ? AND promotes IS NOT NULL`, id).Scan(&n)
	return n
}

// promoTime checks a new promo's link and works out its time: the blog's
// publish time plus the offset (default DefaultPromoOffset), or the
// proposed time when it is fixed.
func (s *Store) promoTime(ctx context.Context, tok *AgentToken, nd *NewDraft) (time.Time, error) {
	var kind, status string
	var tokID, at int64
	err := s.DB.QueryRowContext(ctx, `SELECT kind, status, COALESCE(token_id, 0), scheduled_at FROM drafts WHERE id = ?`, nd.Promotes).Scan(&kind, &status, &tokID, &at)
	if err != nil || kind != KindBlog || tokID != tok.ID {
		return time.Time{}, badInput(fmt.Sprintf("promotes: no blog post %d of yours", nd.Promotes))
	}
	if status == StatusRejected {
		return time.Time{}, badInput(fmt.Sprintf("promotes: blog post %d was rejected", nd.Promotes))
	}
	fixed := !nd.ProposedAt.IsZero()
	if fixed && nd.PromoOffset != nil {
		return time.Time{}, badInput("send proposed_at (a fixed time) or promo_offset_minutes, not both")
	}
	if fixed {
		return nd.ProposedAt, nil
	}
	if status == StatusPosted {
		return time.Time{}, badInput("promotes: that blog post is already published; send proposed_at")
	}
	off := int(DefaultPromoOffset / time.Minute)
	if nd.PromoOffset != nil {
		off = *nd.PromoOffset
	}
	if off < 0 || time.Duration(off)*time.Minute > MaxPromoOffset {
		return time.Time{}, badInput(fmt.Sprintf("promo_offset_minutes: 0 to %d", int(MaxPromoOffset/time.Minute)))
	}
	nd.PromoOffset = &off
	return time.Unix(at, 0).Add(time.Duration(off) * time.Minute), nil
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// syncPromos moves the blog's offset promos (pending or approved) to base
// plus their offset. Approved promos stay approved: they still wait for
// the blog.
func syncPromos(ctx context.Context, ex execer, blogID int64, base time.Time, now int64) error {
	_, err := ex.ExecContext(ctx, `UPDATE drafts SET scheduled_at = ? + promo_offset * 60, updated_at = ?
		WHERE promotes = ? AND promo_offset IS NOT NULL AND status IN ('pending','approved')`, base.Unix(), now, blogID)
	return err
}

// returnPromos sends the blog's approved promos back to pending (the blog
// was rejected), and marks pending ones too, so the card says why.
func returnPromos(ctx context.Context, tx *sql.Tx, blogID int64, now int64, why string) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM drafts WHERE promotes = ? AND status IN ('pending','approved')`, blogID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'pending', approved_at = NULL, promo_returned = 1, updated_at = ?, version = version + 1 WHERE id = ?`, now, id); err != nil {
			return err
		}
		if err := event(ctx, tx, id, now, "scheduler", "back to pending", why); err != nil {
			return err
		}
	}
	return nil
}

// ExpirePromos sends approved promos back to pending when their blog post
// is still not live PromoReturnAfter past the promo's due time. It returns
// how many it moved.
func (s *Store) ExpirePromos(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Not live: not published, or published with no live URL recorded.
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM drafts p JOIN drafts b ON b.id = p.promotes LEFT JOIN blog_posts bp ON bp.draft_id = b.id
		WHERE p.status = 'approved' AND (b.status != 'posted' OR COALESCE(bp.published_url, '') = '') AND p.scheduled_at + ? <= ?`, int64(PromoReturnAfter/time.Second), now.Unix())
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	n := now.Unix()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'pending', approved_at = NULL, promo_returned = 1, updated_at = ?, version = version + 1 WHERE id = ? AND status = 'approved'`, n, id); err != nil {
			return 0, err
		}
		if err := event(ctx, tx, id, n, "scheduler", "back to pending", "blog not live 24h after this promo was due"); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}

// promoApprovable refuses approving a promo whose blog post is rejected
// or gone.
func (s *Store) promoApprovable(ctx context.Context, q querier, id int64) error {
	var blog sql.NullInt64
	q.QueryRowContext(ctx, `SELECT promotes FROM drafts WHERE id = ?`, id).Scan(&blog)
	if !blog.Valid {
		return nil
	}
	var status string
	if q.QueryRowContext(ctx, `SELECT status FROM drafts WHERE id = ?`, blog.Int64).Scan(&status) != nil || status == StatusRejected {
		return &ConflictError{Msg: "Its blog post was rejected, so this promo can't be approved. Reject or delete it."}
	}
	return nil
}

// PromoState is how an approved promo stands against its blog (one of the
// Promo* states), or "" when the draft is not an approved promo.
func PromoState(p, blog *Draft, now time.Time) string {
	if p == nil || !p.IsPromo() || p.Status != StatusApproved || blog == nil {
		return ""
	}
	switch {
	case blog.Status == StatusPosted:
		return PromoReady
	case blog.Status == StatusFailed:
		return PromoHeld
	case !p.ScheduledAt.After(now):
		return PromoWaiting
	}
	return PromoEarly
}

// Promos lists the drafts promoting a blog draft, by time.
func (s *Store) Promos(ctx context.Context, blogID int64) ([]*Draft, error) {
	return s.draftsByQuery(ctx, `SELECT id FROM drafts WHERE promotes = ? ORDER BY scheduled_at, id`, blogID)
}

// PromoText returns the promo's parts with BlogURLToken replaced by the
// blog post's live URL. ok is false while the blog is not published.
func (s *Store) PromoText(ctx context.Context, d *Draft) (parts []Part, ok bool, err error) {
	if !d.IsPromo() {
		return d.Parts, true, nil
	}
	var status, url string
	err = s.DB.QueryRowContext(ctx, `SELECT d.status, COALESCE(bp.published_url, '') FROM drafts d LEFT JOIN blog_posts bp ON bp.draft_id = d.id WHERE d.id = ?`, d.Promotes).Scan(&status, &url)
	if err != nil {
		return nil, false, err
	}
	if status != StatusPosted || url == "" {
		return nil, false, nil
	}
	out := make([]Part, len(d.Parts))
	for i, p := range d.Parts {
		p.Text = strings.ReplaceAll(p.Text, BlogURLToken, url)
		out[i] = p
	}
	return out, true, nil
}
