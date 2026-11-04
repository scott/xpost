package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/blog/hookpub/hfake"
	"github.com/scott/xpost/internal/store"
)

var importKeyRe = regexp.MustCompile(`name="key" value="([0-9a-f]+)"`)

func importKey(t *testing.T, body string) string {
	t.Helper()
	m := importKeyRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no import key on the dry run page")
	}
	return m[1]
}

func (e *env) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.DB.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// blockedOn makes a blog draft on slug that failed because the slug is
// taken on the blog (the Blocked card).
func blockedOn(t *testing.T, e *env, b *store.Blog, sec, slug, title string) int64 {
	t.Helper()
	ctx := context.Background()
	if err := e.st.RecordBlogTest(ctx, e.owner, b.ID, true, "Test passed. Nothing was published.", 1); err != nil {
		t.Fatal(err)
	}
	body := blogBody(b.Name, slug, "Example opening.")
	body["title"] = title
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, body))
	if bo.ID == 0 {
		t.Fatalf("intake: %+v", bo)
	}
	e.approveNow(bo.ID)
	d := e.draft(bo.ID)
	if err := e.st.PublishNow(ctx, e.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	due, _ := e.st.DueBlogs(ctx, e.clock())
	for _, x := range due {
		if x.ID == bo.ID {
			e.st.Claim(ctx, x, e.clock().Add(time.Minute))
		}
	}
	if err := e.st.FinishBlog(ctx, bo.ID, false, "", "", store.BlogSlugTaken+" xpost will not overwrite a post it did not create."); err != nil {
		t.Fatal(err)
	}
	return bo.ID
}

func onlyGets(t *testing.T, calls []string) {
	t.Helper()
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("import made a %q call", c)
		}
	}
}

