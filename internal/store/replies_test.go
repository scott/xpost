package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Invented post ids and handles: every original here is example data.
const (
	origID  = "1900000000000000111"
	origID2 = "1900000000000000222"
)

func sampleOrig(id string) *store.NewReply {
	at := t0.Add(-2 * time.Hour)
	return &store.NewReply{InReplyTo: id, AuthorName: "Sample Builder", AuthorHandle: "sample_builder", Text: "Example post: what do you check before deploy?", CreatedAt: &at}
}

func (f *fx) reply(t *testing.T, tok *store.AgentToken, account, text string, nr *store.NewReply) *store.Draft {
	t.Helper()
	d, _, err := f.st.CreateDraft(context.Background(), tok, store.NewDraft{Account: account, Parts: []store.PartIn{{Text: text}}, Reply: nr})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func conflictMsg(t *testing.T, err error, want string) {
	t.Helper()
	var ce *store.ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, want) {
		t.Fatalf("want conflict %q, got %v", want, err)
	}
}

// A reply is an X draft with a reply row: one part, no images, no time to
// pick (proposed_at is when it arrived), and over 280 is allowed in.
func TestReplyIntake(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	long := strings.Repeat("a", 296)
	d := f.reply(t, f.a, "sample_writer", long, sampleOrig(origID))
	if !d.IsReply() || d.Kind != store.KindX || d.Status != store.StatusPending || !d.ProposedAt.Equal(t0) {
		t.Fatalf("reply: %+v", d)
	}
	r := d.Reply
	if r.InReplyTo != origID || r.AuthorHandle != "sample_builder" || r.AuthorName != "Sample Builder" || !r.Available || r.CreatedAt == nil || r.OpenedAt != nil {
		t.Fatalf("reply row: %+v", r)
	}
	bad := []store.NewDraft{
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a"}, {Text: "b"}}, Reply: sampleOrig(origID)},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a", Media: []store.MediaIn{{Ref: "x"}}}}, Reply: sampleOrig(origID)},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: " "}}, Reply: sampleOrig(origID)},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a"}}, Reply: &store.NewReply{InReplyTo: "12ab"}},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a"}}, Reply: &store.NewReply{InReplyTo: origID, ReplySettings: "nobody"}},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a"}}, Reply: &store.NewReply{InReplyTo: origID, AuthorHandle: "not a handle", Text: "x"}},
		{Account: "sample_writer", Parts: []store.PartIn{{Text: "a"}}, Reply: sampleOrig(origID), Promotes: 5},
		{Account: "nobody_here", Parts: []store.PartIn{{Text: "a"}}, Reply: sampleOrig(origID)},
	}
	for i, nd := range bad {
		if _, _, err := f.st.CreateDraft(ctx, f.a, nd); !errors.Is(err, store.ErrBadInput) {
			t.Errorf("case %d: want bad input, got %v", i, err)
		}
	}
	// No handle or text, or unavailable: only the id is kept.
	u := f.reply(t, f.a, "sample_writer", "Example reply", &store.NewReply{InReplyTo: origID2, AuthorHandle: "sample_builder"})
	if u.Reply.Available || u.Reply.AuthorHandle != "" {
		t.Fatalf("unavailable: %+v", u.Reply)
	}
}

