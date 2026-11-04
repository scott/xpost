package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
	"github.com/scott/xpost/internal/xauth/xfake"
)

// Invented OAuth client values for the fake provider.
const (
	xClientID     = "example-client-id"
	xClientSecret = "example-client-secret-value"
)

type xenv struct {
	*env
	fake *xfake.Server
	fts  *httptest.Server
}

func newXEnv(t *testing.T) *xenv {
	t.Helper()
	fake := &xfake.Server{ClientID: xClientID, ClientSecret: xClientSecret, Users: map[string]string{"sample_writer": "1001", "sample_maker": "1002"}}
	fts := httptest.NewServer(fake.Handler())
	t.Cleanup(fts.Close)
	e := newEnv(t, func(c *Config) {
		c.X = &xauth.Config{ClientID: xClientID, ClientSecret: xClientSecret, AuthorizeURL: fts.URL + "/i/oauth2/authorize",
			TokenURL: fts.URL + "/2/oauth2/token", RevokeURL: fts.URL + "/2/oauth2/revoke", MeURL: fts.URL + "/2/users/me",
			PostURL: fts.URL + "/2/tweets", MediaURL: fts.URL + "/2/media/upload", MediaMetaURL: fts.URL + "/2/media/metadata", WebURL: "https://x.invalid"}
	})
	fake.Now = e.clock
	return &xenv{env: e, fake: fake, fts: fts}
}

// startConnect presses Connect and returns the authorize URL.
func (x *xenv) startConnect(c *client, account string) *url.URL {
	x.t.Helper()
	r := c.post("/settings/accounts", "/settings/accounts/"+account+"/connect", nil)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.loc, x.fts.URL+"/i/oauth2/authorize?") {
		x.t.Fatalf("connect: %d %q", r.status, r.loc)
	}
	u, _ := url.Parse(r.loc)
	return u
}

// approveAt sends the browser to the fake's authorize URL as username and
// returns the callback path+query X would redirect to.
func (x *xenv) approveAt(auth *url.URL, username string) string {
	x.t.Helper()
	x.fake.AutoApprove = username
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(auth.String())
	if err != nil {
		x.t.Fatal(err)
	}
	resp.Body.Close()
	cb, _ := url.Parse(resp.Header.Get("Location"))
	if cb == nil || cb.Path != xauth.CallbackPath {
		x.t.Fatalf("authorize redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return cb.RequestURI()
}

func (x *xenv) connect(c *client, account string) resp {
	x.t.Helper()
	return c.get(x.approveAt(x.startConnect(c, account), account))
}

func TestXConnectFlow(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	auth := x.startConnect(c, "sample_writer")
	q := auth.Query()
	if q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 || q.Get("scope") != "tweet.read users.read tweet.write media.write offline.access" ||
		q.Get("redirect_uri") != "https://xpost.example.test/auth/x/callback" || q.Get("client_id") != xClientID || len(q.Get("state")) < 40 {
		t.Fatalf("authorize query: %v", q)
	}
	if strings.Contains(auth.RawQuery, xClientSecret) {
		t.Fatal("client secret in authorize URL")
	}
	r := c.get(x.approveAt(auth, "sample_writer"))
	if r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=connected" {
		t.Fatalf("callback: %d %q %s", r.status, r.loc, r.body)
	}
	// The fake checked the PKCE verifier against the challenge at the token
	// endpoint; a connection exists only if they matched.
	conn, err := x.st.XConnection(context.Background(), "sample_writer")
	if err != nil || conn.XUsername != "sample_writer" || conn.XUserID != "1001" || conn.Status != store.XConnected {
		t.Fatalf("stored: %+v %v", conn, err)
	}
	page := c.get("/settings/accounts").body
	if !strings.Contains(page, "@sample_writer <span class=\"muted\">(id 1001)</span>") || !strings.Contains(page, "Disconnect") {
		t.Fatal("accounts page does not show the connection")
	}
	tok, _, _ := x.st.XTokensFor(context.Background(), "sample_writer")
	if strings.Contains(page, tok.Access) || strings.Contains(page, tok.Refresh) {
		t.Fatal("token rendered")
	}
	if csp := c.get("/settings/accounts").h.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' "+x.fts.URL) {
		t.Fatalf("accounts CSP %q", csp)
	}
	// The queue allows only the X web origin (Open on X on reply cards),
	// never the OAuth authorize origin.
	if csp := c.get("/queue").h.Get("Content-Security-Policy"); strings.Contains(csp, x.fts.URL) || csp != strings.Replace(CSP, "form-action 'self'", "form-action 'self' https://x.invalid", 1) {
		t.Fatalf("queue CSP widened: %q", csp)
	}
}

