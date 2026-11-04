package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/scott/xpost/internal/weight"
)

// OwnerSource is the source shown for drafts the owner wrote in the
// composer (token_id is NULL for them).
const OwnerSource = "you"

// OwnerDraft is a post the owner wrote in the composer.
type OwnerDraft struct {
	Account string
	Parts   []PartIn // media by ID; only images the owner uploaded
	When    time.Time
	Key     string // the composer form's nonce: a resubmit returns the first draft
}

// PartsEstimate is the cost the dispatcher counts against the cap for parts
// that have not been sent: each part at the post or link rate, plus one
// upload per image and one metadata call per alt text. It uses the same
// PartCost, UploadCost and AltCost the dispatcher sums, so an unset rate
// counts as 0 here; callers must check Rates.Known, UploadKnown and
// AltKnown (or Spend.PostingBlock and MediaBlocks) and fail closed.
func (s *Store) PartsEstimate(parts []Part) int64 {
	var c int64
	images, alts := 0, 0
	for _, p := range parts {
		c += s.PartCost(weight.HasURL(p.Text))
		images += len(p.Media)
		for _, m := range p.Media {
			if strings.TrimSpace(m.Alt) != "" {
				alts++
			}
		}
	}
	return c + s.UploadCost(images) + s.AltCost(alts)
}

// ApprovedBetween estimates every approved draft scheduled in [from, to):
// posts already on the schedule that will spend from the same month.
func (s *Store) ApprovedBetween(ctx context.Context, from, to time.Time) (int64, error) {
	ds, err := s.Between(ctx, from, to)
	if err != nil {
		return 0, err
	}
	var c int64
	for _, d := range ds {
		if d.Status == StatusApproved || d.Status == StatusScheduled {
			c += s.PartsEstimate(d.Parts)
		}
	}
	return c, nil
}

// LastOwnerAccount is the account of the owner's most recent composer
// draft, or "" when there is none.
func (s *Store) LastOwnerAccount(ctx context.Context) string {
	var a string
	s.DB.QueryRowContext(ctx, `SELECT account FROM drafts WHERE token_id IS NULL AND source = ? ORDER BY created_at DESC, id DESC LIMIT 1`, OwnerSource).Scan(&a)
	return a
}

// OwnerDraftByKey is the owner draft a composer form (by its key) already
// created, or nil.
func (s *Store) OwnerDraftByKey(ctx context.Context, key string) *Draft {
	if key == "" {
		return nil
	}
	var id int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM drafts WHERE token_id IS NULL AND idempotency_key = ?`, key).Scan(&id); err != nil {
		return nil
	}
	d, err := s.Draft(ctx, id)
	if err != nil {
		return nil
	}
	return d
}

// CreateOwnerDraft stores a post the owner wrote. With approve it is
// created approved ("Schedule"): the time must be in the future and at
// least MinSpacing from every other item on the account, pending ones
// included. Without approve it is created pending ("Save as draft") at the
// chosen time, spacing conflicts allowed. The spend cap is not checked
// here (the web layer refuses Schedule over the cap, and the dispatcher
// enforces it again before posting). A Key the owner already used returns
// that draft with created=false.
func (s *Store) CreateOwnerDraft(ctx context.Context, u *User, od OwnerDraft, approve bool) (*Draft, bool, error) {
	if u == nil {
		return nil, false, ErrNotFound
	}
	od.Key = strings.TrimSpace(od.Key)
	if len(od.Key) > 200 {
		return nil, false, badInput("form key is too long")
	}
	if d := s.OwnerDraftByKey(ctx, od.Key); d != nil {
		return d, false, nil
	}
	if !s.accountExists(ctx, od.Account) {
		return nil, false, badInput("unknown account " + od.Account)
	}
	if od.When.IsZero() {
		return nil, false, badInput("pick a date and time")
	}
	if !od.When.After(s.Now()) {
		return nil, false, badInput("that time has passed; pick a time in the future")
	}
	if err := validateParts(od.Parts); err != nil {
		return nil, false, err
	}
	if HasBlogURL(od.Parts) {
		return nil, false, badInput(BlogURLToken + " works only in a promo post")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	// The owner attaches only images the owner uploaded (token_id NULL).
	for i, p := range od.Parts {
		for _, m := range p.Media {
			var n int
			tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM media WHERE id = ? AND token_id IS NULL`, m.ID).Scan(&n)
			if n != 1 {
				return nil, false, badInput(fmt.Sprintf("part %d: unknown image", i+1))
			}
		}
	}
	if err := checkMediaMix(ctx, tx, od.Parts); err != nil {
		return nil, false, err
	}
	status, appr := StatusPending, any(nil)
	now := s.now()
	if approve {
		// Same rule as approving: only approved (or posting, posted) neighbours
		// block. A pending neighbour stays the owner's to move or reject.
		if err := s.checkApprovable(ctx, tx, 0, od.Account, od.When); err != nil {
			return nil, false, err
		}
		status, appr = StatusApproved, now
	}
	when := od.When.Unix()
	res, err := tx.ExecContext(ctx, `INSERT INTO drafts(account, status, proposed_at, scheduled_at, token_id, source, idempotency_key, created_at, updated_at, approved_at)
		VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?)`, od.Account, status, when, when, OwnerSource, od.Key, now, now, appr)
	if err != nil {
		return nil, false, err
	}
	id, _ := res.LastInsertId()
	if err := writeParts(ctx, tx, id, od.Parts); err != nil {
		return nil, false, err
	}
	kind := "written"
	if approve {
		kind = "written and approved"
	}
	if err := event(ctx, tx, id, now, ownerActor, kind, ""); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	d, err := s.Draft(ctx, id)
	return d, true, err
}
