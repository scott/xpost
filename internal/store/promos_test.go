package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

func (f *fx) promo(t *testing.T, tok *store.AgentToken, blog int64, key string, at time.Time, off *int) *store.Draft {
	t.Helper()
	d, _, err := f.st.CreateDraft(context.Background(), tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example promo: new sample post " + store.BlogURLToken}},
		ProposedAt: at, IdempotencyKey: key, Promotes: blog, PromoOffset: off})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fx) approve(t *testing.T, id int64) {
	t.Helper()
	d := f.get(t, id)
	if err := f.st.Approve(context.Background(), f.owner, id, d.Version); err != nil {
		t.Fatalf("approve %d: %v", id, err)
	}
}

func dueIDs(t *testing.T, st *store.Store, now time.Time) map[int64]bool {
	t.Helper()
	due, err := st.DueDrafts(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]bool{}
	for _, d := range due {
		m[d.ID] = true
	}
	return m
}

func TestPromoIntakeAndOffsets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "notes.example.test", store.MethodWebhook)
	f.ready(t, b)
	bp := f.propose(t, post(b.Name, "sample-week"))

	p := f.promo(t, f.a, bp.ID, "p1", time.Time{}, nil)
	if !p.IsPromo() || p.PromoOffset == nil || *p.PromoOffset != 120 || !p.ScheduledAt.Equal(bp.ScheduledAt.Add(2*time.Hour)) {
		t.Fatalf("default offset promo: %+v at %v (blog %v)", p.PromoOffset, p.ScheduledAt, bp.ScheduledAt)
	}
	fixedAt := bp.ScheduledAt.Add(5 * time.Hour)
	fx := f.promo(t, f.a, bp.ID, "p2", fixedAt, nil)
	if fx.PromoOffset != nil || !fx.ScheduledAt.Equal(fixedAt) {
		t.Fatalf("fixed promo: %+v %v", fx.PromoOffset, fx.ScheduledAt)
	}
	if c := p.Parts[0].Count(); c.Weighted != len("Example promo: new sample post ")+23 {
		t.Fatalf("{{blog_url}} should count 23, got %d", c.Weighted)
	}

	// Refusals.
	bad := []store.NewDraft{
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "link " + store.BlogURLToken}}, ProposedAt: f.now.Add(time.Hour)},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "x"}}, ProposedAt: f.now.Add(time.Hour), Promotes: bp.ID, PromoOffset: new(int)},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "x"}}, Promotes: 9999},
	}
	for i, nd := range bad {
		var ie *store.InputError
		if _, _, err := f.st.CreateDraft(ctx, f.a, nd); !errors.As(err, &ie) {
			t.Errorf("case %d: want input error, got %v", i, err)
		}
	}
	var ie *store.InputError
	if _, _, err := f.st.CreateDraft(ctx, f.b, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "x"}}, Promotes: bp.ID}); !errors.As(err, &ie) {
		t.Errorf("another agent's blog post: %v", err)
	}
	if _, _, err := f.st.CreateOwnerDraft(ctx, f.owner, store.OwnerDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "link " + store.BlogURLToken}}, When: f.now.Add(time.Hour), Key: "k1"}, false); !errors.As(err, &ie) {
		t.Errorf("owner compose with {{blog_url}}: %v", err)
	}

	// The blog moves: the offset promo moves with it, the fixed one stays.
	bp = f.get(t, bp.ID)
	moved := bp.ScheduledAt.Add(24 * time.Hour)
	if err := f.st.Reschedule(ctx, f.owner, bp.ID, bp.Version, moved, false); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, p.ID).ScheduledAt; !got.Equal(moved.Add(2 * time.Hour)) {
		t.Fatalf("offset promo at %v, want %v", got, moved.Add(2*time.Hour))
	}
	if got := f.get(t, fx.ID).ScheduledAt; !got.Equal(fixedAt) {
		t.Fatalf("fixed promo moved to %v", got)
	}
	// Moving the promo itself fixes its time.
	p = f.get(t, p.ID)
	if err := f.st.Reschedule(ctx, f.owner, p.ID, p.Version, moved.Add(3*time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if f.get(t, p.ID).PromoOffset != nil {
		t.Fatal("a promo the owner moved should keep a fixed time")
	}

	// A blog post with promos can't be deleted; rejecting it sends them back.
	bp = f.get(t, bp.ID)
	if err := f.st.Delete(ctx, f.owner, bp.ID, bp.Version); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete blog with promos: %v", err)
	}
	if err := f.st.AgentDeleteDraft(ctx, f.a, bp.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("agent delete blog with promos: %v", err)
	}
	f.approve(t, fx.ID)
	if err := f.st.Reject(ctx, f.owner, bp.ID, bp.Version, ""); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, fx.ID); got.Status != store.StatusPending || !got.PromoReturned {
		t.Fatalf("after the blog is rejected: %s returned=%v", got.Status, got.PromoReturned)
	}
	if err := f.st.Approve(ctx, f.owner, fx.ID, f.get(t, fx.ID).Version); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approve a promo of a rejected blog: %v", err)
	}
}

