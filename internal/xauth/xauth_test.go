package xauth_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
	"github.com/scott/xpost/internal/xauth/xfake"
)

// Invented values; the fake accepts nothing else.
const (
	clientID = "example-client-id"
	secretV  = "example-client-secret-value"
)

var t0 = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

type fx struct {
	t    *testing.T
	st   *store.Store
	fake *xfake.Server
	c    *xauth.Client
	m    *xauth.Manager
	logs *bytes.Buffer
	now  time.Time
	mu   sync.Mutex
	path string
	ts   *httptest.Server
}

func (f *fx) clock() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *fx) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func setup(t *testing.T) *fx {
	t.Helper()
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	f := &fx{t: t, now: t0, logs: &bytes.Buffer{}, path: filepath.Join(t.TempDir(), "x.db")}
	d, err := db.Open(ctx, f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	k, _ := secret.NewKey()
	f.st = store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := f.st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	f.st.Now = f.clock
	f.fake = &xfake.Server{ClientID: clientID, ClientSecret: secretV, Users: map[string]string{"sample_writer": "1001", "sample_maker": "1002"}, Now: f.clock}
	ts := httptest.NewServer(f.fake.Handler())
	t.Cleanup(ts.Close)
	f.ts = ts
	cfg := &xauth.Config{ClientID: clientID, ClientSecret: secretV, AuthorizeURL: ts.URL + "/i/oauth2/authorize", TokenURL: ts.URL + "/2/oauth2/token",
		RevokeURL: ts.URL + "/2/oauth2/revoke", MeURL: ts.URL + "/2/users/me", PostURL: ts.URL + "/2/tweets", MediaURL: ts.URL + "/2/media/upload", MediaMetaURL: ts.URL + "/2/media/metadata", WebURL: "https://x.invalid",
		RedirectURL: "https://xpost.example.test" + xauth.CallbackPath}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	f.c = xauth.NewClient(cfg)
	f.c.Now = f.clock
	f.m = &xauth.Manager{Store: f.st, Client: f.c, Log: log.New(f.logs, "", 0)}
	return f
}

// connect runs authorize -> code -> exchange against the fake and stores
// the tokens, as the web callback does.
func (f *fx) connect(account string) *xauth.Token {
	f.t.Helper()
	ctx := context.Background()
	f.fake.AutoApprove = account
	state, _ := xauth.NewState()
	verifier, _ := xauth.NewVerifier()
	code := f.authorize(state, verifier)
	tok, err := f.c.Exchange(ctx, code, verifier, f.clock())
	if err != nil {
		f.t.Fatal(err)
	}
	me, err := f.c.Me(ctx, tok.Access)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.SaveXConnection(ctx, account, me.ID, me.Username, store.XTokens{Access: tok.Access, Refresh: tok.Refresh, Expiry: tok.Expiry, Scope: tok.Scope}); err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *fx) authorize(state, verifier string) string {
	f.t.Helper()
	rr := httptest.NewRecorder()
	u, _ := url.Parse(f.c.AuthURL(state, verifier))
	req := httptest.NewRequest("GET", u.RequestURI(), nil)
	f.fake.Handler().ServeHTTP(rr, req)
	loc, _ := url.Parse(rr.Header().Get("Location"))
	if loc == nil || loc.Query().Get("state") != state || loc.Query().Get("code") == "" {
		f.t.Fatalf("authorize: %d %q", rr.Code, rr.Header().Get("Location"))
	}
	return loc.Query().Get("code")
}

func TestPKCEChallengeMatchesRFC7636(t *testing.T) {
	if got := xauth.Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge %q", got)
	}
	v, _ := xauth.NewVerifier()
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier length %d", len(v))
	}
}

func TestAuthURL(t *testing.T) {
	f := setup(t)
	u, _ := url.Parse(f.c.AuthURL("example-state", "example-verifier-0123456789012345678901234567"))
	q := u.Query()
	want := map[string]string{"response_type": "code", "client_id": clientID, "redirect_uri": "https://xpost.example.test/auth/x/callback",
		"scope": "tweet.read users.read tweet.write media.write offline.access", "state": "example-state", "code_challenge_method": "S256",
		"code_challenge": xauth.Challenge("example-verifier-0123456789012345678901234567")}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(u.RawQuery, "+") {
		t.Error("scope spaces must be %20, not +")
	}
	if strings.Contains(u.RawQuery, "example-verifier") {
		t.Error("the verifier itself is in the authorize URL")
	}
}

