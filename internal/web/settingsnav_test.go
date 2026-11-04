package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth/xfake"
)

var (
	stabsRE  = regexp.MustCompile(`(?s)<nav class="stabs" aria-label="Settings">(.*?)</nav>`)
	stabOnRE = regexp.MustCompile(`<a href="([^"]+)" class="on" aria-current="page">([^<]+)</a>`)
	mainRE   = regexp.MustCompile(`(?s)<nav class="nav" aria-label="Main"[^>]*>(.*?)</nav>`)
	tabsRE   = regexp.MustCompile(`(?s)<nav class="tabs" aria-label="Main"[^>]*>(.*?)</nav>`)
)

// mustHaveNav checks the app's top nav (desktop menu and phone tab bar) and
// the Settings tab bar on a Settings page. want is the label of the current
// Settings tab, or "" when none is current (the webhooks section is a
// section of General, reached by its anchor).
func mustHaveNav(t *testing.T, name, body, want string) {
	t.Helper()
	m := mainRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s: no main menu", name)
	}
	if !strings.Contains(m[1], `<a href="/settings" class="on" aria-current="page">`) {
		t.Errorf("%s: Settings is not current in the menu", name)
	}
	for _, href := range []string{`href="/queue"`, `href="/schedule"`, `href="/log"`, `href="/security"`} {
		if !strings.Contains(m[1], href) {
			t.Errorf("%s: menu lacks %s", name, href)
		}
	}
	tb := tabsRE.FindStringSubmatch(body)
	if tb == nil {
		t.Fatalf("%s: no phone tab bar", name)
	}
	if !strings.Contains(tb[1], `class="tab on" href="#more"`) {
		t.Errorf("%s: More is not current in the phone tab bar", name)
	}
	for _, nav := range []string{m[1], tb[1]} {
		if strings.Contains(nav, "accounts") || strings.Contains(nav, "Accounts<") || strings.Contains(nav, "X accounts") {
			t.Errorf("%s: X accounts is still in the main nav", name)
		}
	}
	if strings.Contains(body, `href="/accounts"`) || strings.Contains(body, `action="/accounts/`) {
		t.Errorf("%s: links the old /accounts URL", name)
	}
	all := stabsRE.FindAllStringSubmatch(body, -1)
	if len(all) != 1 {
		t.Fatalf("%s: %d Settings tab bars, want 1", name, len(all))
	}
	s := all[0][1]
	for _, tab := range []string{`<a href="/settings"`, `<a href="/settings/accounts"`, `<a href="/settings/blogs"`, `<a href="/settings#hooksh"`} {
		if !strings.Contains(s, tab) {
			t.Errorf("%s: Settings tabs lack %s", name, tab)
		}
	}
	on := stabOnRE.FindAllStringSubmatch(s, -1)
	switch {
	case want == "" && len(on) != 0:
		t.Errorf("%s: current tab %v, want none", name, on)
	case want != "" && (len(on) != 1 || on[0][2] != want):
		t.Errorf("%s: current tab %v, want %q", name, on, want)
	}
}

// Every Settings tab and sub-page, including the error re-renders, shows the
// app nav and the Settings tab bar.
func TestSettingsNavOnEveryTab(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	ctx := context.Background()
	tok, _ := x.token("sample-agent", store.ScopeDraftsWrite, store.ScopeDraftsRevise)
	b, err := x.st.CreateBlog(ctx, x.owner, store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook, PublishClock: "07:00"})
	if err != nil {
		t.Fatal(err)
	}
	id := strconvI(b.ID)
	for _, tc := range []struct{ name, path, want string }{
		{"general", "/settings", "General"},
		{"general flash", "/settings?done=saved", "General"},
		{"webhooks section", "/settings#hooksh", "General"},
		{"x accounts", "/settings/accounts", "X accounts"},
		{"x accounts flash", "/settings/accounts?done=connected", "X accounts"},
		{"blogs", "/settings/blogs", "Blogs"},
		{"blog open", "/settings/blogs?open=" + id, "Blogs"},
		{"add blog step 1", "/settings/blogs/new", "Blogs"},
		{"add blog api", "/settings/blogs/new?method=api", "Blogs"},
		{"add blog webhook", "/settings/blogs/new?method=webhook", "Blogs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := c.get(tc.path)
			if r.status != http.StatusOK {
				t.Fatalf("%s: %d", tc.path, r.status)
			}
			mustHaveNav(t, tc.path, r.body, tc.want)
		})
	}
	// Error re-renders of each tab keep the nav too.
	posts := []struct {
		name, form, path, want string
		v                      url.Values
	}{
		{"bad time zone", "/settings", "/settings", "General", url.Values{"tz": {"Not/AZone"}, "cap": {"20.00"}}},
		{"bad webhook", "/settings", "/settings/webhooks/" + strconvI(tok.ID), "General", url.Values{"url": {"not a url"}}},
		{"bad new blog", "/settings/blogs/new?method=api", "/settings/blogs", "Blogs", url.Values{"method": {"api"}, "name": {""}}},
		{"bad blog edit", "/settings/blogs", "/settings/blogs/" + id, "Blogs", url.Values{"method": {"webhook"}, "name": {""}}},
		{"check not connected", "/settings/accounts", "/settings/accounts/sample_writer/check", "X accounts", nil},
	}
	for _, tc := range posts {
		t.Run(tc.name, func(t *testing.T) {
			r := c.post(tc.form, tc.path, tc.v)
			if r.status < 400 {
				t.Fatalf("%s: %d %q, want an error page", tc.path, r.status, r.loc)
			}
			mustHaveNav(t, tc.name, r.body, tc.want)
		})
	}
}

