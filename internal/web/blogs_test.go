package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

func (e *env) readyBlog(name string) *store.Blog {
	e.t.Helper()
	ctx := context.Background()
	b, err := e.st.CreateBlog(ctx, e.owner, store.BlogIn{Name: name, SiteURL: "https://" + name, Method: store.MethodWebhook, Topics: []string{"sample banned topic"}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogHookURL, "https://hooks.example.test/blog")
	e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	if err := e.st.RecordBlogTest(ctx, e.owner, b.ID, true, "Test passed. Nothing was published.", 1); err != nil {
		e.t.Fatal(err)
	}
	b, _ = e.st.BlogByID(ctx, b.ID)
	return b
}

func blogBody(site, slug, md string) map[string]any {
	return map[string]any{"kind": "blog", "site": site, "slug": slug, "title": "Example post: a sample week", "description": "Invented one-line summary.",
		"tags": []string{"sample"}, "markdown": md}
}

type blogOut struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	Account    string  `json:"account"`
	Status     string  `json:"status"`
	ProposedAt string  `json:"proposed_at"`
	Scheduled  *string `json:"scheduled_at"`
	Blog       *struct {
		Site  string `json:"site"`
		Slug  string `json:"slug"`
		Words int    `json:"words"`
		Lint  []struct {
			Rule, Level, Where, Message, Excerpt string
		} `json:"lint"`
	} `json:"blog"`
	Error string `json:"error"`
	Lint  []struct {
		Rule, Level, Where, Message string
	} `json:"lint"`
}

func decodeBlog(t *testing.T, r resp) blogOut {
	t.Helper()
	var o blogOut
	if err := json.Unmarshal([]byte(r.body), &o); err != nil {
		t.Fatalf("decode %d %s: %v", r.status, r.body, err)
	}
	return o
}

func TestBlogIntakeAPI(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	_, readOnly := e.token("sample-reader", store.ScopeLogRead)
	e.readyBlog("example.test")

	r := e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "sample-week", "Example opening.\n\nSecond paragraph."))
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	o := decodeBlog(t, r)
	// Default time: the next 7:00 AM in the owner's zone (UTC here).
	if o.Kind != "blog" || o.Account != "example.test" || o.Status != "pending" || o.Scheduled != nil || o.ProposedAt != "2026-06-02T07:00:00Z" {
		t.Fatalf("draft %+v", o)
	}
	if o.Blog == nil || o.Blog.Slug != "sample-week" || o.Blog.Words != 4 || len(o.Blog.Lint) != 0 {
		t.Fatalf("blog %+v", o.Blog)
	}
	// An em dash blocks with 422 and the findings.
	r = e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "dash", "Intro.\n\nA clause\u2014and another."))
	o = decodeBlog(t, r)
	if r.status != http.StatusUnprocessableEntity || o.Error != "Fix 1 lint error to approve or publish." || len(o.Lint) != 1 || o.Lint[0].Rule != "em_dash" || o.Lint[0].Level != "block" || o.Lint[0].Where != "paragraph 2" {
		t.Fatalf("em dash: %d %s", r.status, r.body)
	}
	// Banned topic too; an en dash only warns.
	if r = e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "topic", "About a SAMPLE BANNED TOPIC.")); r.status != http.StatusUnprocessableEntity {
		t.Fatalf("banned: %d %s", r.status, r.body)
	}
	r = e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "years", "Sample years 2019\u20132024."))
	o = decodeBlog(t, r)
	if r.status != http.StatusCreated || len(o.Blog.Lint) != 1 || o.Blog.Lint[0].Level != "warn" || !strings.Contains(o.Blog.Lint[0].Message, "Warning only, nothing to confirm.") {
		t.Fatalf("en dash: %d %s", r.status, r.body)
	}
	for name, body := range map[string]any{
		"kind":         map[string]any{"kind": "page"},
		"x fields":     map[string]any{"kind": "blog", "site": "example.test", "slug": "a", "title": "t", "description": "d", "markdown": "m", "account": "sample_writer"},
		"blog fields":  map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "Example"}}, "proposed_at": "2026-06-01T18:00:00Z", "markdown": "m"},
		"unknown site": blogBody("nope.test", "a", "m"),
		"slug":         blogBody("example.test", "Not A Slug", "m"),
		"dup slug":     blogBody("example.test", "sample-week", "m"),
		"bad time":     map[string]any{"kind": "blog", "site": "example.test", "slug": "t", "title": "t", "description": "d", "markdown": "m", "proposed_at": "tomorrow"},
	} {
		if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	if r := e.api("POST", "/api/v1/drafts", readOnly, blogBody("example.test", "ro", "m")); r.status == http.StatusCreated {
		t.Fatal("blog intake without drafts:write")
	}
	// X drafts say kind x; lists carry both.
	id := e.propose(sec, "sample_writer", testNow.Add(3*time.Hour), "Example X post")
	r = e.api("GET", "/api/v1/drafts", sec, nil)
	if !strings.Contains(r.body, `"kind":"x"`) || !strings.Contains(r.body, `"kind":"blog"`) || id == 0 {
		t.Fatalf("list: %s", r.body)
	}
}

