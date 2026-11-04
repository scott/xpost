package gander_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/gander"
	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/store"
)

// An invented token for the fake; never a real one.
const testToken = "example-gander-token-0000000000000000"

var t0 = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

type rig struct {
	t    *testing.T
	fake *gfake.Server
	srv  *httptest.Server
	pub  *gander.Publisher
	blog *store.Blog
	now  time.Time
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, now: t0}
	r.fake = &gfake.Server{Token: testToken, Now: func() time.Time { return r.now }}
	r.srv = httptest.NewTLSServer(r.fake.Handler())
	t.Cleanup(r.srv.Close)
	r.fake.Base = r.srv.URL
	client := r.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	r.pub = &gander.Publisher{HTTP: func() *http.Client { return client }, Now: func() time.Time { return r.now },
		Token: func(context.Context, int64) (string, error) { return testToken, nil }}
	r.blog = &store.Blog{ID: 1, Name: "guide.example.test", SiteURL: r.srv.URL, Method: store.MethodAPI, APIBase: r.srv.URL, HasAPIToken: true}
	return r
}

func samplePost(slug string) blog.Post {
	return blog.Post{DraftID: 7, Slug: slug, Title: "Example post", Description: "Invented one-line summary.", Tags: []string{"sample"},
		Markdown: "Example opening paragraph.", CoverAlt: "An invented sample cover", HasCover: true, Cover: []byte("\x89PNG example bytes"), CoverMIME: "image/png"}
}

// noLeak checks an error names no URL, host, token or response message.
func noLeak(t *testing.T, err error, extra ...string) {
	t.Helper()
	if err == nil {
		return
	}
	for _, bad := range append([]string{"https://", "127.0.0.1", testToken, "Bearer"}, extra...) {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q shows %q", err, bad)
		}
	}
}

func authCalls(c []string) int {
	n := 0
	for _, x := range c {
		if strings.HasSuffix(x, " auth") {
			n++
		}
	}
	return n
}

// Create, publish and confirm: hidden draft first, the UTC date, no tags,
// the API's url (no trailing slash), and at most 4 API calls.
func TestStagePublishConfirm(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := samplePost("sample-guide")
	ref, err := r.pub.Stage(ctx, r.blog, p)
	if err != nil || !strings.HasPrefix(ref, "gander:sample-guide@") {
		t.Fatalf("stage: %q %v", ref, err)
	}
	got, _ := r.fake.Get("sample-guide")
	if !got.Draft || got.Date != "2026-06-01" || got.CoverMIME != "image/png" || got.Title != "Example post" {
		t.Fatalf("staged %+v", got)
	}
	p.Ref = ref
	u, err := r.pub.Publish(ctx, r.blog, p)
	if err != nil || u != r.srv.URL+"/blog/sample-guide" {
		t.Fatalf("publish: %q %v", u, err)
	}
	live, err := r.pub.Live(ctx, r.blog, u)
	if err != nil || !live {
		t.Fatalf("live: %v %v", live, err)
	}
	calls := r.fake.Calls()
	want := []string{"GET /api/blog/posts/sample-guide auth", "PUT /api/blog/posts/sample-guide auth", "POST /api/blog/posts/sample-guide/publish auth",
		"GET /api/blog/posts/sample-guide auth", "GET /blog/sample-guide"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", calls)
	}
	if authCalls(calls) > 4 {
		t.Fatalf("%d API calls for one publish", authCalls(calls))
	}
}

// A post on the slug that xpost did not create is never overwritten, a
// draft or a live one, and a stale ref (re-created post) is not ours.
func TestForeignSlugRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.fake.Seed(gfake.Post{Slug: "taken-slug", Title: "Someone else's", Description: "x", Markdown: "x", Date: "2026-05-01", Draft: true, Cover: []byte("x"), CoverMIME: "image/png"})
	for _, ref := range []string{"", "gander:taken-slug@2020-01-01T00:00:00Z", "fake_7"} {
		p := samplePost("taken-slug")
		p.Ref = ref
		_, err := r.pub.Stage(ctx, r.blog, p)
		if err == nil || !strings.HasPrefix(err.Error(), gander.SlugTaken) {
			t.Fatalf("ref %q: %v", ref, err)
		}
		noLeak(t, err)
	}
	if got, _ := r.fake.Get("taken-slug"); got.Title != "Someone else's" {
		t.Fatal("foreign post was overwritten")
	}
	for _, c := range r.fake.Calls() {
		if strings.HasPrefix(c, "PUT") {
			t.Fatal("a PUT was sent for a foreign slug")
		}
	}
}