func TestXCallbackRejectsBadState(t *testing.T) {
	x := newXEnv(t)
	c, k := x.setupOwner()
	none := func(why string) {
		t.Helper()
		if _, err := x.st.XConnection(context.Background(), "sample_writer"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: a connection was saved", why)
		}
	}
	// Tampered state.
	cb := x.approveAt(x.startConnect(c, "sample_writer"), "sample_writer")
	u, _ := url.Parse(cb)
	q := u.Query()
	good := q.Get("state")
	q.Set("state", good[:len(good)-2]+"xx")
	if r := c.get(u.Path + "?" + q.Encode()); r.status != http.StatusBadRequest {
		t.Fatalf("tampered state: %d", r.status)
	}
	none("tampered")
	// The real state is still single use: the tampered try did not burn
	// it, but using it from another session does, and then it's gone.
	other := x.login(k)
	if r := other.get(cb); r.status != http.StatusBadRequest {
		t.Fatalf("other session: %d", r.status)
	}
	if r := c.get(cb); r.status != http.StatusBadRequest {
		t.Fatalf("state after another session took it: %d", r.status)
	}
	none("other session")
	// Replay after success.
	cb = x.approveAt(x.startConnect(c, "sample_writer"), "sample_writer")
	if r := c.get(cb); r.status != http.StatusSeeOther {
		t.Fatalf("good callback: %d %s", r.status, r.body)
	}
	if r := c.get(cb); r.status != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", r.status)
	}
	// Expired state.
	cb = x.approveAt(x.startConnect(c, "sample_maker"), "sample_maker")
	x.advance(store.OAuthStateTTL + time.Second)
	c2 := x.login(k) // the old session is still fine; a new one just avoids idle limits
	_ = c2
	if r := c.get(cb); r.status != http.StatusBadRequest {
		t.Fatalf("expired state: %d", r.status)
	}
	// Missing state, denied at X.
	if r := c.get(xauth.CallbackPath + "?code=fake-code-x"); r.status != http.StatusBadRequest {
		t.Fatalf("no state: %d", r.status)
	}
	auth := x.startConnect(c, "sample_maker")
	if r := c.get(x.approveAt(auth, xfake.Deny)); r.status != http.StatusBadRequest || !strings.Contains(r.body, "cancelled") {
		t.Fatalf("access_denied: %d", r.status)
	}
	if _, err := x.st.XConnection(context.Background(), "sample_maker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("denied connect saved something")
	}
}

func TestXCallbackRefusesOtherXUser(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	r := c.get(x.approveAt(x.startConnect(c, "sample_writer"), "sample_maker"))
	if r.status != http.StatusConflict || !strings.Contains(r.body, "You authorized @sample_maker but this is @sample_writer") {
		t.Fatalf("other user: %d", r.status)
	}
	if _, err := x.st.XConnection(context.Background(), "sample_writer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("stored another user's tokens")
	}
	if x.fake.RevokeCalls != 2 {
		t.Fatalf("unused tokens not revoked: %d", x.fake.RevokeCalls)
	}
}

func TestXConnectDisabledWithoutClientID(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	page := c.get("/settings/accounts")
	if !strings.Contains(page.body, "Connecting is off.") || !strings.Contains(page.body, "XPOST_X_CLIENT_ID") ||
		!strings.Contains(page.body, "https://xpost.example.test/auth/x/callback") || !strings.Contains(page.body, `disabled aria-disabled="true"`) {
		t.Fatal("not-configured page lacks its message")
	}
	if strings.Contains(page.body, `action="/settings/accounts/sample_writer/connect"`) {
		t.Fatal("connect form shown without a client id")
	}
	if r := c.post("/settings/accounts", "/settings/accounts/sample_writer/connect", nil); r.status != http.StatusServiceUnavailable {
		t.Fatalf("connect without client id: %d %s", r.status, r.loc)
	}
	if page.h.Get("Content-Security-Policy") != CSP {
		t.Fatal("CSP widened with Connect off")
	}
}

