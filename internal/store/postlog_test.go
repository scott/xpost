package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// The intent row is the double-post guard: a key can be (re)opened only
// when its last attempt definitely did not post.
func TestBeginPartGuardsEveryState(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	e := store.LogEntry{DraftID: 1, Account: "sample_writer", Part: 1, Parts: 1, IdemKey: "k"}
	if err := f.st.BeginPart(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginPart(ctx, e); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("in-flight key reopened: %v", err)
	}
	for _, st := range []string{store.LogFailed, store.LogRetry} {
		if err := f.st.FinishPart(ctx, "k", store.Outcome{Status: st, Error: "example"}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.BeginPart(ctx, e); err != nil {
			t.Fatalf("after %s: %v", st, err)
		}
	}
	if err := f.st.FinishPart(ctx, "k", store.Outcome{Status: store.LogUnknown}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginPart(ctx, e); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("unknown key reopened: %v", err)
	}
	e.IdemKey = "k2"
	f.st.BeginPart(ctx, e)
	if err := f.st.FinishPart(ctx, "k2", store.Outcome{Status: store.LogPosted, PostID: "9"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.BeginPart(ctx, e); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("posted key reopened: %v", err)
	}
	if err := f.st.FinishPart(ctx, "k2", store.Outcome{Status: store.LogFailed}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a posted row was overwritten: %v", err)
	}
	if err := f.st.FinishPart(ctx, "k2", store.Outcome{Status: "bogus"}); !errors.Is(err, store.ErrBadInput) {
		t.Fatal("bogus status accepted")
	}
	r, _ := f.st.LogByKey(ctx, "k")
	if r.Attempts != 3 || r.Status != store.LogUnknown {
		t.Fatalf("%+v", r)
	}
}

func TestSpendCountsUnknownAsBilled(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	add := func(key, st string, link bool) {
		f.st.RecordPart(ctx, store.LogEntry{DraftID: 1, Account: "sample_writer", Part: 1, Parts: 1, At: f.now, Status: st, HasURL: link, IdemKey: key})
	}
	add("a", store.LogPosted, false)
	add("b", store.LogUnknown, true)
	add("c", store.LogFailed, false)
	add("d", store.LogRetry, false)
	sp, err := f.st.MonthSpend(ctx, time.UTC, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Posts != 1 || sp.LinkPosts != 1 || sp.UnknownParts != 1 || sp.FailedParts != 1 || sp.Micros != 3_000_000 {
		t.Fatalf("%+v", sp)
	}
	if sp.CapSet || sp.PostingBlock() == "" {
		t.Fatal("no cap must block")
	}
	f.st.SetCapCents(ctx, f.owner, 300)
	sp, _ = f.st.MonthSpend(ctx, time.UTC, f.now)
	if sp.PostingBlock() == "" {
		t.Fatal("spend at the cap must block")
	}
	f.st.SetCapCents(ctx, f.owner, 301)
	if sp, _ = f.st.MonthSpend(ctx, time.UTC, f.now); sp.PostingBlock() != "" {
		t.Fatalf("blocked under the cap: %s", sp.PostingBlock())
	}
	f.st.Rates = store.Rates{}
	if sp, _ = f.st.MonthSpend(ctx, time.UTC, f.now); sp.Micros != 0 || sp.PostingBlock() == "" {
		t.Fatal("unverified rates must block")
	}
}

func TestCapBounds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for _, c := range []int64{0, -1, store.MaxCapCents + 1} {
		if err := f.st.SetCapCents(ctx, f.owner, c); err == nil {
			t.Errorf("%d accepted", c)
		}
	}
	if err := f.st.SetCapCents(ctx, nil, 2000); err == nil {
		t.Error("set without the owner")
	}
	f.st.SetSetting(ctx, store.SettingCapCents, "0") // a bad stored value fails closed
	if _, ok := f.st.CapCents(ctx); ok {
		t.Error("zero cap treated as set")
	}
}