// gander: the dry run reports and writes nothing; the import adds live
// posts as Published with xpost's ref, adopts a blocked draft whose post
// is live, leaves a hand-staged hidden draft blocked, and a rerun creates
// and changes nothing.
func TestImportGander(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", true)
	_, sec := e.token("sample-agent")
	created := time.Date(2026, 5, 20, 16, 0, 0, 0, time.UTC)
	fake.Seed(gfake.Post{Slug: "sample-week", Title: "Example week", Description: "An invented summary.", Date: "2026-05-20", CreatedAt: created})
	fake.Seed(gfake.Post{Slug: "sample-blocked", Title: "Example blocked post", Description: "Invented.", Date: "2026-05-22", CreatedAt: created})
	fake.Seed(gfake.Post{Slug: "sample-hidden", Title: "Example hidden post", Description: "Invented.", Date: "2026-05-23", Draft: true})
	fake.Seed(gfake.Post{Slug: "sample-later", Title: "Example later post", Description: "Invented.", Date: "2026-07-01"})
	fake.Seed(gfake.Post{Slug: "sample-held", Title: "Example held post", Description: "Invented.", Date: "2026-05-24"})
	blocked := blockedOn(t, e, b, sec, "sample-blocked", "Example blocked post")
	hidden := blockedOn(t, e, b, sec, "sample-hidden", "Example hidden post")
	held := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody(b.Name, "sample-held", "Example opening."))).ID
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	if r := c.get(page); r.status != http.StatusOK || !strings.Contains(r.body, "Dry run") || strings.Contains(r.body, `name="key"`) {
		t.Fatalf("page: %d", r.status)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("opening the page made %d calls", n)
	}
	before := e.count(`SELECT COUNT(*) FROM drafts`)
	r := c.post(page, page, url.Values{"op": {"dry"}})
	if r.status != http.StatusOK {
		t.Fatalf("dry: %d", r.status)
	}
	for _, want := range []string{"Dry run:</b> 1 to import, 1 to adopt, 1 skipped.", "Example week", "Live on the site and not in xpost yet.",
		"is blocked by this live post with the same title", "uses this slug in xpost.", "Import 2 posts"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dry run lacks %q", want)
		}
	}
	if strings.Contains(r.body, "sample-later") || strings.Contains(r.body, "sample-hidden") {
		t.Error("the dry run lists a post gander does not publish")
	}
	if n := e.count(`SELECT COUNT(*) FROM drafts`); n != before {
		t.Fatalf("the dry run wrote %d drafts", n-before)
	}
	key := importKey(t, r.body)
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {key}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Imported 1 post and adopted 1 blocked draft as Published.") {
		t.Fatalf("apply: %d", r.status)
	}
	var id int64
	e.st.DB.QueryRow(`SELECT draft_id FROM blog_posts WHERE blog_id = ? AND slug = 'sample-week'`, b.ID).Scan(&id)
	d := e.draft(id)
	ref := "gander:sample-week@" + created.Format(time.RFC3339)
	if d.Status != store.StatusPosted || d.Source != "import" || d.TokenID != 0 || d.Post.ImportedAt == nil || d.Post.Ref != ref ||
		d.Post.PublishedURL != fake.Base+"/blog/sample-week" || d.Post.Markdown != "" || d.Post.Title != "Example week" {
		t.Fatalf("imported: %s %q %d %v %q %q", d.Status, d.Source, d.TokenID, d.Post.ImportedAt, d.Post.Ref, d.Post.PublishedURL)
	}
	if got := d.ScheduledAt.UTC().Format("2006-01-02"); got != "2026-05-20" {
		t.Fatalf("imported day %s", got)
	}
	ad := e.draft(blocked)
	if ad.Status != store.StatusPosted || ad.Error != "" || ad.Post.Ref != "gander:sample-blocked@"+created.Format(time.RFC3339) || ad.Post.PublishedURL != fake.Base+"/blog/sample-blocked" || ad.Post.ImportedAt != nil {
		t.Fatalf("adopted: %s %q %q", ad.Status, ad.Error, ad.Post.Ref)
	}
	if hd := e.draft(hidden); hd.Status != store.StatusFailed || !strings.HasPrefix(hd.Error, store.BlogSlugTakenPrefix) {
		t.Fatalf("the hand-staged draft's slug is not blocked any more: %s", hd.Status)
	}
	if hd := e.draft(held); hd.Status != store.StatusPending {
		t.Fatalf("held draft: %s", hd.Status)
	}
	// Nothing to publish, promote or wake: no promo, no X post, no PUT.
	if n := e.count(`SELECT COUNT(*) FROM drafts WHERE promotes IS NOT NULL OR kind != 'blog'`); n != 0 {
		t.Fatalf("%d promo or X drafts", n)
	}
	onlyGets(t, fake.Calls())
	if n := len(fake.Calls()); n != 2 {
		t.Fatalf("%d calls for a dry run and an import", n)
	}
	// The queue shows the imported post as Published, with no body.
	q := c.get("/queue?kind=blog&status=posted").body
	for _, want := range []string{"Imported from the site", "Published on May 20, 2026.", "the text stays there", ">Imported</span>"} {
		if !strings.Contains(q, want) {
			t.Errorf("queue lacks %q", want)
		}
	}
	pv := c.get("/drafts/" + strconvI(id)).body
	if !strings.Contains(pv, "xpost keeps only the title, description, date and link") || strings.Contains(pv, "End of post") {
		t.Error("the preview of an imported post")
	}
	// A rerun creates and changes nothing, and an upstream change is only
	// reported.
	n := e.count(`SELECT COUNT(*) FROM drafts`)
	fake.Seed(gfake.Post{Slug: "sample-week", Title: "Example week, renamed", Description: "An invented summary.", Date: "2026-05-20", CreatedAt: created})
	r = c.post(page, page, url.Values{"op": {"dry"}})
	for _, want := range []string{"0 to import, 3 skipped", "Already in xpost as published post #" + strconvI(id) + ". Nothing is changed.", "Changed on the site: title. xpost keeps its own copy."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("rerun lacks %q", want)
		}
	}
	if strings.Contains(r.body, `name="key"`) {
		t.Error("a rerun with nothing to import offers Import")
	}
	// The old key is refused: the plan changed.
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {key}})
	if r.status != http.StatusConflict || !strings.Contains(r.body, "changed since the dry run") {
		t.Fatalf("stale key: %d", r.status)
	}
	if m := e.count(`SELECT COUNT(*) FROM drafts`); m != n {
		t.Fatalf("a rerun added %d drafts", m-n)
	}
	if d := e.draft(id); d.Post.Title != "Example week" {
		t.Fatalf("a rerun changed the title to %q", d.Post.Title)
	}
	for _, bad := range []string{ganderTok} {
		if strings.Contains(r.body, bad) || strings.Contains(e.logs.String(), bad) {
			t.Error("the token leaked")
		}
	}
}