func TestXDisconnectRevokesAndDeletes(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	x.connect(c, "sample_writer")
	tok, _, _ := x.st.XTokensFor(context.Background(), "sample_writer")
	if r := c.postRaw("/settings/accounts/sample_writer/disconnect", url.Values{}, nil); r.status != http.StatusForbidden {
		t.Fatalf("disconnect without CSRF: %d", r.status)
	}
	if r := c.post("/settings/accounts", "/settings/accounts/sample_writer/disconnect", nil); r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=disconnected" {
		t.Fatalf("disconnect: %d %q", r.status, r.loc)
	}
	if _, err := x.st.XConnection(context.Background(), "sample_writer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("tokens kept")
	}
	if x.fake.LiveAccess(tok.Access) || x.fake.RevokeCalls != 2 {
		t.Fatalf("revoke at provider: calls %d", x.fake.RevokeCalls)
	}
	var n int
	x.st.DB.QueryRow(`SELECT COUNT(*) FROM x_connections`).Scan(&n)
	if n != 0 {
		t.Fatal("connection row left")
	}
}

func TestXCheckRefreshesAndFlagsReconnect(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	x.connect(c, "sample_maker")
	x.fake.ExpireAccess("sample_maker")
	if r := c.post("/settings/accounts", "/settings/accounts/sample_maker/check", nil); r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=checked" {
		t.Fatalf("check after 401: %d %q", r.status, r.loc)
	}
	if x.fake.RefreshCalls != 1 {
		t.Fatalf("refreshes %d", x.fake.RefreshCalls)
	}
	x.fake.FailRefresh("sample_maker", 1)
	x.fake.ExpireAccess("sample_maker")
	if r := c.post("/settings/accounts", "/settings/accounts/sample_maker/check", nil); r.status != http.StatusConflict {
		t.Fatalf("check with invalid_grant: %d", r.status)
	}
	page := c.get("/settings/accounts").body
	if !strings.Contains(page, "Needs reconnect") || !strings.Contains(page, ">"+"Reconnect</button>") && !strings.Contains(page, "Reconnect</button>") {
		t.Fatal("reconnect state not shown")
	}
	// Reconnecting through the flow clears it.
	if r := x.connect(c, "sample_maker"); r.status != http.StatusSeeOther {
		t.Fatalf("reconnect: %d", r.status)
	}
	if conn, _ := x.st.XConnection(context.Background(), "sample_maker"); conn.Status != store.XConnected {
		t.Fatalf("after reconnect: %s", conn.Status)
	}
}

// Agent tokens cannot start, finish, check or remove a connection: the
// owner routes ignore bearer tokens and the API has no such route.
func TestAgentCannotConnectOrDisconnect(t *testing.T) {
	x := newXEnv(t)
	owner, _ := x.setupOwner()
	x.connect(owner, "sample_writer")
	before, _ := x.st.XConnection(context.Background(), "sample_writer")
	_, sec := x.token("sample-agent")
	authBefore, revokeBefore := x.fake.LastAuthorize, x.fake.RevokeCalls

	anon := x.client()
	csrf := anon.csrf("/login")
	hdr := map[string]string{"Authorization": "Bearer " + sec}
	for _, p := range []string{"/settings/accounts/sample_maker/connect", "/settings/accounts/sample_writer/disconnect", "/settings/accounts/sample_writer/check"} {
		if r := anon.postRaw(p, url.Values{"csrf": {csrf}}, hdr); r.status != http.StatusUnauthorized {
			t.Errorf("POST %s with bearer: %d, want 401", p, r.status)
		}
	}
	req, _ := http.NewRequest("GET", x.ts.URL+xauth.CallbackPath+"?state=x&code=y", nil)
	req.Header.Set("Authorization", "Bearer "+sec)
	if r := x.do(anon.c, req); r.status != http.StatusSeeOther || !strings.HasPrefix(r.loc, "/login") {
		t.Errorf("callback with bearer: %d %q", r.status, r.loc)
	}
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		for _, p := range []string{"/api/v1/accounts", "/api/v1/accounts/sample_writer", "/api/v1/accounts/sample_writer/connect",
			"/api/v1/accounts/sample_writer/disconnect", "/api/v1/x/connect", "/api/v1/oauth/callback", "/api/v1/auth/x/callback", "/api/v1/accounts/x/callback"} {
			if r := x.api(m, p, sec, `{}`); r.status != http.StatusNotFound && r.status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d", m, p, r.status)
			}
		}
	}
	after, err := x.st.XConnection(context.Background(), "sample_writer")
	if err != nil || after.Generation != before.Generation {
		t.Fatal("connection changed")
	}
	if _, err := x.st.XConnection(context.Background(), "sample_maker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("agent connected an account")
	}
	if x.fake.RevokeCalls != revokeBefore || x.fake.LastAuthorize.Get("state") != authBefore.Get("state") {
		t.Fatal("agent traffic reached the provider")
	}
	var states int
	x.st.DB.QueryRow(`SELECT COUNT(*) FROM oauth_states`).Scan(&states)
	if states != 0 {
		t.Fatalf("%d pending states from agent traffic", states)
	}
}