// xpost's own recorded ref allows an update of its hidden draft, and a
// post of ours that is already live is left alone (no PUT, no unpublish).
func TestOwnRefAllowed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := samplePost("own-slug")
	ref, err := r.pub.Stage(ctx, r.blog, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Ref, p.Title = ref, "Example post, edited"
	r.now = r.now.Add(time.Hour)
	ref2, err := r.pub.Stage(ctx, r.blog, p)
	if err != nil || ref2 != ref {
		t.Fatalf("restage: %q %v", ref2, err)
	}
	if got, _ := r.fake.Get("own-slug"); got.Title != "Example post, edited" || !got.Draft {
		t.Fatalf("own draft not updated: %+v", got)
	}
	if _, err := r.pub.Publish(ctx, r.blog, p); err != nil {
		t.Fatal(err)
	}
	n := len(r.fake.Calls())
	if ref3, err := r.pub.Stage(ctx, r.blog, p); err != nil || ref3 != ref {
		t.Fatalf("stage of a live own post: %q %v", ref3, err)
	}
	if c := r.fake.Calls()[n:]; len(c) != 1 || !strings.HasPrefix(c[0], "GET ") {
		t.Fatalf("stage of a live own post sent %v", c)
	}
	if got, _ := r.fake.Get("own-slug"); got.Draft {
		t.Fatal("a live post was taken down")
	}
}

// 401, 404 and 429: one call each (no retry), and the note is built from
// the status and gander's code only.
func TestErrorStatuses(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	r.pub.Token = func(context.Context, int64) (string, error) { return "example-wrong-token-1111111111111111", nil }
	_, err := r.pub.Test(ctx, r.blog)
	if err == nil || err.Error() != "Listing posts returned 401 Unauthorized (invalid_token). Check the API token." {
		t.Fatalf("401: %v", err)
	}
	noLeak(t, err, "example-wrong-token", "not recognized")
	if n := len(r.fake.Calls()); n != 1 {
		t.Fatalf("401: %d calls", n)
	}

	r = newRig(t)
	p := samplePost("gone-post")
	ref, _ := r.pub.Stage(ctx, r.blog, p)
	p.Ref = ref
	r.fake.AddFault(gfake.Fault{Op: "publish", Status: 404, Code: "not_found", Message: "post not found"})
	if _, err := r.pub.Publish(ctx, r.blog, p); err == nil || err.Error() != "Publishing returned 404 Not Found (not_found)." {
		t.Fatalf("404: %v", err)
	}

	r = newRig(t)
	ref, _ = r.pub.Stage(ctx, r.blog, p)
	p.Ref = ref
	before := len(r.fake.Calls())
	r.fake.AddFault(gfake.Fault{Op: "publish", Status: 429, Code: "rate_limited", Message: "too many blog API requests", Sticky: true})
	_, err = r.pub.Publish(ctx, r.blog, p)
	if err == nil || !strings.Contains(err.Error(), "429 Too Many Requests (rate_limited)") || !strings.Contains(err.Error(), "rate limited xpost; not retried") {
		t.Fatalf("429: %v", err)
	}
	noLeak(t, err, "too many blog API requests")
	if n := len(r.fake.Calls()) - before; n != 1 {
		t.Fatalf("429 retried: %d calls", n)
	}
}