// Approve is blocked by an unavailable original, over 280 weighted, and
// lint errors (the em dash, and banned topics of the account's blogs).
// Warnings (en dash, limited reply settings, duplicates) never block.
func TestReplyApproveBlocks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.st.CreateBlog(ctx, f.owner, store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook,
		PromoAccount: "sample_writer", Topics: []string{"sample banned topic"}}); err != nil {
		t.Fatal(err)
	}
	un := f.reply(t, f.a, "sample_writer", "Example reply", &store.NewReply{InReplyTo: origID, Unavailable: true})
	conflictMsg(t, f.st.Approve(ctx, f.owner, un.ID, un.Version), "the original post didn't load")
	over := f.reply(t, f.a, "sample_writer", strings.Repeat("b", 296), sampleOrig(origID))
	conflictMsg(t, f.st.Approve(ctx, f.owner, over.ID, over.Version), "Shorten by 16 to approve.")
	topic := f.reply(t, f.a, "sample_writer", "Example reply about a Sample Banned Topic here", sampleOrig(origID))
	conflictMsg(t, f.st.Approve(ctx, f.owner, topic.ID, topic.Version), "Can't approve")
	em := f.reply(t, f.a, "sample_writer", "Example reply \u2014 with an em dash", sampleOrig(origID))
	conflictMsg(t, f.st.Approve(ctx, f.owner, em.ID, em.Version), "Em dash in the reply")
	// The other account has no blog: its topics are not banned there.
	other := f.reply(t, f.a, "sample_maker", "Example reply about a sample banned topic", sampleOrig(origID))
	if err := f.st.Approve(ctx, f.owner, other.ID, other.Version); err != nil {
		t.Fatalf("other account: %v", err)
	}
	nr := sampleOrig(origID)
	nr.ReplySettings = "following"
	warn := f.reply(t, f.a, "sample_writer", "Example reply \u2013 en dash only", nr)
	if !warn.Reply.Limited() || f.st.ReplyDuplicates(ctx, warn) == 0 {
		t.Fatalf("warnings missing: limited %v dup %d", warn.Reply.Limited(), f.st.ReplyDuplicates(ctx, warn))
	}
	if err := f.st.Approve(ctx, f.owner, warn.ID, warn.Version); err != nil {
		t.Fatalf("warnings blocked approve: %v", err)
	}
	for _, id := range []int64{un.ID, over.ID, topic.ID, em.ID} {
		if s := f.get(t, id).Status; s != store.StatusPending {
			t.Fatalf("draft %d is %s", id, s)
		}
	}
}