func TestBlogQueueAndPreview(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	xid := e.propose(sec, "sample_writer", testNow.Add(3*time.Hour), "Example X post")
	// Without blogs the queue is as it was: no kind filter, account chips.
	q := c.get("/queue")
	if strings.Contains(q.body, `aria-label="Kind"`) || !strings.Contains(q.body, `aria-label="Account"`) || strings.Contains(q.body, "blog posts") {
		t.Fatal("queue changed for an install without blogs")
	}
	b := e.readyBlog("example.test")
	md := "Example opening paragraph.\n\n<script>alert(1)</script>\n\n## Sample heading\n\n[a link](https://example.com/sample)"
	o := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "sample-week", md)))
	id := o.ID
	q = c.get("/queue")
	for _, want := range []string{"X posts and blog posts", `aria-label="Kind"`, "Read full post", "Lint passed", "/sample-week", "Publish now", "example.test"} {
		if !strings.Contains(q.body, want) {
			t.Errorf("queue lacks %q", want)
		}
	}
	if strings.Contains(q.body, path(id, "/rewrite")) {
		t.Fatal("Rewrite on a blog card")
	}
	if strings.Contains(q.body, `aria-label="Account"`) {
		t.Fatal("account chips under All with blogs set up")
	}
	if xq := c.get("/queue?kind=x"); strings.Contains(xq.body, `id="d`+strconvI(id)+`"`) || !strings.Contains(xq.body, `id="d`+strconvI(xid)+`"`) || !strings.Contains(xq.body, `aria-label="Account"`) {
		t.Fatal("X filter")
	}
	if bq := c.get("/queue?kind=blog"); !strings.Contains(bq.body, `id="d`+strconvI(id)+`"`) || strings.Contains(bq.body, `id="d`+strconvI(xid)+`"`) {
		t.Fatal("Blog filter")
	}
	// Preview: sanitized Markdown.
	pv := c.get(path(id, ""))
	if pv.status != http.StatusOK || !strings.Contains(pv.body, "<h2>Sample heading</h2>") || strings.Contains(pv.body, "<script>alert") || !strings.Contains(pv.body, `rel="nofollow noreferrer noopener"`) && !strings.Contains(pv.body, `rel="nofollow`) {
		t.Fatalf("preview: %d", pv.status)
	}
	// The X editor and Rewrite refuse blog posts.
	if r := c.post("/queue", path(id, ""), e.ownerForm(id, url.Values{"part0": {"x"}})); r.status != http.StatusNotFound {
		t.Fatalf("X edit on a blog post: %d", r.status)
	}
	if r := c.post("/queue", path(id, "/rewrite"), e.ownerForm(id, nil)); r.status == http.StatusSeeOther {
		t.Fatal("rewrite on a blog post")
	}
	// No publisher on this server: Publish now is off and refused.
	if r := c.post("/queue", path(id, "/publish"), e.ownerForm(id, nil)); r.status != http.StatusConflict || !strings.Contains(r.body, "not available on this server") {
		t.Fatalf("publish without a publisher: %d", r.status)
	}
	// Lint that blocks (rules can change after intake) turns Approve off.
	e.st.UpdateBlog(context.Background(), e.owner, b.ID, store.BlogIn{Name: b.Name, SiteURL: b.SiteURL, Method: b.Method, Topics: []string{"opening"}})
	q = c.get("/queue?kind=blog")
	if !strings.Contains(q.body, "Fix 1 lint error to approve or publish.") || !strings.Contains(q.body, "Lint found 1 error. Approve and Publish now are off until it is fixed.") {
		t.Fatal("lint blocked card")
	}
	if r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, nil)); r.status != http.StatusConflict {
		t.Fatalf("approve with lint error: %d", r.status)
	}
}