// gander's invalid_post and invalid_json messages, and unknown codes, are
// never echoed.
func TestInvalidPostTextNotEchoed(t *testing.T) {
	r := newRig(t)
	r.fake.AddFault(gfake.Fault{Op: "put", Status: 400, Code: "invalid_post", Message: "cover required at https://gander.example.test/x token=abc"})
	_, err := r.pub.Stage(context.Background(), r.blog, samplePost("sample-bad"))
	if err == nil || err.Error() != "Staging the post returned 400 Bad Request (invalid_post)." {
		t.Fatalf("got %v", err)
	}
	noLeak(t, err, "cover required", "token=abc")
	r.fake.AddFault(gfake.Fault{Op: "list", Status: 500, Code: "sample_unknown_code", Message: "internal detail"})
	_, err = r.pub.Test(context.Background(), r.blog)
	if err == nil || err.Error() != "Listing posts returned 500 Internal Server Error." {
		t.Fatalf("unknown code: %v", err)
	}
}

// The gander date is the UTC day of the actual publish time: 11:30 PM in
// Denver is already the next day in UTC.
func TestUTCDateAtDayBoundary(t *testing.T) {
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Skip("no zone data")
	}
	r := newRig(t)
	r.now = time.Date(2026, 6, 1, 23, 30, 0, 0, denver)
	if _, err := r.pub.Stage(context.Background(), r.blog, samplePost("late-night")); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.fake.Get("late-night"); got.Date != "2026-06-02" {
		t.Fatalf("date %s, want the UTC day 2026-06-02", got.Date)
	}
}

// Cover type and size, slug and body are checked before anything is sent.
func TestLocalChecksBeforeSending(t *testing.T) {
	r := newRig(t)
	cases := map[string]func(*blog.Post){
		"svg cover":  func(p *blog.Post) { p.CoverMIME = "image/svg+xml" },
		"big cover":  func(p *blog.Post) { p.Cover = make([]byte, gander.MaxCoverBytes+1) },
		"no cover":   func(p *blog.Post) { p.Cover, p.HasCover = nil, false },
		"bad slug":   func(p *blog.Post) { p.Slug = "Bad_Slug" },
		"big body":   func(p *blog.Post) { p.Markdown = strings.Repeat("a", gander.MaxMarkdownBytes+1) },
		"no summary": func(p *blog.Post) { p.Description = " " },
	}
	for name, mod := range cases {
		p := samplePost("sample-check")
		mod(&p)
		if _, err := r.pub.Stage(context.Background(), r.blog, p); err == nil {
			t.Errorf("%s: staged", name)
		}
	}
	if n := len(r.fake.Calls()); n != 0 {
		t.Fatalf("%d calls for posts that fail local checks", n)
	}
}

// Live is false for a draft, a future UTC date, or a public page that is
// not 200; a redirect is not followed.
func TestLiveChecks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	u := r.srv.URL + "/blog/sample-live"
	r.fake.Seed(gfake.Post{Slug: "sample-live", Title: "t", Description: "d", Markdown: "m", Date: "2026-06-02", Draft: false, Cover: []byte("x"), CoverMIME: "image/png"})
	if live, err := r.pub.Live(ctx, r.blog, u); err != nil || live {
		t.Fatalf("future date: %v %v", live, err)
	}
	r.now = r.now.Add(24 * time.Hour)
	r.fake.AddFault(gfake.Fault{Op: "page", Slug: "sample-live", Status: http.StatusFound})
	if live, err := r.pub.Live(ctx, r.blog, u); err != nil || live {
		t.Fatalf("redirected page: %v %v", live, err)
	}
	if live, err := r.pub.Live(ctx, r.blog, u); err != nil || !live {
		t.Fatalf("live page: %v %v", live, err)
	}
	for _, c := range r.fake.Calls() {
		if strings.HasPrefix(c, "GET /blog/") && strings.HasSuffix(c, " auth") {
			t.Fatal("the public page check sent the token")
		}
	}
}