// Import off (XPOST_BLOG_IMPORT not 1), even with live publishing on: the
// page says so, Dry run is off, and a post makes no call.
func TestImportOff(t *testing.T) {
	e, fake, b, c := ganderEnvOpts(t, "http://localhost:8090", true, false)
	fake.Seed(gfake.Post{Slug: "sample-week", Title: "Example week", Description: "Invented.", Date: "2026-05-20"})
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	g := c.get(page)
	if !strings.Contains(g.body, "Import is off on this server. Dry run stays off, and no call is made.") || !strings.Contains(g.body, `value="dry" disabled`) {
		t.Fatalf("off page: %d", g.status)
	}
	r := c.post(page, page, url.Values{"op": {"dry"}})
	if r.status != http.StatusOK || !strings.Contains(r.body, "Import is off on this server, so it can&#39;t read the site. No call was made and nothing was imported.") {
		t.Fatalf("off: %d", r.status)
	}
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {"0123"}})
	if !strings.Contains(r.body, "No call was made") {
		t.Fatal("off apply")
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("off made %d calls", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM drafts`); n != 0 {
		t.Fatalf("off wrote %d drafts", n)
	}
}

// Import on, live publishing off: the import reads, and Settings' Test
// still makes no call.
func TestImportOnLiveOff(t *testing.T) {
	e, fake, b, c := ganderEnvOpts(t, "http://localhost:8090", false, true)
	fake.Seed(gfake.Post{Slug: "sample-week", Title: "Example week", Description: "Invented.", Date: "2026-05-20"})
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	r := c.post(page, page, url.Values{"op": {"dry"}})
	if !strings.Contains(r.body, "1 to import") {
		t.Fatalf("dry: %d", r.status)
	}
	c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}})
	if got := fake.Calls(); len(got) != 1 || got[0] != "GET /api/blog/posts auth" {
		t.Fatalf("calls %v", got)
	}
	_ = e
}

// A gander error is status only, and nothing is written.
func TestImportGanderError(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", true)
	fake.AddFault(gfake.Fault{Op: "list", Status: 503, Code: "sample_down", Message: "an invented message " + fake.Base})
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	r := c.post(page, page, url.Values{"op": {"dry"}})
	if r.status != http.StatusBadGateway || !strings.Contains(r.body, "Listing posts returned 503") || strings.Contains(r.body, "invented message") || strings.Contains(r.body, strings.TrimPrefix(fake.Base, "https://")) {
		t.Fatalf("error: %d", r.status)
	}
	if n := e.count(`SELECT COUNT(*) FROM drafts`); n != 0 {
		t.Fatalf("%d drafts", n)
	}
}

// The plan's rules: one per slug, real days only, and a blocked draft is
// adopted only with the same title and no promos.
func TestImportPlanRules(t *testing.T) {
	e, _, b, _ := ganderEnv(t, "http://localhost:8090", true)
	_, sec := e.token("sample-agent")
	ctx := context.Background()
	other := blockedOn(t, e, b, sec, "sample-renamed", "Example draft title")
	promoted := blockedOn(t, e, b, sec, "sample-promoted", "Example promoted post")
	x := e.propose(sec, "sample_writer", e.clock().Add(48*time.Hour), "Example promo text.")
	if _, err := e.st.DB.Exec(`UPDATE drafts SET promotes = ? WHERE id = ?`, promoted, x); err != nil {
		t.Fatal(err)
	}
	live := func(slug, title, day string) store.UpstreamPost {
		return store.UpstreamPost{Slug: slug, Title: title, Date: day, URL: "https://guide.example.test/blog/" + slug, Ref: "gander:" + slug + "@2026-05-01T00:00:00Z", Live: true}
	}
	plan, err := e.st.PlanImport(ctx, b, []store.UpstreamPost{
		live("sample-one", "Example one", "2026-05-01"),
		live("sample-one", "Example one again", "2026-05-02"),
		live("sample-bad-day", "Example", "May 1"),
		live("Not A Slug", "Example", "2026-05-01"),
		live("sample-renamed", "Example live title", "2026-05-01"),
		live("sample-promoted", "Example promoted post", "2026-05-01"),
		{Slug: "sample-off", Title: "Example", Date: "2026-05-01", Why: "its page returned 404."},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"import", "skip:more than once", "skip:YYYY-MM-DD", "skip:slug xpost can't use", "skip:title differs", "skip:has promos", "skip:Not live: its page returned 404."}
	if len(plan.Lines) != len(want) {
		t.Fatalf("%d lines", len(plan.Lines))
	}
	for i, w := range want {
		act, why, _ := strings.Cut(w, ":")
		if ln := plan.Lines[i]; ln.Action != act || !strings.Contains(ln.Reason, why) {
			t.Errorf("line %d: %s %q, want %s %q", i, ln.Action, ln.Reason, act, why)
		}
	}
	if plan.New != 1 || plan.Adopted != 0 || plan.Skip != 6 {
		t.Fatalf("counts %d %d %d", plan.New, plan.Adopted, plan.Skip)
	}
	if e.draft(other).Status != store.StatusFailed || e.draft(promoted).Status != store.StatusFailed {
		t.Fatal("a plan changed a draft")
	}
	if _, err := e.st.ApplyImport(ctx, e.owner, b, nil, "", nil); err != store.ErrImportChanged {
		t.Fatalf("no key: %v", err)
	}
	many := make([]store.UpstreamPost, store.MaxImport+1)
	if _, err := e.st.PlanImport(ctx, b, many); err != store.ErrImportTooMany {
		t.Fatalf("too many: %v", err)
	}
}

// A webhook blog: one GET of the post index, then one GET of each page,
// all on the blog's site with no secret; a page that is missing or
// redirects is skipped. No wake is sent.
func TestImportWebhookSite(t *testing.T) {
	g := newHookRig(t)
	e, f, b, c := g.e, g.f, g.b, g.c
	f.Serve("/blog/posts.json", "application/json", `[
		{"slug":"sample-one","title":"Example one","date":"2026-05-10","description":"Invented one."},
		{"slug":"sample-two","title":"Example two","date":"2026-05-11","description":"Invented two."},
		{"slug":"sample-moved","title":"Example moved","date":"2026-05-12","description":"Invented."},
		{"slug":"../Not a slug","title":"Example","date":"2026-05-12","description":"Invented."}]`)
	f.Deploy("/blog/sample-one/", hfake.Page("Example one", "", "<p>Invented body.</p>"))
	f.Redirect("/blog/sample-moved/")
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	r := c.post(page, page, url.Values{"op": {"dry"}})
	for _, want := range []string{"1 to import, 3 skipped", "Not live: its page returned 404.", "Not live: its page redirects.", "slug xpost can&#39;t use"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dry run lacks %q", want)
		}
	}
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {importKey(t, r.body)}})
	if !strings.Contains(r.body, "Imported 1 post as Published.") {
		t.Fatalf("apply: %d", r.status)
	}
	var id int64
	e.st.DB.QueryRow(`SELECT draft_id FROM blog_posts WHERE blog_id = ? AND slug = 'sample-one'`, b.ID).Scan(&id)
	d := e.draft(id)
	if d.Status != store.StatusPosted || d.Post.PublishedURL != g.ts.URL+"/blog/sample-one/" || d.Post.Ref != "" || d.Post.ImportedAt == nil {
		t.Fatalf("imported %s %q %q", d.Status, d.Post.PublishedURL, d.Post.Ref)
	}
	if n := len(f.Wakes()); n != 1 { // the rig's own Settings test
		t.Fatalf("import sent %d wakes", n-1)
	}
	gets := f.Gets()
	if len(gets) != 8 {
		t.Fatalf("gets %v", gets)
	}
	for _, x := range gets {
		if strings.HasSuffix(x, "auth=true") || strings.Contains(x, "Not") || strings.Contains(x, "..") {
			t.Fatalf("get %q", x)
		}
	}
	if n := e.count(`SELECT COUNT(*) FROM blog_publishes`); n != 0 {
		t.Fatalf("%d publishes", n)
	}
	// Rerun: nothing new.
	r = c.post(page, page, url.Values{"op": {"dry"}})
	if !strings.Contains(r.body, "0 to import, 4 skipped") {
		t.Fatal("rerun")
	}
	// Index faults are status only.
	f.Serve("/blog/posts.json", "text/html", "<p>invented</p>")
	if r = c.post(page, page, url.Values{"op": {"dry"}}); !strings.Contains(r.body, "The post index is not JSON. Nothing was imported.") {
		t.Fatalf("not json: %d", r.status)
	}
	f.Serve("/blog/posts.json", "application/json", `{"posts":"invented"}`)
	if r = c.post(page, page, url.Values{"op": {"dry"}}); !strings.Contains(r.body, "The post index is not a list of posts.") {
		t.Fatalf("not a list: %d", r.status)
	}
	if strings.Contains(r.body, strings.TrimPrefix(g.ts.URL, "https://")) || strings.Contains(r.body, pubKey) || strings.Contains(r.body, pubAuth) {
		t.Fatal("the page shows an address or secret")
	}
}

// The Settings page links to the import, and the import is owner only.
func TestImportOwnerOnly(t *testing.T) {
	e, fake, b, c := ganderEnv(t, "http://localhost:8090", true)
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	if s := c.get("/settings/blogs").body; !strings.Contains(s, `href="`+page+`"`) || !strings.Contains(s, "Import existing posts") {
		t.Fatal("no link in Settings")
	}
	anon := e.client()
	if r := anon.get(page); r.status == http.StatusOK {
		t.Fatalf("signed out: %d", r.status)
	}
	if r := anon.postRaw(page, url.Values{"op": {"dry"}}, nil); r.status == http.StatusOK {
		t.Fatalf("signed out post: %d", r.status)
	}
	// No CSRF token: refused.
	if r := c.postRaw(page, url.Values{"op": {"dry"}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("no csrf: %d", r.status)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("%d calls", n)
	}
	if r := c.get("/settings/blogs/999/import"); r.status != http.StatusNotFound {
		t.Fatalf("unknown blog: %d", r.status)
	}
}

// The import-only client refuses, before connecting, every request the
// import's check does not approve: no wake, no write, no other path.
func TestImportOnlyClientRefuses(t *testing.T) {
	n := 0
	ts := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { n++ }))
	defer ts.Close()
	e := newEnv(t, func(c *Config) {
		c.BaseURL = "http://localhost:8090"
		c.WebhookHTTP = ts.Client()
		c.ImportAllow = importAllowed
	})
	c := e.srv.BlogImportHTTP()
	for _, r := range []struct{ method, path, auth string }{
		{"POST", "/hook", ""}, {"PUT", "/api/blog/posts/sample", "Bearer x"}, {"GET", "/api/blog/posts/sample", "Bearer x"},
		{"GET", "/api/blog/posts", "Bearer x"}, {"GET", "/api/blog/posts?published=1", ""}, {"GET", "/blog/sample/", "Bearer x"},
		{"GET", "/blog/Not-A-Slug/", ""}, {"GET", "/blog/posts.json?x=1", ""}, {"GET", "/other", ""}, {"POST", "/blog/posts.json", ""},
		// Site reads: a bare "?", a query, a fragment, an escaped path.
		{"GET", "/blog/posts.json?", ""}, {"GET", "/blog/sample-one/?", ""}, {"GET", "/blog/sample-one/?x", ""},
		{"GET", "/blog/sample-one/#top", ""}, {"GET", "/blog/posts.json#top", ""}, {"GET", "/blog/%73ample-one/", ""},
		// The gander list: only exactly published=1.
		{"GET", "/api/blog/posts?", "Bearer x"}, {"GET", "/api/blog/posts?published=0", "Bearer x"},
		{"GET", "/api/blog/posts?published=1&x=1", "Bearer x"}, {"GET", "/api/blog/posts?x=1&published=1", "Bearer x"},
		{"GET", "/api/blog/posts?Published=1", "Bearer x"}, {"GET", "/api/blog/posts?published=%31", "Bearer x"},
		{"GET", "/api/blog/posts?published=1&", "Bearer x"}, {"GET", "/api/blog/posts?published=1#top", "Bearer x"},
		{"GET", "/api/blog/posts/?published=1", "Bearer x"},
	} {
		req, _ := http.NewRequest(r.method, ts.URL+r.path, nil)
		if r.auth != "" {
			req.Header.Set("Authorization", r.auth)
		}
		if res, err := c.Do(req); err == nil {
			res.Body.Close()
			t.Errorf("%s %s auth=%v went through", r.method, r.path, r.auth != "")
		}
	}
	if n != 0 {
		t.Fatalf("%d refused requests reached the server", n)
	}
	// The import's own reads go through.
	for _, r := range []struct{ path, auth string }{{"/api/blog/posts?published=1", "Bearer x"}, {"/blog/posts.json", ""}, {"/blog/sample-one/", ""}} {
		req, _ := http.NewRequest("GET", ts.URL+r.path, nil)
		if r.auth != "" {
			req.Header.Set("Authorization", r.auth)
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s refused: %v", r.path, err)
		}
		res.Body.Close()
	}
	// With no check, nothing goes through.
	e2 := newEnv(t, func(c *Config) { c.BaseURL = "http://localhost:8090"; c.WebhookHTTP = ts.Client() })
	req, _ := http.NewRequest("GET", ts.URL+"/blog/posts.json", nil)
	if _, err := e2.srv.BlogImportHTTP().Do(req); err == nil {
		t.Fatal("no ImportAllow, but a request went through")
	}
}

// A blocked webhook-blog draft is adopted under gander's rules: the same
// title, no promos, and its page live (200 HTML at the exact /blog/<slug>/).
// Its ref stays as xpost had it; the live URL and adopted_at record the
// match. A title mismatch or a missing page is skipped.
func TestImportAdoptsBlockedWebhookDraft(t *testing.T) {
	g := newHookRig(t)
	e, f, b, c := g.e, g.f, g.b, g.c
	ctx := context.Background()
	mk := func(slug, title string) int64 {
		body := blogBody(b.Name, slug, "Example opening.")
		body["title"] = title
		bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", g.writer, body))
		e.approveNow(bo.ID)
		d := e.draft(bo.ID)
		if err := e.st.PublishNow(ctx, e.owner, d.ID, d.Version); err != nil {
			t.Fatal(err)
		}
		due, _ := e.st.DueBlogs(ctx, e.clock())
		for _, x := range due {
			if x.ID == bo.ID {
				e.st.Claim(ctx, x, e.clock().Add(time.Minute))
			}
		}
		if err := e.st.FinishBlog(ctx, bo.ID, false, "", "", store.BlogSlugTakenPrefix+b.Name+". The agent never touches a post xpost did not publish."); err != nil {
			t.Fatal(err)
		}
		return bo.ID
	}
	same := mk("sample-same", "Example same title")
	other := mk("sample-other", "Example draft title")
	gone := mk("sample-gone", "Example gone")
	before := e.draft(same).Post.Ref
	f.Serve("/blog/posts.json", "application/json", `[{"slug":"sample-same","title":"Example same title","date":"2026-05-10","description":"Invented."},
		{"slug":"sample-other","title":"Example live title","date":"2026-05-11","description":"Invented."},
		{"slug":"sample-gone","title":"Example gone","date":"2026-05-12","description":"Invented."}]`)
	f.Deploy("/blog/sample-same/", hfake.Page("Example", "", "<p>Invented.</p>"))
	f.Deploy("/blog/sample-other/", hfake.Page("Example", "", "<p>Invented.</p>"))
	page := "/settings/blogs/" + strconvI(b.ID) + "/import"
	r := c.post(page, page, url.Values{"op": {"dry"}})
	for _, want := range []string{"0 to import, 1 to adopt, 2 skipped", "is blocked by this live post with the same title", "its title differs from the live post", "Not live: its page returned 404."} {
		if !strings.Contains(r.body, want) {
			t.Errorf("dry run lacks %q", want)
		}
	}
	nw := len(f.Wakes())
	r = c.post(page, page, url.Values{"op": {"apply"}, "key": {importKey(t, r.body)}})
	if !strings.Contains(r.body, "adopted 1 blocked draft") {
		t.Fatalf("apply: %d", r.status)
	}
	d := e.draft(same)
	if d.Status != store.StatusPosted || d.Error != "" || d.Post.PublishedURL != g.ts.URL+"/blog/sample-same/" || d.Post.AdoptedAt == nil || d.Post.ImportedAt != nil || d.Post.Ref != before {
		t.Fatalf("adopted: %s %q %q %v ref %q (was %q)", d.Status, d.Error, d.Post.PublishedURL, d.Post.AdoptedAt, d.Post.Ref, before)
	}
	for _, id := range []int64{other, gone} {
		if x := e.draft(id); x.Status != store.StatusFailed {
			t.Errorf("draft %d: %s", id, x.Status)
		}
	}
	if len(f.Wakes()) != nw {
		t.Fatal("the import sent a wake")
	}
	if q := c.get("/queue?kind=blog&status=posted").body; !strings.Contains(q, "Matched to the live post by import.") {
		t.Error("the adopted card lacks its marker")
	}
}
