package poster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// A promo posts only once its blog post is live, with the live URL in
// place of {{blog_url}}.
func TestPromoPostsAfterBlogWithURL(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	b, err := f.st.CreateBlog(ctx, f.owner, store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook})
	if err != nil {
		t.Fatal(err)
	}
	f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookURL, "https://hooks.example.test/blog")
	f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	f.st.RecordBlogTest(ctx, f.owner, b.ID, true, "ok", 1)
	bp, _, err := f.st.CreateBlogDraft(ctx, f.tok, store.NewBlogDraft{Site: b.Name, Slug: "sample-week", Title: "Example post", Description: "Invented.", Markdown: "Example opening."}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := f.st.CreateDraft(ctx, f.tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example promo " + store.BlogURLToken}}, Promotes: bp.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Approve(ctx, f.owner, p.ID, p.Version); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Approve(ctx, f.owner, bp.ID, f.get(t, bp.ID).Version); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{}
	d := f.disp(fake)
	f.now = p.ScheduledAt.Add(time.Minute)
	f.run(t, d)
	if n := len(fake.calls); n != 0 {
		t.Fatalf("the promo posted before its blog post is live (%d calls)", n)
	}
	// The blog publishes, and the promo follows.
	due, _ := f.st.DueBlogs(ctx, f.now)
	f.st.Claim(ctx, due[0], f.now.Add(time.Minute))
	url := b.LiveURL("sample-week")
	if err := f.st.FinishBlog(ctx, bp.ID, true, url, "", ""); err != nil {
		t.Fatal(err)
	}
	// The offset counts from the real publish time: not due yet.
	f.run(t, d)
	if n := len(fake.calls); n != 0 {
		t.Fatalf("the promo posted before publish time + offset (%d calls)", n)
	}
	f.now = f.now.Add(2*time.Hour + time.Minute)
	f.run(t, d)
	if len(fake.calls) != 1 || fake.calls[0].Text != "Example promo "+url || strings.Contains(fake.calls[0].Text, "{{") {
		t.Fatalf("posted %+v", fake.calls)
	}
	if got := f.get(t, p.ID); got.Status != store.StatusPosted {
		t.Fatalf("promo %s", got.Status)
	}
}

// A fixed-time promo whose time has passed still waits while its blog
// post publishes late, and goes out once the post is confirmed live.
func TestFixedPromoWaitsForLateBlog(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	b, err := f.st.CreateBlog(ctx, f.owner, store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook})
	if err != nil {
		t.Fatal(err)
	}
	f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookURL, "https://hooks.example.test/blog")
	f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	f.st.RecordBlogTest(ctx, f.owner, b.ID, true, "ok", 1)
	bp, _, err := f.st.CreateBlogDraft(ctx, f.tok, store.NewBlogDraft{Site: b.Name, Slug: "sample-late", Title: "Example post", Description: "Invented.", Markdown: "Example opening."}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	fixedAt := bp.ScheduledAt.Add(20 * time.Minute)
	p, _, err := f.st.CreateDraft(ctx, f.tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example fixed promo " + store.BlogURLToken}}, Promotes: bp.ID, ProposedAt: fixedAt})
	if err != nil {
		t.Fatal(err)
	}
	f.st.Approve(ctx, f.owner, p.ID, p.Version)
	if err := f.st.Approve(ctx, f.owner, bp.ID, f.get(t, bp.ID).Version); err != nil {
		t.Fatal(err)
	}
	fake := &Fake{}
	d := f.disp(fake)
	// The blog is claimed at its slot but is still publishing past the
	// promo's fixed time.
	f.now = bp.ScheduledAt
	due, _ := f.st.DueBlogs(ctx, f.now)
	f.st.Claim(ctx, due[0], f.now.Add(2*time.Hour))
	for _, m := range []time.Duration{21, 45, 90} {
		f.now = bp.ScheduledAt.Add(m * time.Minute)
		f.run(t, d)
		if n := len(fake.calls); n != 0 {
			t.Fatalf("+%d min: the promo posted before its blog post is live", m)
		}
	}
	if err := f.st.FinishBlog(ctx, bp.ID, true, b.LiveURL("sample-late"), "", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, p.ID); !got.ScheduledAt.Equal(fixedAt) {
		t.Fatalf("a fixed promo moved to %v", got.ScheduledAt)
	}
	f.run(t, d)
	if len(fake.calls) != 1 || !strings.Contains(fake.calls[0].Text, b.LiveURL("sample-late")) {
		t.Fatalf("posted %+v", fake.calls)
	}
}