// Nothing secret reaches the logs or any page, on success or failure.
func TestXSecretsNeverLoggedOrRendered(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	var bodies []string
	keep := func(r resp) resp { bodies = append(bodies, r.body, r.loc); return r }
	var secrets []string

	// Success.
	auth := x.startConnect(c, "sample_writer")
	cb := x.approveAt(auth, "sample_writer")
	cbq, _ := url.Parse(cb)
	secrets = append(secrets, cbq.Query().Get("code"), cbq.Query().Get("state"))
	keep(c.get(cb))
	keep(c.get("/settings/accounts"))
	tok, _, _ := x.st.XTokensFor(context.Background(), "sample_writer")
	secrets = append(secrets, tok.Access, tok.Refresh)
	// Replayed code and state (exchange refused at X side and state check).
	keep(c.get(cb))
	// A fresh state with a code X won't accept.
	auth = x.startConnect(c, "sample_maker")
	cb2 := x.approveAt(auth, "sample_maker")
	u2, _ := url.Parse(cb2)
	q2 := u2.Query()
	secrets = append(secrets, q2.Get("state"), q2.Get("code"))
	q2.Set("code", "fake-code-made-up-example")
	secrets = append(secrets, "fake-code-made-up-example")
	keep(c.get(u2.Path + "?" + q2.Encode()))
	// Refresh, a refused refresh, and a disconnect.
	x.fake.ExpireAccess("sample_writer")
	keep(c.post("/settings/accounts", "/settings/accounts/sample_writer/check", nil))
	tok2, _, _ := x.st.XTokensFor(context.Background(), "sample_writer")
	secrets = append(secrets, tok2.Access, tok2.Refresh)
	x.fake.FailRefresh("sample_writer", 1)
	x.fake.ExpireAccess("sample_writer")
	keep(c.post("/settings/accounts", "/settings/accounts/sample_writer/check", nil))
	keep(c.get("/settings/accounts"))
	keep(c.post("/settings/accounts", "/settings/accounts/sample_writer/disconnect", nil))
	secrets = append(secrets, xClientSecret)

	logs := x.logs.String()
	if !strings.Contains(logs, "connected") {
		t.Fatalf("expected some log lines, got %q", logs)
	}
	for _, s := range secrets {
		if s == "" {
			t.Fatal("empty secret captured")
		}
		if strings.Contains(logs, s) {
			t.Errorf("logs contain a secret (%d chars)", len(s))
		}
		for _, b := range bodies {
			if strings.Contains(b, s) {
				t.Errorf("a page contains a secret (%d chars)", len(s))
			}
		}
	}
}

// The callback registered with X is <base URL>/auth/x/callback; nothing else answers.
func TestXCallbackPath(t *testing.T) {
	x := newXEnv(t)
	c, _ := x.setupOwner()
	if xauth.CallbackPath != "/auth/x/callback" || x.srv.cfg.X.RedirectURL != "https://xpost.example.test/auth/x/callback" {
		t.Fatalf("callback %q %q", xauth.CallbackPath, x.srv.cfg.X.RedirectURL)
	}
	// The old /accounts tree only redirects; it never runs a callback and
	// never carries OAuth parameters along.
	if r := c.get("/accounts/x/callback?state=a&code=b"); r.status != http.StatusSeeOther || r.loc != "/settings/accounts" {
		t.Fatalf("old callback path answered %d %q", r.status, r.loc)
	}
	if r := c.get("/settings/accounts/x/callback?state=a&code=b"); r.status != http.StatusNotFound {
		t.Fatalf("callback under Settings answered %d", r.status)
	}
}
