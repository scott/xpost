package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/blog/hookpub/hfake"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/web"
)

// XPOST_BLOG_IMPORT: off unless exactly "1"; unset or empty is off, and
// anything else (also "0") refuses to start. XPOST_BLOG_LIVE=1 alone does
// not turn the import on.
func TestBlogImportSwitchFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "1": true} {
		if got, err := blogImportFromEnv(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"0", " 1", "1 ", "on", "true", "yes", "2", "01"} {
		if _, err := blogImportFromEnv(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	dbp := testEnv(t)
	recordDispatch(t)
	t.Setenv(EnvBlogsEnabled, "1") // blogs are optional; these switches need them on
	t.Setenv(EnvBlogImport, "0")
	if code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies"); code == 0 || !strings.Contains(logs, "XPOST_BLOG_IMPORT must be unset, empty or 1") {
		t.Fatalf("0: exit %d %s", code, logs)
	}
	t.Setenv(EnvBlogImport, "")
	t.Setenv(EnvBlogLive, "1")
	if code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies"); code != 0 || !strings.Contains(logs, "blog import off (XPOST_BLOG_IMPORT not 1)") {
		t.Fatalf("live alone: exit %d %s", code, logs)
	}
	t.Setenv(EnvBlogLive, "")
	t.Setenv(EnvBlogImport, "1")
	if code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies"); code != 0 || !strings.Contains(logs, "blog import on (XPOST_BLOG_IMPORT=1)") {
		t.Fatalf("on: exit %d %s", code, logs)
	}
	if w := wireBlogs(nil, schedOff, true, false, "test", io.Discard); w.importer != nil {
		t.Fatal("XPOST_BLOG_LIVE=1 alone wired the import")
	}
}

