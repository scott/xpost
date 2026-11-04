package poster

import (
	"context"
	"testing"
	"time"

	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/store"
)

// The composer counts a new post with store.PartsEstimate. For a post with
// nothing sent yet the dispatcher must reach the same cap decision: it
// posts when spent + estimate is exactly the cap and waits one cent below.
// The owner's post is a thread with a link and an image with alt text, so
// every rate is in play, and it posts back to back like any thread.
func TestComposerEstimateMatchesDispatcher(t *testing.T) {
	for _, tc := range []struct {
		capCents int64
		posts    bool
	}{{360, true}, {359, false}} {
		f := setup(t)
		f.enable(t) // $1 a post, $2 with a link
		f.enableImages()
		ctx := context.Background()
		_, mid, err := f.st.AddMedia(ctx, nil, demo.Image(1))
		if err != nil {
			t.Fatal(err)
		}
		parts := []store.PartIn{
			{Text: "Example owner post", Media: []store.MediaIn{{ID: mid, Alt: "Example alt text"}}},
			{Text: "Example part two https://example.com/sample"},
		}
		d, _, err := f.st.CreateOwnerDraft(ctx, f.owner, store.OwnerDraft{Account: "sample_writer", Parts: parts, When: t0.Add(20 * time.Minute)}, true)
		if err != nil {
			t.Fatal(err)
		}
		if est := f.st.PartsEstimate(d.Parts); est != 3_600_000 {
			t.Fatalf("estimate %d, want $1 + $2 + $0.50 + $0.10", est)
		}
		f.st.SetCapCents(ctx, f.owner, tc.capCents)
		fake := f.fake()
		f.now = t0.Add(30 * time.Minute)
		f.run(t, f.disp(fake))
		got := status(t, f, d.ID)
		if tc.posts && (got != store.StatusPosted || len(fake.Calls()) != 2) {
			t.Fatalf("cap %d: %s, %d calls", tc.capCents, got, len(fake.Calls()))
		}
		if !tc.posts && (got != store.StatusApproved || len(fake.Calls()) != 0) {
			t.Fatalf("cap %d: %s, %d calls", tc.capCents, got, len(fake.Calls()))
		}
	}
}
