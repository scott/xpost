package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

func TestMain(m *testing.M) {
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	os.Exit(m.Run())
}

// Invented test credentials; nothing here is real.
const (
	testEmail = "owner@example.com"
	testPW    = "example password 123"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	t     *testing.T
	st    *store.Store
	srv   *Server
	ts    *httptest.Server
	logs  *syncBuf
	now   time.Time
	mu    sync.Mutex
	owner *store.User
	setup string // setup link token from CreateOwner
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// testNow is a fixed instant: Mon 2026-06-01 15:00 UTC.
var testNow = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "xpost.db"))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := secret.NewKey()
	e := &env{t: t, logs: &syncBuf{}, now: testNow}
	e.st = store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := e.st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	e.st.Now = e.clock
	cfg := Config{BaseURL: "https://xpost.example.test", BlogsEnabled: true, FailLog: e.logs, Log: e.logs, Location: time.UTC, RPID: testRPID, RPOrigin: testOrigin}
	if mod != nil {
		mod(&cfg)
	}
	e.srv, err = New(e.st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.ts = httptest.NewServer(e.srv.Handler())
	t.Cleanup(func() { e.srv.Close(); e.ts.Close(); d.Close() }) // live streams end first
	e.owner, e.setup, err = e.st.CreateOwner(ctx, testEmail, "Sample Owner")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type client struct {
	e *env
	c *http.Client
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

type resp struct {
	status int
	body   string
	loc    string
	h      http.Header
}

func (e *env) do(c *http.Client, req *http.Request) resp {
	e.t.Helper()
	r, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, string(b), r.Header.Get("Location"), r.Header}
}

func (c *client) get(path string) resp {
	c.e.t.Helper()
	req, _ := http.NewRequest("GET", c.e.ts.URL+path, nil)
	return c.e.do(c.c, req)
}

var csrfRE = regexp.MustCompile(`(?:name="csrf" value|data-csrf)="([^"]+)"`)

// csrf loads a page with a form and returns its CSRF token.
func (c *client) csrf(path string) string {
	c.e.t.Helper()
	r := c.get(path)
	m := csrfRE.FindStringSubmatch(r.body)
	if m == nil {
		c.e.t.Fatalf("no csrf token on %s (status %d)", path, r.status)
	}
	return m[1]
}

func (c *client) postRaw(path string, form url.Values, hdr map[string]string) resp {
	c.e.t.Helper()
	req, _ := http.NewRequest("POST", c.e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.e.do(c.c, req)
}

// post submits form to path with a CSRF token taken from formPage.
func (c *client) post(formPage, path string, form url.Values) resp {
	c.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf", c.csrf(formPage))
	return c.postRaw(path, form, nil)
}

// setupOwner runs the real setup flow: setup link, password, then a passkey
// registered by a software authenticator. It returns a full-session client.
func (e *env) setupOwner() (*client, *softKey) {
	e.t.Helper()
	c := e.client()
	r := c.post("/setup/"+e.setup, "/setup/"+e.setup, url.Values{"password": {testPW}, "confirm": {testPW}})
	if r.status != http.StatusSeeOther || r.loc != "/setup/passkey" {
		e.t.Fatalf("setup: %d %s %s", r.status, r.loc, r.body)
	}
	k := newSoftKey(e.t)
	csrf := c.csrf("/setup/passkey")
	r = c.postRaw("/passkeys/begin", url.Values{"csrf": {csrf}}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("register begin: %d %s", r.status, r.body)
	}
	r = c.postRaw("/passkeys/finish", url.Values{"csrf": {csrf}, "name": {"Test key"}, "credential": {k.create(r.body)}}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("register finish: %d %s", r.status, r.body)
	}
	if g := c.get("/queue"); g.status != http.StatusOK {
		e.t.Fatalf("queue after setup: %d", g.status)
	}
	return c, k
}

// passwordStep signs in with the password only; the session is not full.
func (e *env) passwordStep() *client {
	e.t.Helper()
	c := e.client()
	r := c.post("/login", "/login", url.Values{"email": {testEmail}, "password": {testPW}})
	if r.status != http.StatusSeeOther || r.loc != "/login/passkey" {
		e.t.Fatalf("password step: %d %s", r.status, r.loc)
	}
	return c
}

// login signs in with password then passkey.
func (e *env) login(k *softKey) *client {
	e.t.Helper()
	c := e.passwordStep()
	csrf := c.csrf("/login/passkey")
	r := c.postRaw("/login/passkey/begin", url.Values{"csrf": {csrf}}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("login begin: %d %s", r.status, r.body)
	}
	r = c.postRaw("/login/passkey/finish", url.Values{"csrf": {csrf}, "credential": {k.get(r.body)}}, nil)
	if r.status != http.StatusOK {
		e.t.Fatalf("login finish: %d %s", r.status, r.body)
	}
	return c
}

// token makes an agent token directly in the store and returns its secret.
func (e *env) token(name string, scopes ...string) (*store.AgentToken, string) {
	e.t.Helper()
	if len(scopes) == 0 {
		scopes = []string{store.ScopeDraftsWrite, store.ScopeLogRead}
	}
	tok, sec, err := e.st.CreateAgentToken(context.Background(), e.owner, name, scopes)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok, sec
}

// api calls the agent API with a bearer token; body is JSON-encoded unless
// it is a string.
func (e *env) api(method, path, bearer string, body any) resp {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return e.do(http.DefaultClient, req)
}

type draftOut struct {
	ID           int64   `json:"id"`
	Status       string  `json:"status"`
	ScheduledAt  *string `json:"scheduled_at"`
	RejectReason *string `json:"reject_reason"`
	Parts        []struct {
		Text string `json:"text"`
	} `json:"parts"`
	Error string `json:"error"`
}

func decode(t *testing.T, r resp) draftOut {
	t.Helper()
	var d draftOut
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatalf("decode %d %q: %v", r.status, r.body, err)
	}
	return d
}

func draftBody(account string, at time.Time, texts ...string) map[string]any {
	var parts []map[string]any
	for _, s := range texts {
		parts = append(parts, map[string]any{"text": s})
	}
	return map[string]any{"account": account, "proposed_at": at.UTC().Format(time.RFC3339), "parts": parts}
}

// propose creates a pending draft through the API and returns its id.
func (e *env) propose(bearer, account string, at time.Time, texts ...string) int64 {
	e.t.Helper()
	r := e.api("POST", "/api/v1/drafts", bearer, draftBody(account, at, texts...))
	if r.status != http.StatusCreated {
		e.t.Fatalf("propose: %d %s", r.status, r.body)
	}
	return decode(e.t, r).ID
}

func (e *env) draft(id int64) *store.Draft {
	e.t.Helper()
	d, err := e.st.Draft(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

// approveNow approves through the store as the owner.
func (e *env) approveNow(id int64) {
	e.t.Helper()
	d := e.draft(id)
	if err := e.st.Approve(context.Background(), e.owner, id, d.Version); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ownerForm(id int64, extra url.Values) url.Values {
	f := url.Values{"v": {strconv.Itoa(e.draft(id).Version)}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func path(id int64, rest string) string { return "/drafts/" + strconv.FormatInt(id, 10) + rest }

func strconvI(n int64) string { return strconv.FormatInt(n, 10) }
