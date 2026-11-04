package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/scott/xpost/internal/store"
)

// seedReplies adds example replies in each hand-off state (Sol's 08
// mocks): pending, over the limit, original unavailable, author limits
// replies, approved, opened on X, and posted with and without a link.
// Every id, handle and text is invented; links point at x.invalid.
func seedReplies(ctx context.Context, st *store.Store, owner *store.User, tok *store.AgentToken, now time.Time) ([]string, error) {
	at := func(t time.Time) { st.Now = func() time.Time { return t } }
	orig := func(id, name, handle, text string, ago time.Duration, settings string) *store.NewReply {
		c := now.Add(-ago)
		return &store.NewReply{InReplyTo: id, AuthorName: name, AuthorHandle: handle, Text: text, CreatedAt: &c, ReplySettings: settings}
	}
	// 296 weighted characters: 16 over, so Approve is blocked (08d2).
	long := "Example reply that runs long on purpose: I re-read the diff once, then make sure the rollback step still works, then I check the logs, alerts and the on-call notes, and then, only then, I press the button. Boring, but it has saved me more than once, and it will again next week too, I am so sure."
	type spec struct {
		acct, text string
		recv       time.Duration
		nr         *store.NewReply
		state      string // "", approved, opened, posted, posted-nolink
		opened     time.Duration
	}
	specs := []spec{
		{acct: "sample_writer", recv: 58 * time.Minute, text: "Example reply: I re-read the diff once, then make sure the rollback step still works. Boring, but it has saved me more than once.",
			nr: orig("1900000000000000111", "Sample Builder", "sample_builder", "Example post: shipping a tiny demo feature today. What's the one thing you always check before you hit deploy?", 2*time.Hour, "")},
		{acct: "sample_writer", recv: 22 * time.Minute, text: long,
			nr: orig("1900000000000000222", "Sample Builder", "sample_builder", "Example post: what is on your pre-deploy checklist?", 3*time.Hour, "")},
		{acct: "sample_maker", recv: 40 * time.Minute, text: "Example reply: the demo export works for me too. Nice sample.",
			nr: &store.NewReply{InReplyTo: "1900000000000000555", Unavailable: true}},
		{acct: "sample_writer", recv: 50 * time.Minute, text: "Example reply: we keep a short sample checklist for this.",
			nr: orig("1900000000000000333", "Demo Studio", "demo_studio_ex", "Example post: show us your sample release checklist.", 5*time.Hour, "following")},
		{acct: "sample_writer", recv: 70 * time.Minute, state: "approved", text: "Example reply: small releases, every day. It keeps the rollback boring.",
			nr: orig("1900000000000000444", "Sample Builder", "sample_builder", "Example post: how often do you ship a sample project?", 4*time.Hour, "")},
		{acct: "sample_writer", recv: 90 * time.Minute, state: "opened", opened: 9 * time.Minute, text: "Example reply: I re-read the diff once, then make sure the rollback step still works.",
			nr: orig("1900000000000000666", "Example Dev", "example_dev", "Example post: what do you check twice before a demo?", 6*time.Hour, "")},
		{acct: "sample_writer", recv: 26 * time.Hour, state: "posted", text: "Example reply: thanks for the sample notes, they helped.",
			nr: orig("1900000000000000777", "Sample Builder", "sample_builder", "Example post: sample release notes are up.", 30*time.Hour, "")},
		{acct: "sample_maker", recv: 28 * time.Hour, state: "posted-nolink", text: "Example reply: same here, example only.",
			nr: orig("1900000000000000888", "Demo Studio", "demo_studio_ex", "Example post: who else tests on a phone first?", 32*time.Hour, "")},
	}
	for i, sp := range specs {
		at(now.Add(-sp.recv))
		d, _, err := st.CreateDraft(ctx, tok, store.NewDraft{Account: sp.acct, Parts: []store.PartIn{{Text: sp.text}}, Reply: sp.nr})
		if err != nil {
			return nil, fmt.Errorf("demo reply: %w", err)
		}
		if sp.state == "" {
			continue
		}
		at(now.Add(-sp.recv / 2))
		if err := st.Approve(ctx, owner, d.ID, d.Version); err != nil {
			return nil, fmt.Errorf("demo reply approve: %w", err)
		}
		if sp.state == "approved" {
			continue
		}
		open := now.Add(-sp.opened)
		if sp.opened == 0 {
			open = now.Add(-sp.recv / 3)
		}
		at(open)
		d, _ = st.Draft(ctx, d.ID)
		if err := st.OpenedOnX(ctx, owner, d.ID, d.Version); err != nil {
			return nil, err
		}
		if sp.state == "opened" {
			continue
		}
		at(open.Add(2 * time.Minute))
		d, _ = st.Draft(ctx, d.ID)
		link := ""
		if sp.state == "posted" {
			link = fmt.Sprintf("https://x.invalid/%s/status/19000000000000099%02d", sp.acct, i)
		}
		if err := st.MarkReplyPosted(ctx, owner, d.ID, d.Version, link); err != nil {
			return nil, err
		}
	}
	return []string{fmt.Sprintf("%d example replies (posted by hand on X; invented originals)", len(specs))}, nil
}