func TestExchangeChecksVerifierAndCodeIsSingleUse(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.fake.AutoApprove = "sample_writer"
	v, _ := xauth.NewVerifier()
	code := f.authorize("s1", v)
	other, _ := xauth.NewVerifier()
	if _, err := f.c.Exchange(ctx, code, other, f.clock()); !xauth.IsInvalidGrant(err) {
		t.Fatalf("wrong verifier: %v", err)
	}
	code = f.authorize("s2", v)
	if _, err := f.c.Exchange(ctx, code, v, f.clock()); err != nil {
		t.Fatalf("right verifier: %v", err)
	}
	if _, err := f.c.Exchange(ctx, code, v, f.clock()); !xauth.IsInvalidGrant(err) {
		t.Fatalf("reused code: %v", err)
	}
	// Wrong client secret.
	bad := *f.c.Cfg
	bad.ClientSecret = "wrong-example-secret"
	code = f.authorize("s3", v)
	if _, err := xauth.NewClient(&bad).Exchange(ctx, code, v, f.clock()); err == nil || strings.Contains(err.Error(), "wrong-example-secret") {
		t.Fatalf("bad secret: %v", err)
	}
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := xauth.FromEnv(env(nil), "https://post.example.com")
	if err != nil || c.Enabled() {
		t.Fatalf("no id: %v enabled=%v", err, c.Enabled())
	}
	if c.RedirectURL != "https://post.example.com/auth/x/callback" || c.TokenURL != xauth.DefaultTokenURL {
		t.Fatalf("defaults: %+v", c)
	}
	sf := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(sf, []byte(secretV+"\n"), 0o600)
	c, err = xauth.FromEnv(env(map[string]string{xauth.EnvClientID: clientID, xauth.EnvClientSecretFile: sf}), "https://post.example.com/")
	if err != nil || !c.Enabled() || c.ClientSecret != secretV {
		t.Fatalf("secret file: %v", err)
	}
	if _, err := xauth.FromEnv(env(map[string]string{xauth.EnvClientSecret: secretV}), "https://post.example.com"); err == nil {
		t.Error("secret without id accepted")
	}
	if _, err := xauth.FromEnv(env(map[string]string{xauth.EnvClientID: clientID, xauth.EnvTokenURL: "http://token.example.com/t"}), "https://post.example.com"); err == nil {
		t.Error("plain http token URL accepted")
	}
	if _, err := xauth.FromEnv(env(map[string]string{xauth.EnvClientID: clientID, xauth.EnvClientSecret: "a", xauth.EnvClientSecretFile: sf}), "https://post.example.com"); err == nil {
		t.Error("both secret and secret file accepted")
	}
}

func TestTokensEncryptedAtRest(t *testing.T) {
	f := setup(t)
	tok := f.connect("sample_writer")
	f.st.DB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	for _, p := range []string{f.path, f.path + "-wal"} {
		b, _ := os.ReadFile(p)
		for _, s := range []string{tok.Access, tok.Refresh, tok.Access[8:20], tok.Refresh[8:20]} {
			if bytes.Contains(b, []byte(s)) {
				t.Fatalf("%s holds a plaintext token", filepath.Base(p))
			}
		}
	}
	got, _, err := f.st.XTokensFor(context.Background(), "sample_writer")
	if err != nil || got.Access != tok.Access || got.Refresh != tok.Refresh {
		t.Fatalf("round trip: %v", err)
	}
	// Ciphertext is bound to its account and kind: moving it breaks it.
	f.connect("sample_maker")
	f.st.DB.Exec(`UPDATE x_connections SET access_enc = (SELECT access_enc FROM x_connections WHERE account='sample_writer') WHERE account='sample_maker'`)
	if _, _, err := f.st.XTokensFor(context.Background(), "sample_maker"); err == nil {
		t.Fatal("a token sealed for another account decrypted")
	}
}