// No client, a non-https or non-root API base, or an unreachable host:
// a short error with no address in it, and nothing sent.
func TestRefusals(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	nc := &gander.Publisher{Token: r.pub.Token}
	if _, err := nc.Test(ctx, r.blog); err != gander.ErrNoClient {
		t.Fatalf("no client: %v", err)
	}
	for _, base := range []string{"http://guide.example.test", r.srv.URL + "/api/blog", "https://guide.example.test?x=1"} {
		b := *r.blog
		b.APIBase = base
		if _, err := r.pub.Test(ctx, &b); err == nil || !strings.Contains(err.Error(), "site root") {
			t.Errorf("%s: %v", base, err)
		}
	}
	if n := len(r.fake.Calls()); n != 0 {
		t.Fatalf("%d calls", n)
	}
	b := *r.blog
	r.srv.Close()
	_, err := r.pub.Test(ctx, &b)
	if err == nil || !strings.HasPrefix(err.Error(), "Listing posts could not connect.") {
		t.Fatalf("down: %v", err)
	}
	noLeak(t, err)
}

func TestTestPasses(t *testing.T) {
	r := newRig(t)
	d, err := r.pub.Test(context.Background(), r.blog)
	if err != nil || d != "Listed posts, 200 OK (0 published). Nothing was changed." {
		t.Fatalf("%q %v", d, err)
	}
	if c := r.fake.Calls(); len(c) != 1 || c[0] != "GET /api/blog/posts auth" {
		t.Fatalf("calls %v", c)
	}
}

// A live address that does not parse, or is not on the blog's site
// (host compared without case, port-aware), is not live and makes no call.
func TestLiveAddressChecks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	host := strings.TrimPrefix(r.srv.URL, "https://") // 127.0.0.1:port
	hostOnly, port, _ := strings.Cut(host, ":")
	for _, c := range []struct{ live, want string }{
		{"https://%zz/blog/sample-live", "The live address is not a gander post page."},
		{"https://" + hostOnly + ":1/blog/sample-live", "The live address is not on the blog's site, so it was not checked. Nothing is marked live."},
		{"https://" + hostOnly + "/blog/sample-live", "The live address is not on the blog's site, so it was not checked. Nothing is marked live."},
		{"https://other.example.test:" + port + "/blog/sample-live", "The live address is not on the blog's site, so it was not checked. Nothing is marked live."},
		{"https://user@" + host + "/blog/sample-live", "The live address is not a gander post page."},
	} {
		live, err := r.pub.Live(ctx, r.blog, c.live)
		if live || err == nil || err.Error() != c.want {
			t.Errorf("%q: %v %v", c.live, live, err)
		}
		if err != nil && (strings.Contains(err.Error(), "example.test") || strings.Contains(err.Error(), hostOnly)) {
			t.Errorf("%q: error shows the address", c.live)
		}
	}
	if n := len(r.fake.Calls()); n != 0 {
		t.Fatalf("%d calls for addresses that fail local checks", n)
	}
	// A matching host still passes (the existing live checks cover the full path).
	r.fake.Seed(gfake.Post{Slug: "sample-live", Title: "t", Description: "d", Markdown: "m", Date: "2026-06-01", Cover: []byte("x"), CoverMIME: "image/png"})
	if live, err := r.pub.Live(ctx, r.blog, r.srv.URL+"/blog/sample-live"); err != nil || !live {
		t.Fatalf("same host: %v %v", live, err)
	}
}

