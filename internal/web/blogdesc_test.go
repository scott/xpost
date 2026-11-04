package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

// desc returns an invented one-line description of exactly n characters.
func desc(n int) string {
	s := strings.Repeat("Invented sample summary words. ", n/31+1)[:n]
	return s[:n-1] + "x" // no trailing space for the store to trim
}

func TestDescFixture(t *testing.T) {
	for _, n := range []int{300, 301, 565, 1000, 1001} {
		if d := desc(n); len([]rune(d)) != n || strings.ContainsAny(d, "\r\n") || strings.TrimSpace(d) != d {
			t.Fatalf("desc(%d) is %d characters", n, len([]rune(d)))
		}
	}
}

// The description limit is 1000 at intake (the API), the owner's edit and
// the editor's maxlength; 1001 is refused. The title limit stays 200.
func TestBlogDescriptionLimit(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("example.test")
	if store.MaxDescription != 1000 || store.MaxTitle != 200 {
		t.Fatalf("limits %d %d", store.MaxDescription, store.MaxTitle)
	}
	body := blogBody("example.test", "sample-long", "Example opening.")
	body["description"] = desc(1000)
	r := e.api("POST", "/api/v1/drafts", sec, body)
	if r.status != http.StatusCreated {
		t.Fatalf("1000: %d %s", r.status, r.body)
	}
	id := decodeBlog(t, r).ID
	if d := e.draft(id); d.Post.Description != desc(1000) {
		t.Fatal("the 1000-character description was not stored as sent")
	}
	body = blogBody("example.test", "sample-longer", "Example opening.")
	body["description"] = desc(1001)
	if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest || !strings.Contains(r.body, "description: one line, 1 to 1000 characters") {
		t.Fatalf("1001: %d %s", r.status, r.body)
	}
	// Still one line.
	body = blogBody("example.test", "sample-lines", "Example opening.")
	body["description"] = "Invented line one.\nInvented line two."
	if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest {
		t.Fatalf("two lines: %d %s", r.status, r.body)
	}
	// The editor allows 1000 and its edit refuses 1001.
	pg := c.get(path(id, "/edit")).body
	if !strings.Contains(pg, `id="description" name="description" class="oneline" rows="2" maxlength="1000"`) || !strings.Contains(pg, `id="title" name="title" class="oneline" rows="2" maxlength="200"`) {
		t.Fatal("editor maxlength")
	}
	form := func(d string) url.Values {
		return e.ownerForm(id, url.Values{"title": {"Example post: a sample week"}, "description": {d}, "slug": {"/sample-long"},
			"tags": {"sample"}, "markdown": {"Example opening."}, "date": {"2026-06-03"}, "time": {"07:00"}, "op": {"save"}})
	}
	if r := c.post(path(id, "/edit"), path(id, "/edit"), form(desc(1001))); r.status == http.StatusSeeOther || !strings.Contains(r.body, "1 to 1000 characters") {
		t.Fatalf("edit 1001: %d", r.status)
	}
	if r := c.post(path(id, "/edit"), path(id, "/edit"), form(desc(999))); r.status != http.StatusSeeOther {
		t.Fatalf("edit 999: %d %s", r.status, r.body)
	}
}

// A description over 300 characters only warns, with the exact text, on
// intake, the card and the editor; Approve still works.
func TestBlogLongDescriptionWarns(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("example.test")
	body := blogBody("example.test", "sample-long", "Example opening.")
	body["description"] = desc(301)
	r := e.api("POST", "/api/v1/drafts", sec, body)
	o := decodeBlog(t, r)
	if r.status != http.StatusCreated || len(o.Blog.Lint) != 1 || o.Blog.Lint[0].Rule != "long_description" || o.Blog.Lint[0].Level != "warn" ||
		o.Blog.Lint[0].Where != "the description" || o.Blog.Lint[0].Message != "Long description: search results may cut it off" {
		t.Fatalf("301: %d %s", r.status, r.body)
	}
	id := o.ID
	body = blogBody("example.test", "sample-short", "Example opening.")
	body["description"] = desc(300)
	if o := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, body)); len(o.Blog.Lint) != 0 {
		t.Fatalf("300 warned: %+v", o.Blog.Lint)
	}
	for _, p := range []string{"/queue?kind=blog", path(id, "/edit")} {
		if pg := c.get(p).body; !strings.Contains(pg, "Long description: search results may cut it off") {
			t.Errorf("%s lacks the warning", p)
		}
	}
	if r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, nil)); r.status != http.StatusSeeOther {
		t.Fatalf("approve with the warning: %d %s", r.status, r.body)
	}
	if d := e.draft(id); d.Status != store.StatusApproved {
		t.Fatalf("status %s", d.Status)
	}
	if blogtext.LongDescription != 300 {
		t.Fatalf("warning starts at %d", blogtext.LongDescription)
	}
}

// gander descriptions run to several hundred characters: a 565-character
// one-line description imports as New; over 1000 or two lines is skipped.
func TestImportLongDescription(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", true)
	fake.Seed(gfake.Post{Slug: "sample-long", Title: "Example long", Description: desc(565), Date: "2026-05-20"})
	fake.Seed(gfake.Post{Slug: "sample-max", Title: "Example max", Description: desc(1000), Date: "2026-05-21"})
	fake.Seed(gfake.Post{Slug: "sample-over", Title: "Example over", Description: desc(1001), Date: "2026-05-22"})
	fake.Seed(gfake.Post{Slug: "sample-lines", Title: "Example lines", Description: "Invented line one.\nInvented line two.", Date: "2026-05-23"})
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	r := c.post(page, page, url.Values{"op": {"dry"}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Dry run:</b> 2 to import, 2 skipped.") {
		t.Fatalf("dry: %d", r.status)
	}
	if n := strings.Count(r.body, "The description is more than one line or too long."); n != 2 {
		t.Fatalf("%d skipped for the description", n)
	}
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {importKey(t, r.body)}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Imported 2 posts") {
		t.Fatalf("apply: %d", r.status)
	}
	for slug, n := range map[string]int{"sample-long": 565, "sample-max": 1000} {
		var id int64
		e.st.DB.QueryRow(`SELECT draft_id FROM blog_posts WHERE blog_id = ? AND slug = ?`, b.ID, slug).Scan(&id)
		if d := e.draft(id); d.Status != store.StatusPosted || d.Source != "import" || d.Post.Description != desc(n) {
			t.Fatalf("%s: %s %q", slug, d.Status, d.Source)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM blog_posts WHERE blog_id = ? AND slug IN ('sample-over', 'sample-lines')`, b.ID); n != 0 {
		t.Fatalf("%d skipped posts imported", n)
	}
	onlyGets(t, fake.Calls())
}