func TestNoRefreshWhileFresh(t *testing.T) {
	f := setup(t)
	tok := f.connect("sample_writer")
	f.advance(time.Hour) // 2h tokens: still well inside the skew
	got, err := f.m.Access(context.Background(), "sample_writer")
	if err != nil || got != tok.Access || f.fake.RefreshCalls != 0 {
		t.Fatalf("access %v, %d refreshes", err, f.fake.RefreshCalls)
	}
}

func TestRefreshBeforeExpiryAndRotation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tok := f.connect("sample_writer")
	f.advance(2*time.Hour - xauth.RefreshSkew + time.Second) // inside the skew
	got, err := f.m.Access(ctx, "sample_writer")
	if err != nil || got == tok.Access || f.fake.RefreshCalls != 1 {
		t.Fatalf("refresh: %v same=%v calls=%d", err, got == tok.Access, f.fake.RefreshCalls)
	}
	stored, conn, _ := f.st.XTokensFor(ctx, "sample_writer")
	if stored.Access != got || stored.Refresh == tok.Refresh || stored.Refresh == "" || conn.RefreshedAt == nil {
		t.Fatal("rotated refresh token not stored")
	}
	if !stored.Expiry.Equal(f.clock().Add(2 * time.Hour)) {
		t.Fatalf("expiry %v", stored.Expiry)
	}
	// The old refresh token is spent at the provider.
	if _, err := f.c.Refresh(ctx, tok.Refresh, f.clock()); !xauth.IsInvalidGrant(err) {
		t.Fatalf("old refresh token still works: %v", err)
	}
	// And the next refresh uses the new one.
	f.advance(2 * time.Hour)
	if _, err := f.m.Access(ctx, "sample_writer"); err != nil || f.fake.RefreshCalls != 3 {
		t.Fatalf("second refresh: %v calls=%d", err, f.fake.RefreshCalls)
	}
}

func TestRefreshOn401RetriesOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.connect("sample_writer")
	f.fake.ExpireAccess("sample_writer") // provider revoked it early
	calls := 0
	err := f.m.Do(ctx, "sample_writer", func(access string) error {
		calls++
		_, err := f.c.Me(ctx, access)
		return err
	})
	if err != nil || calls != 2 || f.fake.RefreshCalls != 1 {
		t.Fatalf("Do: %v calls=%d refreshes=%d", err, calls, f.fake.RefreshCalls)
	}
	// A call that keeps failing with 401 is retried exactly once.
	calls = 0
	err = f.m.Do(ctx, "sample_writer", func(string) error { calls++; return &xauth.Error{Op: "users/me", Status: 401} })
	if !xauth.IsUnauthorized(err) || calls != 2 {
		t.Fatalf("persistent 401: %v calls=%d", err, calls)
	}
	// Other errors are not retried.
	calls = 0
	err = f.m.Do(ctx, "sample_writer", func(string) error { calls++; return errors.New("example failure") })
	if err == nil || calls != 1 {
		t.Fatalf("non-401: calls=%d", calls)
	}
}

func TestInvalidGrantMarksReconnect(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.connect("sample_maker")
	f.fake.FailRefresh("sample_maker", 1)
	f.advance(3 * time.Hour)
	if _, err := f.m.Access(ctx, "sample_maker"); !errors.Is(err, xauth.ErrReconnect) {
		t.Fatalf("access: %v", err)
	}
	c, _ := f.st.XConnection(ctx, "sample_maker")
	if c.Status != store.XReconnect || !strings.Contains(c.LastError, "invalid_grant") {
		t.Fatalf("status %s %q", c.Status, c.LastError)
	}
	// Further calls do not hammer the provider.
	n := f.fake.RefreshCalls
	f.m.Access(ctx, "sample_maker")
	if f.fake.RefreshCalls != n {
		t.Fatal("refresh retried after reconnect was flagged")
	}
	// Reconnecting clears it.
	f.connect("sample_maker")
	if c, _ := f.st.XConnection(ctx, "sample_maker"); c.Status != store.XConnected {
		t.Fatalf("after reconnect: %s", c.Status)
	}
}

