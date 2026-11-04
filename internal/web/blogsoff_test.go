package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// blogsRig is a server with one tested blog, one pending blog post and one
// pending X post, with the blog feature on or off (XPOST_BLOGS_ENABLED).
// The blog rows are made through the store, so they exist either way.
type blogsRig struct {
	e        *env
	c        *client
	sec      string
	b        *store.Blog
	bid, xid int64
}

func newBlogsRig(t *testing.T, on bool) *blogsRig {
	t.Helper()
	e := newEnv(t, func(c *Config) { c.BlogsEnabled = on })
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead)
	e.st.NoBlogs = false // set up the rows, then hide them again
	b := e.readyBlog("notes.example.test")
	d, _, err := e.st.CreateBlogDraft(context.Background(), tok, store.NewBlogDraft{Site: b.Name, Slug: "sample-week", Title: "Example post: a sample week",
		Description: "Invented one-line summary.", Tags: []string{"sample"}, Markdown: "Sample body.", ProposedAt: testNow.Add(24 * time.Hour)}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	e.st.NoBlogs = !on
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	return &blogsRig{e: e, c: c, sec: sec, b: b, bid: d.ID, xid: xid}
}

// Off (the default for a new install): blog pages and import 404, and the
// queue, its kind chips, the sidebar, Settings, the schedule and the token
// form show nothing about blogs. Agents cannot propose a blog post.
func TestBlogsOff(t *testing.T) {
	g := newBlogsRig(t, false)
	c, id := g.c, strconvI(g.b.ID)
	for _, p := range []string{"/settings/blogs", "/settings/blogs/new", "/settings/blogs/new?method=webhook", "/settings/blogs/" + id + "/import", path(g.bid, "/edit")} {
		if r := c.get(p); r.status != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.status)
		}
	}
	for _, p := range []string{"/settings/blogs", "/settings/blogs/" + id, "/settings/blogs/" + id + "/import", path(g.bid, "/publish"), path(g.bid, "/edit")} {
		if r := c.post("/queue", p, url.Values{"op": {"test"}}); r.status != http.StatusNotFound {
			t.Errorf("POST %s: %d", p, r.status)
		}
	}
	for _, p := range []string{"/queue", "/queue?kind=blog", "/queue?blog=" + id, "/queue?status=approved"} {
		b := c.get(p).body
		if !strings.Contains(p, "approved") {
			mustHave(t, b, `id="d`+strconvI(g.xid)+`"`)
		}
		mustLack(t, b, `id="d`+strconvI(g.bid)+`"`, "sample-week", `class="accts blogs"`, `href="/queue?kind=blog`, ">Blog ", "posts or publishes", "notes.example.test")
	}
	mustHave(t, c.get("/queue").body, "1 waiting")
	set := c.get("/settings").body
	mustHave(t, set, `<a href="/settings/accounts"`)
	mustLack(t, set, `href="/settings/blogs"`)
	lg := c.get("/log").body
	mustLack(t, lg, "blogs:publish", "blogs_publish")
	mustLack(t, c.get("/schedule").body, "kind=blog", "Blog, banner")
	r := g.e.api("POST", "/api/v1/drafts", g.sec, blogBody("notes.example.test", "sample-other", "Sample."))
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "blogs are off on this server") {
		t.Fatalf("blog proposal: %d %s", r.status, r.body)
	}
	// The stored rows are kept, untouched.
	if d := g.e.draft(g.bid); d.Status != store.StatusPending || d.Post == nil {
		t.Fatalf("blog draft changed: %+v", d)
	}
}

// On: the same data shows blogs exactly as before.
func TestBlogsOn(t *testing.T) {
	g := newBlogsRig(t, true)
	c, id := g.c, strconvI(g.b.ID)
	for _, p := range []string{"/settings/blogs", "/settings/blogs/new", "/settings/blogs/" + id + "/import", path(g.bid, "/edit")} {
		if r := c.get(p); r.status != http.StatusOK {
			t.Errorf("GET %s: %d", p, r.status)
		}
	}
	q := c.get("/queue").body
	mustHave(t, q, `id="d`+strconvI(g.bid)+`"`, `id="d`+strconvI(g.xid)+`"`, `class="accts blogs"`, "kind=blog", "posts or publishes", "2 item")
	mustHave(t, c.get("/queue?kind=blog").body, `id="d`+strconvI(g.bid)+`"`)
	mustHave(t, c.get("/settings").body, `href="/settings/blogs"`)
	mustHave(t, c.get("/log").body, "blogs:publish")
	if r := g.e.api("POST", "/api/v1/drafts", g.sec, blogBody("notes.example.test", "sample-other", "Sample.")); r.status != http.StatusCreated {
		t.Fatalf("blog proposal: %d %s", r.status, r.body)
	}
}