// List is one authenticated GET. A post the API lists that is still a
// draft or dated after today (gander should leave both out) is reported as
// not live, with no URL or ref, so it is never imported.
func TestListImportRead(t *testing.T) {
	r := newRig(t)
	created := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	r.fake.Seed(gfake.Post{Slug: "sample-live", Title: "Example live", Description: "Invented.", Date: "2026-05-01", CreatedAt: created})
	r.fake.Seed(gfake.Post{Slug: "sample-hidden", Title: "Example hidden", Description: "Invented.", Date: "2026-05-01", Draft: true})
	ups, err := r.pub.List(context.Background(), r.blog)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || !ups[0].Live || ups[0].Ref != "gander:sample-live@"+created.Format(time.RFC3339) || ups[0].URL != r.srv.URL+"/blog/sample-live" || ups[0].Title != "Example live" {
		t.Fatalf("%+v", ups)
	}
	if c := r.fake.Calls(); len(c) != 1 || c[0] != "GET /api/blog/posts auth" {
		t.Fatalf("calls %v", c)
	}
	// An API that lists a draft and a future post anyway.
	raw := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"posts":[{"slug":"sample-a","title":"A","date":"2026-05-01","draft":true,"created_at":"2026-05-01T00:00:00Z"},
			{"slug":"sample-b","title":"B","date":"2026-09-01","draft":false,"created_at":"2026-05-01T00:00:00Z"},
			{"slug":"sample-c","title":"C","date":"2026-05-01","draft":false}]}`))
	}))
	defer raw.Close()
	cl := raw.Client()
	r.pub.HTTP = func() *http.Client { return cl }
	b := *r.blog
	b.SiteURL, b.APIBase = raw.URL, raw.URL
	ups, err = r.pub.List(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range ups {
		if u.Live || u.URL != "" || u.Ref != "" || u.Why == "" {
			t.Errorf("%s counted as live: %+v", u.Slug, u)
		}
	}
	// A failing list names only the status.
	r.pub.HTTP = func() *http.Client { return r.srv.Client() }
	r.fake.AddFault(gfake.Fault{Op: "list", Status: 500, Code: "sample_error", Message: "invented " + r.srv.URL})
	_, err = r.pub.List(context.Background(), r.blog)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("fault: %v", err)
	}
	noLeak(t, err, "invented")
}

// ImportRead allows the published list only with the query exactly
// "published=1"; every variant, a fragment or an escaped path is refused.
func TestImportReadExactQuery(t *testing.T) {
	const base = "https://gander.example.test/api/blog/posts"
	get := func(s string) *http.Request {
		t.Helper()
		r, err := http.NewRequest(http.MethodGet, s, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+testToken)
		return r
	}
	if !gander.ImportRead(get(base + "?published=1")) {
		t.Fatal("exact query refused")
	}
	for _, q := range []string{"", "?", "?published=0", "?published=true", "?Published=1", "?published=%31", "?published%3D1",
		"?published=1&", "?published=1&x=1", "?x=1&published=1", "?published=1&published=1", "?published=1;x", "?published=1#",
		"?published=1#top", "?published=1?", "?published= 1", "#published=1"} {
		r := get(base + q)
		if gander.ImportRead(r) && r.URL.String() != base+"?published=1" {
			t.Errorf("%q allowed", q)
		}
	}
	r := get(base + "?published=1")
	r.URL.RawFragment = "top"
	if gander.ImportRead(r) {
		t.Error("RawFragment allowed")
	}
	for _, s := range []string{"https://gander.example.test/api/blog/posts/?published=1", "https://gander.example.test/api/blog/%70osts?published=1"} {
		if gander.ImportRead(get(s)) {
			t.Errorf("%q allowed", s)
		}
	}
	r = get(base + "?published=1")
	r.Header.Del("Authorization")
	if gander.ImportRead(r) {
		t.Error("no Bearer allowed")
	}
}

// Live with no blog or on a nil Publisher returns an error, not a panic,
// and makes no call.
func TestLiveNilBlogOrPublisher(t *testing.T) {
	r := newRig(t)
	live := r.srv.URL + "/blog/sample-week"
	call := func(name string, f func() (bool, error)) {
		t.Helper()
		defer func() {
			if x := recover(); x != nil {
				t.Fatalf("%s panicked: %v", name, x)
			}
		}()
		ok, err := f()
		if ok || err == nil || err.Error() != "The live check has no blog or publisher set up, so nothing was checked. Nothing is marked live." {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
		noLeak(t, err)
	}
	call("nil blog", func() (bool, error) { return r.pub.Live(context.Background(), nil, live) })
	var nilPub *gander.Publisher
	call("nil publisher", func() (bool, error) { return nilPub.Live(context.Background(), r.blog, live) })
	if c := r.fake.Calls(); len(c) != 0 {
		t.Fatalf("calls %v", c)
	}
}