func TestPromoWaitsForBlog(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "notes.example.test", store.MethodWebhook)
	f.ready(t, b)
	bp := f.propose(t, post(b.Name, "sample-week"))
	p := f.promo(t, f.a, bp.ID, "p1", time.Time{}, nil)
	f.approve(t, p.ID) // approved early
	if st := store.PromoState(f.get(t, p.ID), f.get(t, bp.ID), f.now); st != store.PromoEarly {
		t.Fatalf("state %q", st)
	}
	f.approve(t, bp.ID)

	// Due, but the blog is not live: the promo waits.
	f.now = p.ScheduledAt.Add(time.Minute)
	if dueIDs(t, f.st, f.now)[p.ID] {
		t.Fatal("a promo is due before its blog post is live")
	}
	if st := store.PromoState(f.get(t, p.ID), f.get(t, bp.ID), f.now); st != store.PromoWaiting {
		t.Fatalf("state %q", st)
	}
	if _, ok, _ := f.st.PromoText(ctx, f.get(t, p.ID)); ok {
		t.Fatal("promo text resolved before the blog is live")
	}

	// The publish fails: held, still approved.
	due, _ := f.st.DueBlogs(ctx, f.now)
	if len(due) != 1 {
		t.Fatalf("due blogs %d", len(due))
	}
	if ok, err := f.st.Claim(ctx, due[0], f.now.Add(time.Minute)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := f.st.FinishBlog(ctx, bp.ID, false, "", "", "example error"); err != nil {
		t.Fatal(err)
	}
	if st := store.PromoState(f.get(t, p.ID), f.get(t, bp.ID), f.now); st != store.PromoHeld {
		t.Fatalf("state %q", st)
	}

	// Retry publish: the slot is now and the promo keeps its offset, still approved.
	bp = f.get(t, bp.ID)
	if err := f.st.PublishNow(ctx, f.owner, bp.ID, bp.Version); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, p.ID); got.Status != store.StatusApproved || !got.ScheduledAt.Equal(f.now.Add(2*time.Hour)) {
		t.Fatalf("after retry: %s at %v", got.Status, got.ScheduledAt)
	}
	due, _ = f.st.DueBlogs(ctx, f.now)
	f.st.Claim(ctx, due[0], f.now.Add(time.Minute))
	url := b.LiveURL("sample-week")
	if err := f.st.FinishBlog(ctx, bp.ID, true, url, "example-ref", ""); err != nil {
		t.Fatal(err)
	}
	parts, ok, err := f.st.PromoText(ctx, f.get(t, p.ID))
	if err != nil || !ok || !strings.Contains(parts[0].Text, url) || strings.Contains(parts[0].Text, store.BlogURLToken) {
		t.Fatalf("promo text: %v %v %+v", ok, err, parts)
	}
	f.now = f.now.Add(2*time.Hour + time.Minute)
	if !dueIDs(t, f.st, f.now)[p.ID] {
		t.Fatal("the promo should be due once the blog is live")
	}
}

func TestPromoReturnsAfter24h(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "notes.example.test", store.MethodWebhook)
	f.ready(t, b)
	bp := f.propose(t, post(b.Name, "sample-week"))
	p := f.promo(t, f.a, bp.ID, "p1", time.Time{}, nil)
	f.approve(t, p.ID)
	f.now = p.ScheduledAt.Add(23 * time.Hour)
	if n, err := f.st.ExpirePromos(ctx, f.now); n != 0 || err != nil {
		t.Fatalf("expired early: %d %v", n, err)
	}
	f.now = p.ScheduledAt.Add(24 * time.Hour)
	if n, err := f.st.ExpirePromos(ctx, f.now); n != 1 || err != nil {
		t.Fatalf("expire: %d %v", n, err)
	}
	got := f.get(t, p.ID)
	if got.Status != store.StatusPending || !got.PromoReturned || got.ApprovedAt != nil {
		t.Fatalf("returned promo: %s %v", got.Status, got.PromoReturned)
	}
}

