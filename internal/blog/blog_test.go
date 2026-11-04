package blog_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

var t0 = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

func TestDispatcherPublishesThroughFake(t *testing.T) {
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	k, _ := secret.NewKey()
	st := store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	now := t0
	st.Now = func() time.Time { return now }
	owner, _, _ := st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	tok, _, _ := st.CreateAgentToken(ctx, owner, "sample-agent", []string{store.ScopeDraftsWrite})
	b, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "example.test", SiteURL: "https://example.test", Method: store.MethodWebhook})
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlogSecret(ctx, owner, b.ID, store.BlogHookURL, "https://hooks.example.test/x")
	st.SetBlogSecret(ctx, owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	fp := &blog.FakePublisher{}
	b, _ = st.BlogByID(ctx, b.ID)
	detail, err := fp.Test(ctx, b)
	if err != nil || detail != "Test passed. Nothing was published." {
		t.Fatalf("test: %q %v", detail, err)
	}
	st.RecordBlogTest(ctx, owner, b.ID, true, detail, 1)

	mk := func(slug string) *store.Draft {
		dr, _, err := st.CreateBlogDraft(ctx, tok, store.NewBlogDraft{Site: "example.test", Slug: slug, Title: "Example " + slug, Description: "Sample.", Markdown: "Body."}, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Approve(ctx, owner, dr.ID, dr.Version); err != nil {
			t.Fatal(err)
		}
		return dr
	}
	ok1, bad := mk("sample-one"), mk("sample-two")
	fp.Fail = map[string]string{"sample-two": "Sample host said no."}
	dp := &blog.Dispatcher{Store: st, Publisher: fp}
	// Not due yet: nothing happens.
	if n, _ := dp.RunDue(ctx, now); n != 0 || len(fp.Calls()) != 1 {
		t.Fatalf("early run: %d %v", n, fp.Calls())
	}
	now = now.Add(24 * time.Hour)
	n, err := dp.RunDue(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("run: %d %v", n, err)
	}
	g, _ := st.Draft(ctx, ok1.ID)
	if g.Status != store.StatusPosted || g.Post.PublishedURL != "https://example.test/blog/sample-one/" || g.Post.Ref == "" || !g.Post.Staged() {
		t.Fatalf("published: %s %+v", g.Status, g.Post)
	}
	g, _ = st.Draft(ctx, bad.ID)
	if g.Status != store.StatusFailed || g.Error != "Sample host said no." || !g.Post.Staged() {
		t.Fatalf("failed: %s %q", g.Status, g.Error)
	}
	want := "test example.test,stage example.test/sample-one,publish example.test/sample-one,live https://example.test/blog/sample-one/,stage example.test/sample-two,publish example.test/sample-two"
	if got := strings.Join(fp.Calls(), ","); got != want {
		t.Fatalf("calls:\n%s\nwant\n%s", got, want)
	}
	// A run with nothing due does nothing.
	if n, _ := dp.RunDue(ctx, now); n != 0 {
		t.Fatal("published twice")
	}
}