func TestConcurrentRefreshSpendsTokenOnce(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.connect("sample_writer")
	f.advance(3 * time.Hour)
	var wg sync.WaitGroup
	got := make([]string, 12)
	errs := make([]error, 12)
	for i := range got {
		wg.Add(1)
		go func(i int) { defer wg.Done(); got[i], errs[i] = f.m.Access(ctx, "sample_writer") }(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil || got[i] != got[0] {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
	}
	if f.fake.RefreshCalls != 1 {
		t.Fatalf("%d refreshes for one expiry", f.fake.RefreshCalls)
	}
}

type hookRT struct {
	once sync.Once
	hook func()
	next http.RoundTripper
}

func (h *hookRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/oauth2/token") {
		h.once.Do(h.hook)
	}
	return h.next.RoundTrip(r)
}

// Two managers (as two processes would) racing on one rotated refresh
// token: B has read the old row and is sending the old refresh token when
// A's refresh lands first. B gets invalid_grant, finds A's token, and does
// not flag the account for reconnect.
func TestCrossProcessRefreshRace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.connect("sample_writer")
	f.advance(3 * time.Hour)
	var aTok string
	var aErr error
	bClient := xauth.NewClient(f.c.Cfg)
	bClient.HTTP.Transport = &hookRT{hook: func() { aTok, aErr = f.m.Access(ctx, "sample_writer") }, next: http.DefaultTransport}
	b := &xauth.Manager{Store: f.st, Client: bClient}
	bTok, err := b.Access(ctx, "sample_writer")
	if aErr != nil || err != nil {
		t.Fatalf("a: %v b: %v", aErr, err)
	}
	if bTok != aTok || f.fake.RefreshCalls != 2 {
		t.Fatalf("b got a's token: %v, refreshes %d", bTok == aTok, f.fake.RefreshCalls)
	}
	if c, _ := f.st.XConnection(ctx, "sample_writer"); c.Status != store.XConnected {
		t.Fatal("race flagged reconnect")
	}
	// UpdateXTokens refuses a stale generation.
	_, conn, _ := f.st.XTokensFor(ctx, "sample_writer")
	ok, _ := f.st.UpdateXTokens(ctx, "sample_writer", conn.Generation-1, store.XTokens{Access: "fake-stale", Expiry: f.clock().Add(time.Hour)})
	if ok {
		t.Fatal("stale generation overwrote tokens")
	}
}

func TestDisconnectRevokesAndDeletes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tok := f.connect("sample_writer")
	revoked, err := f.m.Disconnect(ctx, "sample_writer")
	if err != nil || !revoked {
		t.Fatalf("disconnect: %v revoked=%v", err, revoked)
	}
	if f.fake.RevokeCalls != 2 || len(f.fake.Revoked) != 2 {
		t.Fatalf("revokes: %d %v", f.fake.RevokeCalls, f.fake.Revoked)
	}
	if _, err := f.st.XConnection(ctx, "sample_writer"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("tokens still stored")
	}
	if f.fake.LiveAccess(tok.Access) {
		t.Fatal("access token still live at provider")
	}
	if _, err := f.m.Access(ctx, "sample_writer"); !errors.Is(err, xauth.ErrNotConnected) {
		t.Fatalf("after disconnect: %v", err)
	}
	// Revoke failure still deletes locally.
	f.connect("sample_maker")
	bad := *f.c.Cfg
	bad.RevokeURL = bad.RevokeURL + "-missing"
	m2 := &xauth.Manager{Store: f.st, Client: xauth.NewClient(&bad)}
	revoked, err = m2.Disconnect(ctx, "sample_maker")
	if err != nil || revoked {
		t.Fatalf("revoke failure: %v revoked=%v", err, revoked)
	}
	if _, err := f.st.XConnection(ctx, "sample_maker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("tokens kept after a failed revoke")
	}
}

func TestErrorsAndLogsCarryNoSecrets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	tok := f.connect("sample_writer")
	f.fake.FailRefresh("sample_writer", 1)
	f.advance(3 * time.Hour)
	_, err := f.m.Access(ctx, "sample_writer")
	_, err2 := f.c.Refresh(ctx, tok.Refresh, f.clock())
	_, err3 := f.c.Exchange(ctx, "fake-code-example", "example-verifier", f.clock())
	all := f.logs.String() + "|" + err.Error() + "|" + err2.Error() + "|" + err3.Error()
	for _, s := range []string{tok.Access, tok.Refresh, secretV, "fake-code-example", "example-verifier", "fake x:"} {
		if strings.Contains(all, s) {
			t.Fatalf("secret or provider text in errors/logs: %q", all)
		}
	}
}