// Offset promos count from the real publish time: a late publish moves
// them. Fixed-time promos keep their time, and nothing is due before the
// blog post is confirmed live (published, with its URL).
func TestPromoOffsetFromRealPublish(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "notes.example.test", store.MethodWebhook)
	f.ready(t, b)
	bp := f.propose(t, post(b.Name, "sample-late"))
	slot := bp.ScheduledAt
	off := f.promo(t, f.a, bp.ID, "p1", time.Time{}, nil)
	fixedAt := slot.Add(30 * time.Minute)
	fx := f.promo(t, f.a, bp.ID, "p2", fixedAt, nil)
	f.approve(t, off.ID)
	f.approve(t, fx.ID)
	f.approve(t, bp.ID)

	f.now = slot.Add(time.Minute)
	due, _ := f.st.DueBlogs(ctx, f.now)
	if len(due) != 1 {
		t.Fatalf("due blogs %d", len(due))
	}
	if ok, err := f.st.Claim(ctx, due[0], f.now.Add(4*time.Hour)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	// Still publishing well past both promo times: neither is due.
	f.now = slot.Add(3 * time.Hour)
	if m := dueIDs(t, f.st, f.now); m[off.ID] || m[fx.ID] {
		t.Fatalf("a promo is due before its blog post is live: %v", m)
	}
	if err := f.st.FinishBlog(ctx, bp.ID, true, b.LiveURL("sample-late"), "example-ref", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, off.ID).ScheduledAt; !got.Equal(f.now.Add(2 * time.Hour)) {
		t.Fatalf("offset promo at %v, want real publish time + 2h (%v)", got, f.now.Add(2*time.Hour))
	}
	if got := f.get(t, fx.ID).ScheduledAt; !got.Equal(fixedAt) {
		t.Fatalf("fixed promo moved to %v", got)
	}
	m := dueIDs(t, f.st, f.now)
	if !m[fx.ID] || m[off.ID] {
		t.Fatalf("after a late publish: fixed due %v, offset due %v", m[fx.ID], m[off.ID])
	}
	// Published but no live URL recorded: not confirmed live, nothing due.
	f.st.DB.ExecContext(ctx, `UPDATE blog_posts SET published_url = '' WHERE draft_id = ?`, bp.ID)
	if m := dueIDs(t, f.st, f.now.Add(3*time.Hour)); m[fx.ID] || m[off.ID] {
		t.Fatalf("a promo is due without a live URL: %v", m)
	}
}

// A blog post marked published with no live URL is not live: its approved
// promos still return to Pending 24h past their time.
func TestPromoReturnsWhenPublishedWithoutURL(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "notes.example.test", store.MethodWebhook)
	f.ready(t, b)
	bp := f.propose(t, post(b.Name, "sample-nourl"))
	p := f.promo(t, f.a, bp.ID, "p1", time.Time{}, nil)
	f.approve(t, p.ID)
	f.approve(t, bp.ID)
	f.now = bp.ScheduledAt.Add(time.Minute)
	due, _ := f.st.DueBlogs(ctx, f.now)
	f.st.Claim(ctx, due[0], f.now.Add(time.Minute))
	if err := f.st.FinishBlog(ctx, bp.ID, true, b.LiveURL("sample-nourl"), "example-ref", ""); err != nil {
		t.Fatal(err)
	}
	f.st.DB.ExecContext(ctx, `UPDATE blog_posts SET published_url = '' WHERE draft_id = ?`, bp.ID)
	p = f.get(t, p.ID)
	f.now = p.ScheduledAt.Add(24*time.Hour + time.Minute)
	if n, err := f.st.ExpirePromos(ctx, f.now); n != 1 || err != nil {
		t.Fatalf("expire with an empty published_url: %d %v", n, err)
	}
	if got := f.get(t, p.ID); got.Status != store.StatusPending {
		t.Fatalf("promo %s", got.Status)
	}
}
