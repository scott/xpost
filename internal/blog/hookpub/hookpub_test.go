package hookpub_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/hookpub"
	"github.com/scott/xpost/internal/blog/hookpub/hfake"
	"github.com/scott/xpost/internal/store"
)

const (
	key  = "sample-signing-key-0000000000000000"
	auth = "Bearer example-routine-value-0000"
)

var t0 = time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)

// rig is a hookpub.Publisher pointed at a TLS fake agent and site. Its
// client refuses redirects like the guarded one.
func rig(t *testing.T, withAuth bool) (*hookpub.Publisher, *hfake.Server, *httptest.Server, *store.Blog) {
	f := &hfake.Server{Key: key}
	ts := httptest.NewTLSServer(f.Handler())
	t.Cleanup(ts.Close)
	sec := map[string]string{store.BlogHookURL: ts.URL + "/hook", store.BlogHookKey: key}
	if withAuth {
		sec[store.BlogHookAuth] = auth
	}
	p := &hookpub.Publisher{Now: func() time.Time { return t0 }, NewID: func() string { return "d_sample0001" },
		HTTP: func() *http.Client {
			c := ts.Client()
			c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			return c
		},
		Secret: func(_ context.Context, _ int64, field string) (string, error) { return sec[field], nil }}
	return p, f, ts, &store.Blog{ID: 1, Name: "notes.example.test", Method: store.MethodWebhook, SiteURL: ts.URL}
}

func TestWakeIsThinAndSigned(t *testing.T) {
	p, f, ts, b := rig(t, true)
	exp := t0.Add(time.Hour)
	if err := p.Wake(context.Background(), b, blog.Wake{Event: blog.EventPublish, PublishID: 812, Version: 6, DeliveryID: "d_sample0002", ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	w := f.Wakes()
	if len(w) != 1 || !w[0].SigOK || w[0].Event != "blog.publish" || w[0].Delivery != "d_sample0002" || w[0].Auth != auth {
		t.Fatalf("wake %+v", w)
	}
	want := `{"event":"blog.publish","publish_id":812,"version":6,"delivery_id":"d_sample0002","expires_at":"2026-11-05T15:00:00Z"}`
	if w[0].Raw != want {
		t.Fatalf("body %s", w[0].Raw)
	}
	// No Authorization value saved: none is sent.
	p2, f2, _, b2 := rig(t, false)
	if _, err := p2.Test(context.Background(), b2); err != nil {
		t.Fatal(err)
	}
	if w := f2.Wakes(); len(w) != 1 || w[0].Auth != "" || w[0].Event != "blog.test" || !w[0].SigOK || !strings.Contains(w[0].Raw, `"publish_id":0`) {
		t.Fatalf("test wake %+v", w)
	}
	_ = ts
}

func TestWakeErrorsNameOnlyTheStatus(t *testing.T) {
	p, f, ts, b := rig(t, true)
	for code, want := range map[int]string{401: "HTTP 401. Check the Authorization header. Nothing more was sent.", 500: "HTTP 500. Nothing more was sent.",
		302: "HTTP 302: the webhook redirects, and xpost does not follow redirects. Nothing more was sent."} {
		f.SetStatus(code)
		err := p.Wake(context.Background(), b, blog.Wake{Event: blog.EventPublish, PublishID: 1, Version: 1, DeliveryID: "d_x", ExpiresAt: t0})
		if err == nil || err.Error() != want {
			t.Errorf("%d: %v", code, err)
		}
		if err != nil && (strings.Contains(err.Error(), ts.URL) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), auth)) {
			t.Errorf("%d: error shows a secret or URL", code)
		}
	}
	ts.Close()
	if err := p.Wake(context.Background(), b, blog.Wake{Event: blog.EventPublish, DeliveryID: "d_y", ExpiresAt: t0}); err == nil || err.Error() != "no connection. Nothing more was sent." {
		t.Errorf("closed: %v", err)
	}
}

func TestLiveCheck(t *testing.T) {
	p, f, ts, b := rig(t, true)
	ctx := context.Background()
	marker := blog.Marker(strings.Repeat("a", 64))
	live := ts.URL + "/blog/sample-week/"
	if ok, note, err := p.Check(ctx, b, live, marker); ok || err != nil || note != "the page returned 404." {
		t.Fatalf("missing: %v %q %v", ok, note, err)
	}
	f.Deploy("/blog/sample-week/", hfake.Page("Example", blog.Marker(strings.Repeat("b", 64)), "<p>Example.</p>"))
	if ok, note, _ := p.Check(ctx, b, live, marker); ok || note != "the page does not show this version yet." {
		t.Fatalf("other version: %v %q", ok, note)
	}
	f.Deploy("/blog/sample-week/", hfake.Page("Example", marker, "<p>Example.</p>"))
	if ok, _, err := p.Check(ctx, b, live, marker); !ok || err != nil {
		t.Fatalf("live: %v %v", ok, err)
	}
	// Only the exact URL with its trailing slash counts; redirects are not followed.
	if _, _, err := p.Check(ctx, b, strings.TrimSuffix(live, "/"), marker); err == nil {
		t.Fatal("a live URL without the trailing slash was checked")
	}
	f.Deploy("/blog/moved/", hfake.Page("Example", marker, ""))
	f.Redirect("/blog/moved/")
	if ok, note, _ := p.Check(ctx, b, ts.URL+"/blog/moved/", marker); ok || note != "the live URL redirects." {
		t.Fatalf("redirect: %v %q", ok, note)
	}
	for _, g := range f.Gets() {
		if strings.HasSuffix(g, "auth=true") || g == "GET /blog/moved auth=false" {
			t.Fatalf("page check sent a token or followed a redirect: %v", f.Gets())
		}
	}
}