// The old X accounts URLs redirect (303) to Settings > X accounts with the
// query kept, never off site, and never run an action.
func TestOldAccountsRedirect(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	for _, tc := range []struct{ method, path, loc string }{
		{"GET", "/accounts", "/settings/accounts"},
		{"GET", "/accounts?done=connected", "/settings/accounts?done=connected"},
		{"GET", "/accounts/", "/settings/accounts"},
		{"GET", "/accounts/sample_writer", "/settings/accounts"},
		{"GET", "/accounts/a/b/c?x=1&y=two+words", "/settings/accounts?x=1&y=two+words"},
		{"HEAD", "/accounts", "/settings/accounts"},
		{"POST", "/accounts/sample_writer/connect", "/settings/accounts"},
		{"POST", "/accounts/sample_writer/disconnect?done=x", "/settings/accounts?done=x"},
		// Open-redirect attempts stay on /settings/accounts.
		{"GET", "/accounts//evil.example", "/settings/accounts"},
		{"GET", "/accounts/%2F%2Fevil.example", "/settings/accounts"},
		{"GET", "/accounts?next=https://evil.example&u=//evil.example", "/settings/accounts?next=https%3A%2F%2Fevil.example&u=%2F%2Fevil.example"},
		{"GET", "/accounts/x/callback?state=s&code=c&done=checked", "/settings/accounts?done=checked"},
	} {
		// Follow at most three same-site hops (the router cleans "//" paths
		// with a relative redirect first); every hop must stay on this site.
		method, p, form := tc.method, tc.path, url.Values{}
		if method == "POST" {
			form.Set("csrf", c.csrf("/settings/accounts"))
		}
		var r resp
		for hop := 0; hop < 3; hop++ {
			req, _ := http.NewRequest(method, x.ts.URL+p, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r = x.do(c.c, req)
			if u, err := url.Parse(r.loc); r.loc == "" || err != nil || u.Host != "" || u.Scheme != "" || !strings.HasPrefix(r.loc, "/") || strings.HasPrefix(r.loc, "//") {
				t.Errorf("%s %s: hop %d leaves the site or stops: %d %q", tc.method, tc.path, hop, r.status, r.loc)
				break
			}
			if r.status == http.StatusSeeOther || r.status == http.StatusFound {
				break
			}
			p = r.loc
		}
		if r.status != http.StatusSeeOther && r.status != http.StatusFound {
			t.Errorf("%s %s: %d", tc.method, tc.path, r.status)
			continue
		}
		if r.loc != tc.loc {
			t.Errorf("%s %s: Location %q, want %q", tc.method, tc.path, r.loc, tc.loc)
		}
	}
	// An old POST without a CSRF token is refused before any redirect.
	if r := c.postRaw("/accounts/sample_writer/connect", url.Values{}, nil); r.status != http.StatusForbidden {
		t.Errorf("old POST without CSRF: %d", r.status)
	}
	if _, err := x.st.XConnection(context.Background(), "sample_writer"); err == nil {
		t.Fatal("an old POST ran an action")
	}
	// A signed-out visitor is redirected the same way; the target then asks
	// them to sign in.
	if r := x.client().get("/accounts?done=connected"); r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=connected" {
		t.Fatalf("signed out: %d %q", r.status, r.loc)
	}
}

// Connect through the fake X: the callback (path unchanged) lands on
// Settings > X accounts with its flash, and errors render that page in place.
func TestXCallbackLandsOnSettings(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	r := x.connect(c, "sample_writer")
	if r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=connected" {
		t.Fatalf("callback: %d %q", r.status, r.loc)
	}
	page := c.get(r.loc)
	if page.status != http.StatusOK || !strings.Contains(page.body, accountsFlash["connected"]) {
		t.Fatalf("landing: %d, flash missing", page.status)
	}
	mustHaveNav(t, "landing", page.body, "X accounts")

	// Cancelled at X: the error shows on the same page, nothing connects.
	cb := x.approveAt(x.startConnect(c, "sample_maker"), xfake.Deny)
	r = c.get(cb)
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "You cancelled at X, so nothing was connected.") {
		t.Fatalf("denied: %d", r.status)
	}
	mustHaveNav(t, "denied", r.body, "X accounts")

	// Disconnect and Check land there too.
	if r := c.post("/settings/accounts", "/settings/accounts/sample_writer/check", nil); r.loc != "/settings/accounts?done=checked" {
		t.Fatalf("check: %d %q", r.status, r.loc)
	}
	if r := c.post("/settings/accounts", "/settings/accounts/sample_writer/disconnect", nil); r.loc != "/settings/accounts?done=disconnected" {
		t.Fatalf("disconnect: %d %q", r.status, r.loc)
	}
}

// Pages outside Settings have the menu without X accounts as well.
func TestMenuWithoutXAccounts(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	for _, p := range []string{"/queue", "/schedule", "/log", "/security", "/compose"} {
		body := c.get(p).body
		m, tb := mainRE.FindStringSubmatch(body), tabsRE.FindStringSubmatch(body)
		if m == nil || tb == nil {
			t.Fatalf("%s: nav missing", p)
		}
		if strings.Contains(m[1]+tb[1], "accounts") || strings.Contains(m[1]+tb[1], "X accounts") {
			t.Errorf("%s: X accounts still in the main nav", p)
		}
		if n := strings.Count(tb[1], `<a class="tab`); n != 4 {
			t.Errorf("%s: %d phone tabs, want 4", p, n)
		}
	}
}
