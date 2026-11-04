package gander_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

func pngBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The dispatcher end to end against the fake: published with the API's
// url, a foreign slug blocked, a 429 failed once with a note, and no
// failure text naming a URL.
func TestDispatcherThroughGander(t *testing.T) {
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
	r := newRig(t)
	st.Now = func() time.Time { return r.now }
	r.pub.Now = st.Now
	r.pub.Token = func(ctx context.Context, id int64) (string, error) { return st.BlogSecret(ctx, id, store.BlogAPIToken) }
	owner, _, _ := st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	tok, _, _ := st.CreateAgentToken(ctx, owner, "sample-agent", []string{store.ScopeDraftsWrite})
	b, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "guide.example.test", SiteURL: r.srv.URL, Method: store.MethodAPI, APIBase: r.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetBlogSecret(ctx, owner, b.ID, store.BlogAPIToken, testToken); err != nil {
		t.Fatal(err)
	}
	b, _ = st.BlogByID(ctx, b.ID)
	detail, err := r.pub.Test(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	st.RecordBlogTest(ctx, owner, b.ID, true, detail, 1)
	ref, _, err := st.AddMedia(ctx, tok, pngBytes(t, 120, 63))
	if err != nil {
		t.Fatal(err)
	}
	mk := func(slug string) *store.Draft {
		dr, _, err := st.CreateBlogDraft(ctx, tok, store.NewBlogDraft{Site: "guide.example.test", Slug: slug, Title: "Example " + slug, Description: "Sample.",
			Tags: []string{"sample"}, Markdown: "Body.", Cover: &store.MediaIn{Ref: ref, Alt: "An invented cover"}, ProposedAt: r.now.Add(time.Hour)}, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Approve(ctx, owner, dr.ID, dr.Version); err != nil {
			t.Fatal(err)
		}
		return dr
	}
	good, taken, limited := mk("sample-one"), mk("sample-taken"), mk("sample-limited")
	r.fake.Seed(gfake.Post{Slug: "sample-taken", Title: "Not ours", Description: "x", Markdown: "x", Date: "2026-05-01", Cover: []byte("x"), CoverMIME: "image/png"})
	r.fake.AddFault(gfake.Fault{Op: "publish", Slug: "sample-limited", Status: 429, Code: "rate_limited", Message: "too many blog API requests"})
	r.now = r.now.Add(2 * time.Hour)
	dp := &blog.Dispatcher{Store: st, Publisher: blog.Methods{API: r.pub}}
	if n, err := dp.RunDue(ctx, r.now); err != nil || n != 1 {
		t.Fatalf("run: %d %v", n, err)
	}
	g, _ := st.Draft(ctx, good.ID)
	if g.Status != store.StatusPosted || g.Post.PublishedURL != r.srv.URL+"/blog/sample-one" || !strings.HasPrefix(g.Post.Ref, "gander:sample-one@") {
		t.Fatalf("published: %s %+v", g.Status, g.Post)
	}
	if fp, _ := r.fake.Get("sample-one"); fp.Draft || len(fp.Cover) == 0 || fp.CoverMIME != "image/png" {
		t.Fatalf("fake post %+v", fp)
	}
	g, _ = st.Draft(ctx, taken.ID)
	if g.Status != store.StatusFailed || !strings.Contains(g.Error, store.BlogSlugTaken) || g.Post.Staged() {
		t.Fatalf("taken: %s %q", g.Status, g.Error)
	}
	if fp, _ := r.fake.Get("sample-taken"); fp.Title != "Not ours" {
		t.Fatal("foreign post overwritten")
	}
	g, _ = st.Draft(ctx, limited.ID)
	if g.Status != store.StatusFailed || !strings.Contains(g.Error, "429") || !strings.Contains(g.Error, "rate limited") {
		t.Fatalf("limited: %s %q", g.Status, g.Error)
	}
	publishes := 0
	for _, c := range r.fake.Calls() {
		if c == "POST /api/blog/posts/sample-limited/publish auth" {
			publishes++
		}
	}
	if publishes != 1 {
		t.Fatalf("429 publish sent %d times", publishes)
	}
	for _, id := range []int64{taken.ID, limited.ID} {
		g, _ := st.Draft(ctx, id)
		if strings.Contains(g.Error, "https://") || strings.Contains(g.Error, "127.0.0.1") {
			t.Errorf("failure text names an address: %q", g.Error)
		}
	}
	// Nothing due now: a second pass sends nothing (no polling, no retry).
	n := len(r.fake.Calls())
	if _, err := dp.RunDue(ctx, r.now.Add(time.Hour)); err != nil || len(r.fake.Calls()) != n {
		t.Fatalf("second pass made calls: %v", r.fake.Calls()[n:])
	}
}