func TestNoClientNoCall(t *testing.T) {
	_, f, _, b := rig(t, true)
	p := &hookpub.Publisher{Secret: func(context.Context, int64, string) (string, error) { return "https://hooks.example.test/x", nil }}
	if err := p.Wake(context.Background(), b, blog.Wake{Event: blog.EventPublish, ExpiresAt: t0}); err != hookpub.ErrNoClient {
		t.Fatalf("no client: %v", err)
	}
	if len(f.Wakes()) != 0 {
		t.Fatal("a wake went out")
	}
}

// Check refuses, with no call, a live address that does not parse, has a
// user, a query or a fragment (even an empty one), is not https, does not
// end in a slash, or is on another host or port than the blog's site.
func TestCheckSameHost(t *testing.T) {
	p, f, ts, b := rig(t, true)
	ctx := context.Background()
	marker := blog.Marker(strings.Repeat("a", 64))
	host := strings.TrimPrefix(ts.URL, "https://")
	hostOnly, port, _ := strings.Cut(host, ":")
	notPage := "The live address is not an https address ending in a slash."
	other := "The live address is not on the blog's site, so it was not checked. Nothing is marked live."
	for _, c := range []struct{ live, want string }{
		{"https://%zz/blog/sample-week/", notPage},
		{"https://user@" + host + "/blog/sample-week/", notPage},
		{"https://" + host + "/blog/sample-week/?x=1", notPage},
		{"https://" + host + "/blog/sample-week/?", notPage},
		{"https://" + host + "/blog/sample-week/#top", notPage},
		{"https://" + host + "/blog/sample-week/#", notPage},
		{"https://" + host + "/blog/sample-week", notPage},
		{"https://" + host + "/blog/sample-week/index.html", notPage},
		{"http://" + host + "/blog/sample-week/", notPage},
		{"HTTP://" + host + "/blog/sample-week/", notPage},
		{"//" + host + "/blog/sample-week/", notPage},
		{"https://" + hostOnly + ":1/blog/sample-week/", other},
		{"https://" + hostOnly + "/blog/sample-week/", other},
		{"https://other.example.test:" + port + "/blog/sample-week/", other},
	} {
		ok, _, err := p.Check(ctx, b, c.live, marker)
		if ok || err == nil || err.Error() != c.want {
			t.Errorf("%q: %v %v", c.live, ok, err)
		}
		if err != nil && (strings.Contains(err.Error(), hostOnly) || strings.Contains(err.Error(), "example.test")) {
			t.Errorf("%q: error shows the address", c.live)
		}
	}
	if g := f.Gets(); len(g) != 0 {
		t.Fatalf("calls for refused addresses: %v", g)
	}
	// The blog's own host still checks.
	f.Deploy("/blog/sample-week/", hfake.Page("Example", marker, ""))
	if ok, _, err := p.Check(ctx, b, ts.URL+"/blog/sample-week/", marker); !ok || err != nil {
		t.Fatalf("same host: %v %v", ok, err)
	}
}

// List reads the post index and each page with GETs only, sends no
// secret, refuses a site URL that is not an https root without a call,
// and never reads a page for a slug it can't use.
func TestListImportRead(t *testing.T) {
	p, f, ts, b := rig(t, true)
	f.Serve("/blog/posts.json", "application/json; charset=utf-8", `[{"slug":"sample-one","title":"Example one","date":"2026-05-01","description":"Invented."},
		{"slug":"sample-two","title":"Example two","date":"2026-05-02","description":"Invented."},{"slug":"x/../y","title":"Example","date":"2026-05-02"}]`)
	f.Deploy("/blog/sample-one/", hfake.Page("Example one", "", "<p>Invented.</p>"))
	ups, err := p.List(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 3 || !ups[0].Live || ups[0].URL != ts.URL+"/blog/sample-one/" || ups[1].Live || ups[1].Why != "its page returned 404." || ups[2].Live {
		t.Fatalf("%+v", ups)
	}
	want := []string{"GET /blog/posts.json auth=false", "GET /blog/sample-one/ auth=false", "GET /blog/sample-two/ auth=false"}
	if g := f.Gets(); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Fatalf("gets %v", g)
	}
	if len(f.Wakes()) != 0 {
		t.Fatal("List sent a wake")
	}
	for _, site := range []string{"http://notes.example.test", "https://notes.example.test/blog", "https://user@notes.example.test", ""} {
		bb := *b
		bb.SiteURL = site
		if _, err := p.List(context.Background(), &bb); err == nil {
			t.Errorf("site %q accepted", site)
		}
	}
	if n := len(f.Gets()); n != 3 {
		t.Fatalf("a refused site made %d calls", n-3)
	}
	f.Serve("/blog/posts.json", "application/json", `[]`)
	f.SetStatus(0)
	f.Redirect("/blog/posts.json")
	if _, err := p.List(context.Background(), b); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("redirect: %v", err)
	}
}