// The hand-off: Approve, Open on X (records opened_at), Mark posted with
// or without a link. Unapprove and Put back in queue clear opened_at. No
// step writes the post log: nothing goes through the API.
func TestReplyHandOff(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.reply(t, f.a, "sample_writer", "Example reply", sampleOrig(origID))
	if err := f.st.OpenedOnX(ctx, f.owner, d.ID, d.Version); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("open before approve: %v", err)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	conflictMsg(t, f.st.MarkReplyPosted(ctx, f.owner, d.ID, d.Version, ""), "Open it on X first")
	if err := f.st.Unapprove(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPending || d.ApprovedAt != nil {
		t.Fatalf("unapprove: %+v", d)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if err := f.st.OpenedOnX(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Reply.OpenedAt == nil || !d.Reply.OpenedAt.Equal(t0) || d.Status != store.StatusApproved {
		t.Fatalf("opened: %+v", d.Reply)
	}
	// Put back in queue (the same Unapprove) clears the opened time.
	if err := f.st.Unapprove(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Reply.OpenedAt != nil || d.Status != store.StatusPending {
		t.Fatalf("put back: %+v %s", d.Reply, d.Status)
	}
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	f.st.OpenedOnX(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	link := "https://x.com/sample_writer/status/1900000000000000777"
	if err := f.st.MarkReplyPosted(ctx, f.owner, d.ID, d.Version, link); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPosted || d.Reply.PostedURL != link {
		t.Fatalf("posted: %s %+v", d.Status, d.Reply)
	}
	if rows, _ := f.st.DraftLog(ctx, d.ID); len(rows) != 0 {
		t.Fatalf("post log rows for a hand-posted reply: %d", len(rows))
	}
	// Posted without a link.
	n := f.reply(t, f.a, "sample_writer", "Example reply two", sampleOrig(origID2))
	f.st.Approve(ctx, f.owner, n.ID, n.Version)
	n = f.get(t, n.ID)
	f.st.OpenedOnX(ctx, f.owner, n.ID, n.Version)
	n = f.get(t, n.ID)
	if err := f.st.MarkReplyPosted(ctx, f.owner, n.ID, n.Version, ""); err != nil {
		t.Fatal(err)
	}
	posted, _ := f.st.Queue(ctx, store.Filter{Status: store.StatusPosted, Kind: store.KindReply})
	if len(posted) != 2 {
		t.Fatalf("posted replies: %d", len(posted))
	}
	// The plain X review actions refuse a reply where they would schedule it.
	r := f.reply(t, f.a, "sample_writer", "Example reply three", sampleOrig(origID2))
	conflictMsg(t, f.st.Reschedule(ctx, f.owner, r.ID, r.Version, t0.Add(time.Hour), false), "no time to move")
	conflictMsg(t, f.st.Save(ctx, f.owner, r.ID, r.Version, store.Edit{Parts: []store.PartIn{{Text: "x"}}}, true), "approve the reply from the queue")
}

// Opened over a day ago and not marked: "Not marked yet". Nothing ever
// reverts it: it stays approved, and the dispatcher still leaves it alone.
func TestReplyNotMarkedYetNeverReverts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.reply(t, f.a, "sample_writer", "Example reply", sampleOrig(origID))
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	f.st.OpenedOnX(ctx, f.owner, d.ID, d.Version)
	f.now = t0.Add(23 * time.Hour)
	if f.get(t, d.ID).Reply.NotMarked(f.now) {
		t.Fatal("not marked before a day")
	}
	f.now = t0.Add(72 * time.Hour)
	d = f.get(t, d.ID)
	if !d.Reply.NotMarked(f.now) || d.Status != store.StatusApproved || d.Reply.OpenedAt == nil {
		t.Fatalf("after 3 days: %s %+v", d.Status, d.Reply)
	}
	if due, _ := f.st.DueDrafts(ctx, f.now); len(due) != 0 {
		t.Fatalf("due: %+v", due)
	}
}

// Replies are never due, not on the Schedule, not spaced and add $0 to
// the spend estimate.
func TestReplySkipsSchedulerSpacingAndSpend(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	d := f.reply(t, f.a, "sample_writer", "Example reply https://example.test/x", sampleOrig(origID))
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	// A post two minutes after the approved reply is not too close.
	p := f.create(t, f.a, "", t0.Add(2*time.Minute), "Example post")
	if n := f.st.Neighbors(ctx, "sample_writer", p.ScheduledAt, p.ID); len(n) != 0 {
		t.Fatalf("reply counted as a neighbor: %+v", n)
	}
	if err := f.st.Approve(ctx, f.owner, p.ID, p.Version); err != nil {
		t.Fatalf("post next to a reply: %v", err)
	}
	f.now = t0.Add(time.Hour)
	due, _ := f.st.DueDrafts(ctx, f.now)
	if len(due) != 1 || due[0].ID != p.ID {
		t.Fatalf("due: %+v", due)
	}
	between, _ := f.st.Between(ctx, t0.Add(-time.Hour), t0.Add(time.Hour))
	if len(between) != 1 || between[0].ID != p.ID {
		t.Fatalf("between: %d", len(between))
	}
	est, _ := f.st.ApprovedBetween(ctx, t0.Add(-time.Hour), t0.Add(time.Hour))
	if est != 1_000_000 {
		t.Fatalf("estimate %d, want only the post (1_000_000)", est)
	}
}

// Retry load reads only what agents sent: the newest available copy of the
// same post on any stored reply. It never calls X.
func TestReplyRetryLoad(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.reply(t, f.a, "sample_writer", "Example reply", &store.NewReply{InReplyTo: origID, Unavailable: true})
	f.now = t0.Add(5 * time.Minute)
	ok, err := f.st.RetryReplyLoad(ctx, f.owner, d.ID, d.Version)
	if err != nil || ok {
		t.Fatalf("retry with nothing to read: %v %v", ok, err)
	}
	d = f.get(t, d.ID)
	if !d.Reply.TriedAt.Equal(f.now) || d.Reply.Available {
		t.Fatalf("tried: %+v", d.Reply)
	}
	f.reply(t, f.b, "sample_maker", "Another example reply", sampleOrig(origID))
	ok, err = f.st.RetryReplyLoad(ctx, f.owner, d.ID, d.Version)
	if err != nil || !ok {
		t.Fatalf("retry: %v %v", ok, err)
	}
	d = f.get(t, d.ID)
	if !d.Reply.Available || d.Reply.AuthorHandle != "sample_builder" || d.Reply.Text == "" {
		t.Fatalf("loaded: %+v", d.Reply)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatalf("approve after load: %v", err)
	}
}

// The queue filter: Replies, and X posts without replies.
func TestReplyQueueFilter(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.create(t, f.a, "", t0.Add(time.Hour), "Example post")
	f.reply(t, f.a, "sample_writer", "Example reply", sampleOrig(origID))
	all, _ := f.st.Queue(ctx, store.Filter{})
	xs, _ := f.st.Queue(ctx, store.Filter{Kind: store.KindX})
	rs, _ := f.st.Queue(ctx, store.Filter{Kind: store.KindReply})
	if len(all) != 2 || len(xs) != 1 || xs[0].IsReply() || len(rs) != 1 || !rs[0].IsReply() {
		t.Fatalf("all %d x %d replies %d", len(all), len(xs), len(rs))
	}
	c, _ := f.st.QueueCounts(ctx, "", store.KindReply)
	if c.ByStatus[store.StatusPending] != 1 || c.PendingByKind[store.KindReply] != 1 || c.PendingByKind[store.KindX] != 1 {
		t.Fatalf("counts %+v", c)
	}
	if !f.st.HasReplies(ctx) || f.st.PendingCount(ctx) != 2 {
		t.Fatal("reply not counted")
	}
}

// Every reply state change bumps the draft version, so the live feed
// (SSE) carries it.
func TestReplyStatesReachTheLiveFeed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.reply(t, f.a, "sample_writer", "Example reply", sampleOrig(origID))
	seq, _, _ := f.st.LiveSeq(ctx)
	steps := []func(v int) error{
		func(v int) error { return f.st.Approve(ctx, f.owner, d.ID, v) },
		func(v int) error { return f.st.OpenedOnX(ctx, f.owner, d.ID, v) },
		func(v int) error { return f.st.Unapprove(ctx, f.owner, d.ID, v) },
		func(v int) error { return f.st.Approve(ctx, f.owner, d.ID, v) },
		func(v int) error { return f.st.OpenedOnX(ctx, f.owner, d.ID, v) },
		func(v int) error { return f.st.MarkReplyPosted(ctx, f.owner, d.ID, v, "") },
	}
	for i, step := range steps {
		v := f.get(t, d.ID).Version
		if err := step(v); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		ch, _ := f.st.ChangesSince(ctx, seq, 100)
		if len(ch) == 0 || ch[len(ch)-1].ID != d.ID || ch[len(ch)-1].V != int64(v+1) {
			t.Fatalf("step %d: changes %+v", i, ch)
		}
		seq = ch[len(ch)-1].Seq
	}
}

// Save works over 280 for a reply and keeps it pending; edits after
// opening clear the opened time.
func TestReplySaveOverLimit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	d := f.reply(t, f.a, "sample_writer", "Example reply", sampleOrig(origID))
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	f.st.OpenedOnX(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	if err := f.st.Save(ctx, f.owner, d.ID, d.Version, store.Edit{Parts: []store.PartIn{{Text: strings.Repeat("c", 300)}}}, false); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPending || d.Reply.OpenedAt != nil || len(d.Parts[0].Text) != 300 || !d.ScheduledAt.Equal(t0) {
		t.Fatalf("saved: %s %+v", d.Status, d.Reply)
	}
	if err := f.st.Save(ctx, f.owner, d.ID, d.Version, store.Edit{Parts: []store.PartIn{{Text: "a"}, {Text: "b"}}}, false); !errors.Is(err, store.ErrBadInput) {
		t.Fatalf("two parts: %v", err)
	}
}
