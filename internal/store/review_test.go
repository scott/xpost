package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

var t0 = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

type fx struct {
	st    *store.Store
	owner *store.User
	a, b  *store.AgentToken
	now   time.Time
}

func setup(t *testing.T) *fx {
	t.Helper()
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	k, _ := secret.NewKey()
	f := &fx{now: t0}
	f.st = store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := f.st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	f.st.Now = func() time.Time { return f.now }
	f.owner, _, _ = f.st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	f.a, _, _ = f.st.CreateAgentToken(ctx, f.owner, "sample-agent-1", []string{store.ScopeDraftsWrite})
	f.b, _, _ = f.st.CreateAgentToken(ctx, f.owner, "sample-agent-2", []string{store.ScopeDraftsWrite})
	return f
}

func (f *fx) create(t *testing.T, tok *store.AgentToken, key string, at time.Time, text string) *store.Draft {
	t.Helper()
	d, _, err := f.st.CreateDraft(context.Background(), tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: text}}, ProposedAt: at, IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fx) get(t *testing.T, id int64) *store.Draft {
	d, err := f.st.Draft(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestStateMachine(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	at := t0.Add(2 * time.Hour)
	d := f.create(t, f.a, "", at, "Example")
	if d.Status != store.StatusPending || d.Version != 1 {
		t.Fatalf("new: %s v%d", d.Status, d.Version)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusApproved || d.Version != 2 {
		t.Fatalf("approved: %s v%d", d.Status, d.Version)
	}
	// Approving twice with the old version is stale.
	if err := f.st.Approve(ctx, f.owner, d.ID, 1); !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrStale) {
		t.Fatalf("stale approve: %v", err)
	}
	// Reschedule without approve returns it to pending.
	if err := f.st.Reschedule(ctx, f.owner, d.ID, d.Version, at.Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	if d = f.get(t, d.ID); d.Status != store.StatusPending {
		t.Fatalf("rescheduled approved: %s", d.Status)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if err := f.st.Reject(ctx, f.owner, d.ID, d.Version, "Example reason"); err != nil {
		t.Fatalf("reject approved: %v", err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusRejected || d.RejectReason != "Example reason" {
		t.Fatalf("rejected: %s %q", d.Status, d.RejectReason)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err == nil {
		t.Fatal("approved a rejected draft")
	}
	if err := f.st.Save(ctx, f.owner, d.ID, d.Version, store.Edit{Parts: []store.PartIn{{Text: "Example"}}, When: at}, false); err == nil {
		t.Fatal("edited a rejected draft")
	}
	if err := f.st.Delete(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatalf("owner delete rejected: %v", err)
	}
}

func TestOwnerCannotChangePostingOrPosted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.create(t, f.a, "", t0.Add(20*time.Minute), "Example")
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	f.now = t0.Add(time.Hour)
	due, err := f.st.DueDrafts(ctx, f.now)
	if err != nil || len(due) != 1 {
		t.Fatalf("due: %v %d", err, len(due))
	}
	if ok, err := f.st.Claim(ctx, due[0], f.now.Add(time.Minute)); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPosting {
		t.Fatalf("claimed: %s", d.Status)
	}
	if ok, _ := f.st.Claim(ctx, due[0], f.now.Add(time.Minute)); ok {
		t.Fatal("claimed twice")
	}
	if again, _ := f.st.DueDrafts(ctx, f.now); len(again) != 0 {
		t.Fatal("a leased draft is due again")
	}
	edit := store.Edit{Parts: []store.PartIn{{Text: "Example edit"}}, When: f.now.Add(time.Hour)}
	for name, err := range map[string]error{
		"save":       f.st.Save(ctx, f.owner, d.ID, d.Version, edit, false),
		"reschedule": f.st.Reschedule(ctx, f.owner, d.ID, d.Version, f.now.Add(time.Hour), false),
		"reject":     f.st.Reject(ctx, f.owner, d.ID, d.Version, ""),
		"delete":     f.st.Delete(ctx, f.owner, d.ID, d.Version),
	} {
		if err == nil {
			t.Errorf("%s of a posting draft succeeded", name)
		}
	}
	if err := f.st.AgentDeleteDraft(ctx, f.a, d.ID); err == nil {
		t.Fatal("agent deleted a posting draft")
	}
}

func TestIdempotencyIsPerToken(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	at := t0.Add(2 * time.Hour)
	a1 := f.create(t, f.a, "example-key", at, "Example A")
	again, created, err := f.st.CreateDraft(ctx, f.a, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example A changed"}}, ProposedAt: at, IdempotencyKey: "example-key"})
	if err != nil || created || again.ID != a1.ID || again.Parts[0].Text != "Example A" {
		t.Fatalf("replay: %v %v %+v", err, created, again)
	}
	b1 := f.create(t, f.b, "example-key", at, "Example B")
	if b1.ID == a1.ID {
		t.Fatal("tokens share idempotency keys")
	}
}

func TestAgentDeleteRules(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	at := t0.Add(2 * time.Hour)
	mine := f.create(t, f.a, "", at, "Example mine")
	if err := f.st.AgentDeleteDraft(ctx, f.b, mine.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other token delete: %v", err)
	}
	f.st.Approve(ctx, f.owner, mine.ID, mine.Version)
	if err := f.st.AgentDeleteDraft(ctx, f.a, mine.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete approved: %v", err)
	}
	p := f.create(t, f.a, "", at.Add(time.Hour), "Example pending")
	if err := f.st.AgentDeleteDraft(ctx, f.a, p.ID); err != nil {
		t.Fatalf("delete own pending: %v", err)
	}
}

func TestNextFreeSkipsBusySlots(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, m := range []int{0, 15} {
		d := f.create(t, f.a, "", t0.Add(time.Hour+time.Duration(m)*time.Minute), "Example")
		if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.st.NextFree(ctx, "sample_writer", t0.Add(time.Hour+5*time.Minute), 0); !got.Equal(t0.Add(time.Hour + 30*time.Minute)) {
		t.Fatalf("next free %v", got)
	}
}