// ImportRead lets through only the plain index and page GETs: a bare "?",
// any query, a fragment (also a raw one) or an escaped path is refused.
func TestImportReadRefusesQueryAndFragment(t *testing.T) {
	const site = "https://notes.example.test"
	get := func(s string) *http.Request {
		t.Helper()
		r, err := http.NewRequest(http.MethodGet, s, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, p := range []string{"/blog/posts.json", "/blog/sample-one/"} {
		if !hookpub.ImportRead(get(site + p)) {
			t.Errorf("%s refused", p)
		}
		for _, s := range []string{p + "?", p + "?x=1", p + "?x", p + "#top", p + "?#", p + "?x=1#top"} {
			if hookpub.ImportRead(get(site + s)) {
				t.Errorf("%q allowed", s)
			}
		}
		r := get(site + p)
		r.URL.ForceQuery = true
		if hookpub.ImportRead(r) {
			t.Errorf("%s with ForceQuery allowed", p)
		}
		r = get(site + p)
		r.URL.RawFragment = "top"
		if hookpub.ImportRead(r) {
			t.Errorf("%s with RawFragment allowed", p)
		}
	}
	for _, s := range []string{"/blog/%73ample-one/", "/blog/posts%2Ejson", "/blog/sample-one"} {
		if hookpub.ImportRead(get(site + s)) {
			t.Errorf("%q allowed", s)
		}
	}
	// A bare "#" does not survive parsing (Go drops it, and it is never
	// sent), so List refuses it in the site URL before any request exists.
	if r := get(site + "/blog/posts.json#"); r.URL.String() != site+"/blog/posts.json" {
		t.Fatalf("bare # kept: %q", r.URL.String())
	}
}

// List refuses a site URL with a bare "?" or "#" with no call.
func TestListRefusesBareQueryOrFragmentSite(t *testing.T) {
	p, f, ts, b := rig(t, true)
	f.Serve("/blog/posts.json", "application/json", `[]`)
	for _, site := range []string{ts.URL + "/?", ts.URL + "/#", ts.URL + "?", ts.URL + "#", ts.URL + "/#top", ts.URL + "/?x=1"} {
		bb := *b
		bb.SiteURL = site
		if _, err := p.List(context.Background(), &bb); err == nil {
			t.Errorf("site %q accepted", site)
		}
	}
	if g := f.Gets(); len(g) != 0 {
		t.Fatalf("calls for refused sites: %v", g)
	}
	if _, err := p.List(context.Background(), b); err != nil {
		t.Fatalf("plain site: %v", err)
	}
}

// Check with no blog or on a nil Publisher returns an error, not a panic,
// and makes no call.
func TestCheckNilBlogOrPublisher(t *testing.T) {
	p, f, ts, b := rig(t, true)
	live := ts.URL + "/blog/sample-week/"
	marker := blog.Marker(strings.Repeat("a", 64))
	call := func(name string, fn func() (bool, string, error)) {
		t.Helper()
		defer func() {
			if x := recover(); x != nil {
				t.Fatalf("%s panicked: %v", name, x)
			}
		}()
		ok, _, err := fn()
		if ok || err == nil || err.Error() != "The live check has no blog or publisher set up, so nothing was checked. Nothing is marked live." {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
	}
	call("nil blog", func() (bool, string, error) { return p.Check(context.Background(), nil, live, marker) })
	var nilPub *hookpub.Publisher
	call("nil publisher", func() (bool, string, error) { return nilPub.Check(context.Background(), b, live, marker) })
	if g := f.Gets(); len(g) != 0 {
		t.Fatalf("calls %v", g)
	}
}

// Check refuses, with no call, a live host that only matches the site's
// host under Unicode case folding (KELVIN SIGN folds to k, LONG S to s):
// it names a different host.
func TestCheckRefusesUnicodeFoldedHost(t *testing.T) {
	p, _, _, b := rig(t, true)
	n := 0
	p.HTTP = func() *http.Client {
		return &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) { n++; return nil, errors.New("refused by the test") })}
	}
	marker := blog.Marker(strings.Repeat("a", 64))
	for _, c := range []struct{ site, live string }{
		{"https://kample.example.test", "https://\u212Aample.example.test/blog/sample-week/"},
		{"https://sample.example.test", "https://\u017Fample.example.test/blog/sample-week/"},
	} {
		bb := *b
		bb.SiteURL = c.site
		ok, _, err := p.Check(context.Background(), &bb, c.live, marker)
		if ok || err == nil || err.Error() != "The live address is not on the blog's site, so it was not checked. Nothing is marked live." {
			t.Errorf("%q: %v %v", c.live, ok, err)
		}
	}
	if n != 0 {
		t.Fatalf("%d requests for a different host", n)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