func TestBlogEditAndPublishNow(t *testing.T) {
	woke := 0
	e := newEnv(t, func(c *Config) { c.BlogWake = func() { woke++ } })
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("example.test")
	id := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "sample-week", "Example opening."))).ID
	// Edit: new text and slug, back to pending.
	edit := func(slug string) resp {
		return c.post(path(id, "/edit"), path(id, "/edit"), e.ownerForm(id, url.Values{"title": {"New sample title"}, "description": {"New line."}, "slug": {"/" + slug},
			"tags": {"one, two"}, "markdown": {"Body.\r\n\r\nMore."}, "date": {"2026-06-03"}, "time": {"07:00"}, "op": {"save"}}))
	}
	if r := edit("renamed-week"); r.status != http.StatusSeeOther {
		t.Fatalf("edit: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if d.Post.Title != "New sample title" || d.Post.Slug != "renamed-week" || len(d.Post.Tags) != 2 || d.Post.Markdown != "Body.\n\nMore." || d.ScheduledAt.UTC().Format(time.RFC3339) != "2026-06-03T07:00:00Z" {
		t.Fatalf("saved %+v %v", d.Post, d.ScheduledAt)
	}
	// Once staged the slug locks.
	e.st.MarkStaged(context.Background(), id, "ref-1")
	if r := edit("third-name"); r.status != http.StatusBadRequest || !strings.Contains(r.body, "slug is locked") {
		t.Fatalf("staged slug: %d", r.status)
	}
	if r := c.get(path(id, "/edit")); !strings.Contains(r.body, "readonly") || !strings.Contains(r.body, "Locked: this post is staged on example.test.") {
		t.Fatal("edit page slug not locked")
	}
	// Preview tab renders without saving.
	if r := c.post(path(id, "/edit"), path(id, "/edit"), e.ownerForm(id, url.Values{"title": {"t"}, "description": {"d"}, "slug": {"renamed-week"}, "markdown": {"## Unsaved"}, "date": {"2026-06-03"}, "time": {"07:00"}, "op": {"preview"}})); r.status != http.StatusOK || !strings.Contains(r.body, "<h2>Unsaved</h2>") {
		t.Fatalf("preview tab: %d", r.status)
	}
	if e.draft(id).Post.Markdown != "Body.\n\nMore." {
		t.Fatal("preview saved")
	}
	// Publish now: the confirm, then approved for now and the publisher woken.
	if r := c.get("/queue?kind=blog&publish=" + strconvI(id)); !strings.Contains(r.body, "Publish this post now?") || !strings.Contains(r.body, "Publishes right away and replaces the scheduled slot.") {
		t.Fatal("confirm")
	}
	if r := c.post("/queue", path(id, "/publish"), e.ownerForm(id, url.Values{"return": {"/queue?kind=blog"}})); r.status != http.StatusSeeOther {
		t.Fatalf("publish now: %d %s", r.status, r.body)
	}
	d = e.draft(id)
	if d.Status != store.StatusApproved || !d.ScheduledAt.Equal(testNow) || woke != 1 {
		t.Fatalf("after publish now: %s %v woke %d", d.Status, d.ScheduledAt, woke)
	}
}
