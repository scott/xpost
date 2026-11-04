package poster

import (
	"context"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// The scheduler skips replies: an approved reply is never due, so the
// poster is never called for it, however long it waits.
func TestSchedulerSkipsReplies(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	d, _, err := f.st.CreateDraft(ctx, f.tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example reply"}},
		Reply: &store.NewReply{InReplyTo: "1900000000000000111", AuthorHandle: "sample_builder", Text: "Example post"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{}
	disp := f.disp(fake)
	for i := 0; i < 3; i++ {
		f.now = f.now.Add(30 * 24 * time.Hour)
		if rep := f.run(t, disp); rep.Posted != 0 {
			t.Fatalf("run %d posted: %+v", i, rep)
		}
	}
	if len(fake.Calls()) != 0 || status(t, f, d.ID) != store.StatusApproved {
		t.Fatalf("%d poster calls, status %s", len(fake.Calls()), status(t, f, d.ID))
	}
}