// With XPOST_BLOG_IMPORT=1 and XPOST_BLOG_LIVE off, serve's real wiring
// lets only the import's GETs leave: the gander published list (with the
// token) and a webhook blog's index and post pages. Test, Publish now,
// the dispatcher and Retry make no call, and even calling publish, wake
// and staging methods on the import's own readers sends nothing.
func TestImportOnLiveOffSendsOnlyImportReads(t *testing.T) {
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
	const tok = "example-gander-token-0000000000000000"
	gf := &gfake.Server{Token: tok, Now: time.Now}
	gs := httptest.NewTLSServer(gf.Handler())
	defer gs.Close()
	gf.Base = gs.URL
	created := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	gf.Seed(gfake.Post{Slug: "sample-live", Title: "Example live", Description: "Invented.", Date: "2026-05-01", CreatedAt: created})
	hf := &hfake.Server{Key: "sample-signing-key-0000000000000000"}
	hs := httptest.NewTLSServer(hf.Handler())
	defer hs.Close()
	hf.Serve("/blog/posts.json", "application/json", `[{"slug":"sample-page","title":"Example page","date":"2026-05-02","description":"Invented."}]`)
	hf.Deploy("/blog/sample-page/", hfake.Page("Example page", "", "<p>Invented.</p>"))

	owner, _, err := st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []string{schedFake, schedOn, schedOff} {
		w := wireBlogs(st, mode, false, true, "test", io.Discard)
		var wake func()
		if w.pub != nil {
			wake = func() {}
		}
		srv, err := web.New(st, web.Config{BaseURL: "http://localhost:8090", WebhookHTTP: gs.Client(), BlogTest: w.test, BlogImport: w.importer, ImportAllow: importAllowed, BlogWake: wake})
		if err != nil {
			t.Fatal(err)
		}
		w.attach(srv)
		// Both fakes share httptest's one certificate authority.
		suffix := string(rune('a' + i))
		api, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "guide" + suffix + ".example.test", SiteURL: gs.URL, Method: store.MethodAPI, APIBase: gs.URL})
		if err != nil {
			t.Fatal(err)
		}
		st.SetBlogSecret(ctx, owner, api.ID, store.BlogAPIToken, tok)
		hook, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "notes" + suffix + ".example.test", SiteURL: hs.URL, Method: store.MethodWebhook})
		if err != nil {
			t.Fatal(err)
		}
		st.SetBlogSecret(ctx, owner, hook.ID, store.BlogHookURL, hs.URL+"/hook")
		st.SetBlogSecret(ctx, owner, hook.ID, store.BlogHookKey, hf.Key)
		api, _ = st.BlogByID(ctx, api.ID)
		hook, _ = st.BlogByID(ctx, hook.ID)

		// The import's reads go through.
		ups, err := w.importer(ctx, api)
		if err != nil || len(ups) != 1 || !ups[0].Live {
			t.Fatalf("%s: gander import %v %+v", mode, err, ups)
		}
		ups, err = w.importer(ctx, hook)
		if err != nil || len(ups) != 1 || !ups[0].Live {
			t.Fatalf("%s: webhook import %v %+v", mode, err, ups)
		}
		// Test makes no call (live off).
		for _, b := range []*store.Blog{api, hook} {
			if _, err := w.test(ctx, b); b.Method == store.MethodAPI && !errors.Is(err, store.ErrLiveOff) {
				t.Errorf("%s: test %s: %v", mode, b.Name, err)
			}
		}
		// The publishing side: never a real adapter with live off.
		if m, ok := w.pub.(blog.Methods); ok {
			if m.Hook != nil || m.API == blog.Publisher(w.ganderPub) || m.API == blog.Publisher(w.ganderImport) {
				t.Fatalf("%s: live off but a real adapter publishes: %#v", mode, m)
			}
		} else if w.pub != nil {
			t.Fatalf("%s: publisher %#v", mode, w.pub)
		}
		// Publish, Retry and the dispatcher, end to end.
		if w.pub != nil {
			agent, _, _ := st.CreateAgentToken(ctx, owner, "sample-agent-"+suffix, []string{store.ScopeDraftsWrite})
			for _, b := range []*store.Blog{api, hook} {
				st.RecordBlogTest(ctx, owner, b.ID, true, "Test passed. Nothing was published.", 1)
				dr, _, err := st.CreateBlogDraft(ctx, agent, store.NewBlogDraft{Site: b.Name, Slug: "sample-" + suffix, Title: "Example", Description: "Sample.", Markdown: "Body.", ProposedAt: time.Now().Add(time.Hour)}, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				if err := st.Approve(ctx, owner, dr.ID, dr.Version); err != nil {
					t.Fatal(err)
				}
				dr, _ = st.Draft(ctx, dr.ID)
				if err := st.PublishNow(ctx, owner, dr.ID, dr.Version); err != nil {
					t.Fatal(err)
				}
				// It fails once (as if the blog refused), then Retry runs
				// the dispatcher: through the fake, never a blog host.
				due, _ := st.DueBlogs(ctx, time.Now().Add(time.Minute))
				for _, x := range due {
					if x.ID == dr.ID {
						st.Claim(ctx, x, time.Now().Add(time.Minute))
					}
				}
				if err := st.FinishBlog(ctx, dr.ID, false, "", "", "Example failure."); err != nil {
					t.Fatal(err)
				}
				dr, _ = st.Draft(ctx, dr.ID)
				if err := st.PublishNow(ctx, owner, dr.ID, dr.Version); err != nil { // Retry
					t.Fatal(err)
				}
				dp := &blog.Dispatcher{Store: st, Publisher: w.pub}
				dp.RunDue(ctx, time.Now().Add(time.Minute))
				if dr, _ = st.Draft(ctx, dr.ID); dr.Status != store.StatusPosted {
					t.Errorf("%s: %s retry ended %s", mode, b.Name, dr.Status)
				}
			}
		}
		// Even the import's own readers can't publish, stage, wake or test.
		p := blog.Post{Slug: "sample-live", Title: "Example", Description: "Sample.", Markdown: "Body.", Cover: []byte("x"), CoverMIME: "image/png", Ref: "gander:sample-live@" + created.Format(time.RFC3339)}
		if _, err := w.ganderImport.Stage(ctx, api, p); err == nil {
			t.Errorf("%s: the import reader staged a post", mode)
		}
		if _, err := w.ganderImport.Publish(ctx, api, p); err == nil {
			t.Errorf("%s: the import reader published", mode)
		}
		if _, err := w.ganderImport.Live(ctx, api, gs.URL+"/blog/sample-live"); err == nil {
			t.Errorf("%s: the import reader checked a live page", mode)
		}
		if err := w.hookImport.Wake(ctx, hook, blog.Wake{Event: "blog.publish", PublishID: 1}); err == nil {
			t.Errorf("%s: the import reader sent a wake", mode)
		}
		if _, err := w.hookImport.Test(ctx, hook); err == nil {
			t.Errorf("%s: the import reader sent a test wake", mode)
		}
	}
	for _, c := range gf.Calls() {
		if c != "GET /api/blog/posts auth" {
			t.Errorf("a non-import gander request: %q", c)
		}
	}
	if n := len(gf.Calls()); n != 3 {
		t.Errorf("%d gander calls for three imports", n)
	}
	if w := hf.Wakes(); len(w) != 0 {
		t.Errorf("%d wakes", len(w))
	}
	for _, g := range hf.Gets() {
		if g != "GET /blog/posts.json auth=false" && g != "GET /blog/sample-page/ auth=false" {
			t.Errorf("a non-import site request: %q", g)
		}
	}
	if n := len(hf.Gets()); n != 6 {
		t.Errorf("%d site reads for three imports", n)
	}
}
