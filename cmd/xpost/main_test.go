package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// testEnv gives serve an invented key and a fresh DB, and makes sure no
// real X client settings from the environment reach it.
func testEnv(t *testing.T) string {
	t.Helper()
	k, err := secret.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XPOST_KEY", k.Encode())
	t.Setenv("XPOST_KEY_FILE", "")
	for _, v := range []string{"XPOST_X_CLIENT_ID", "XPOST_X_CLIENT_SECRET", "XPOST_X_CLIENT_SECRET_FILE", "XPOST_ADDR", "XPOST_BASE_URL",
		"XPOST_SCHEDULER", "XPOST_COST_POST_USD", "XPOST_COST_LINK_POST_USD", "XPOST_X_POST_URL", "XPOST_X_WEB_URL",
		"XPOST_X_AUTHORIZE_URL", "XPOST_X_TOKEN_URL", "XPOST_X_REVOKE_URL", "XPOST_X_ME_URL",
		"XPOST_XSTATS_URL", "XPOST_BLOG_LIVE", "XPOST_BLOG_IMPORT"} {
		t.Setenv(v, "")
	}
	return filepath.Join(t.TempDir(), "xpost.db")
}

var lastMode string
var lastBlogPub blog.Publisher

func recordDispatch(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := startDispatcher
	lastMode = "unset"
	startDispatcher = func(_ context.Context, _ *store.Store, mode string, _ *xauth.Manager, _ *time.Location, _ io.Writer, _ chan struct{}, bp blog.Publisher) <-chan struct{} {
		*n++
		lastMode, lastBlogPub = mode, bp
		return nil
	}
	t.Cleanup(func() { startDispatcher = old })
	return n
}

// recordSweep counts startSweeper calls without running a sweep.
func recordSweep(t *testing.T) *int {
	t.Helper()
	n := new(int)
	old := startSweeper
	startSweeper = func(_ context.Context, _ *store.Store, _ io.Writer) <-chan struct{} {
		*n++
		return nil
	}
	t.Cleanup(func() { startSweeper = old })
	return n
}

// fakeXEnv points every X endpoint at a loopback address that is never
// called, with an invented client id.
func fakeXEnv(t *testing.T) {
	t.Helper()
	base := "http://127.0.0.1:9"
	t.Setenv("XPOST_X_CLIENT_ID", "example-client-id")
	t.Setenv("XPOST_X_AUTHORIZE_URL", base+"/i/oauth2/authorize")
	t.Setenv("XPOST_X_TOKEN_URL", base+"/2/oauth2/token")
	t.Setenv("XPOST_X_REVOKE_URL", base+"/2/oauth2/revoke")
	t.Setenv("XPOST_X_ME_URL", base+"/2/users/me")
	t.Setenv("XPOST_X_POST_URL", base+"/2/tweets")
	t.Setenv("XPOST_X_WEB_URL", "https://x.invalid")
}

// XPOST_SCHEDULER=on is the only way the real poster starts.
func TestSchedulerOnStartsDispatcher(t *testing.T) {
	dbp := testEnv(t)
	fakeXEnv(t)
	t.Setenv("XPOST_SCHEDULER", "on")
	n := recordDispatch(t)
	code, logs := serveFor(t, "-db", dbp, "-base-url", "https://post.example.com", "-trust-proxy")
	if code != 0 || *n != 1 || lastMode != schedOn || !strings.Contains(logs, "scheduler ON") || strings.Contains(logs, "posting off") {
		t.Fatalf("exit %d, dispatcher %d mode %q: %s", code, *n, lastMode, logs)
	}
	if !strings.Contains(logs, "cost rates unverified") {
		t.Fatalf("no unverified-rates warning: %s", logs)
	}
}

func TestSchedulerFlagValues(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, "on": true, " on ": true} {
		got, err := schedulerFromEnv(v)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"1", "true", "yes", "ON", "enabled"} {
		if _, err := schedulerFromEnv(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
}

func TestSchedulerRefusals(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
		want string
	}{
		{"typo", map[string]string{"XPOST_SCHEDULER": "yes"}, nil, "must be on or off"},
		{"no X config", map[string]string{"XPOST_SCHEDULER": "on"}, nil, "needs X connect configured"},
		{"both", map[string]string{"XPOST_SCHEDULER": "on"}, []string{"-fake-poster"}, "not both"},
		{"one rate", map[string]string{"XPOST_COST_POST_USD": "0.5"}, nil, "or neither"},
		{"bad rate", map[string]string{"XPOST_COST_POST_USD": "abc", "XPOST_COST_LINK_POST_USD": "1"}, nil, "dollar amount"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dbp := testEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			n := recordDispatch(t)
			var out, errw syncBuf
			args := append([]string{"serve", "-addr", "127.0.0.1:0", "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies"}, c.args...)
			code := run(context.Background(), args, &out, &errw)
			if code != 1 || *n != 0 || !strings.Contains(errw.String(), c.want) {
				t.Fatalf("exit %d, dispatcher %d: %s", code, *n, errw.String())
			}
		})
	}
}

// serveFor runs `xpost serve` until it is listening, then stops it.
func serveFor(t *testing.T, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errw syncBuf
	done := make(chan int, 1)
	go func() { done <- run(ctx, append([]string{"serve", "-addr", "127.0.0.1:0"}, args...), &out, &errw) }()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case code := <-done:
			return code, errw.String()
		case <-deadline:
			t.Fatalf("serve did not start: %s", errw.String())
		case <-time.After(20 * time.Millisecond):
			if strings.Contains(errw.String(), " listening on ") {
				cancel()
				return <-done, errw.String()
			}
		}
	}
}

// Production runs serve without -fake-poster: no dispatcher, no poster.
func TestServeWithoutFakePosterStartsNoDispatcher(t *testing.T) {
	dbp := testEnv(t)
	n := recordDispatch(t)
	sw := recordSweep(t)
	code, logs := serveFor(t, "-db", dbp, "-base-url", "https://post.example.com", "-trust-proxy")
	if code != 0 || *n != 0 {
		t.Fatalf("exit %d, dispatcher started %d times: %s", code, *n, logs)
	}
	// The promo 24h sweep runs with posting off.
	if *sw != 1 {
		t.Fatalf("promo sweep started %d times with posting off", *sw)
	}
	if !strings.Contains(logs, "posting off: no dispatcher or poster is running") || strings.Contains(logs, "fake poster on") {
		t.Fatalf("logs: %s", logs)
	}
	if !strings.Contains(logs, "X connect off") {
		t.Fatalf("X client from the environment leaked into the test: %s", logs)
	}
}

func TestFakePosterRefusedOffLoopback(t *testing.T) {
	dbp := testEnv(t)
	n := recordDispatch(t)
	for _, base := range []string{"https://post.example.com", "http://192.0.2.10:8090", "https://localhost.example.com"} {
		var out, errw syncBuf
		code := run(context.Background(), []string{"serve", "-addr", "127.0.0.1:0", "-db", dbp, "-base-url", base, "-fake-poster"}, &out, &errw)
		if code != 1 || *n != 0 || !strings.Contains(errw.String(), "local development only") {
			t.Fatalf("%s: exit %d, dispatcher %d: %s", base, code, *n, errw.String())
		}
	}
}

func TestFakePosterStartsOnlyWhenAskedLocally(t *testing.T) {
	dbp := testEnv(t)
	n := recordDispatch(t)
	code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies", "-fake-poster")
	if code != 0 || *n != 1 || lastMode != schedFake || !strings.Contains(logs, "fake poster on") {
		t.Fatalf("exit %d, dispatcher %d: %s", code, *n, logs)
	}
}

// Only startDispatcher may build a Dispatcher or call Loop/RunDue, so the
// tests above cover every way a poster can start.
func TestDispatcherStartedInOnePlace(t *testing.T) {
	root := filepath.Join("..", "..")
	var hits []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if sel, ok := x.Type.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Dispatcher" || sel.Sel.Name == "XPoster") {
					hits = append(hits, p+": "+sel.Sel.Name+"{}")
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Loop" || sel.Sel.Name == "RunDue") && !strings.HasPrefix(filepath.ToSlash(p), "../../internal/poster/") && !strings.HasPrefix(filepath.ToSlash(p), "../../internal/blog/") {
					hits = append(hits, p+": "+sel.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	want := []string{"cmd/xpost/main.go: XPoster{}", "cmd/xpost/main.go: Dispatcher{}", "cmd/xpost/main.go: Dispatcher{}", "cmd/xpost/main.go: Loop", "cmd/xpost/main.go: Loop"}
	ok := len(hits) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = strings.HasSuffix(hits[i], want[i])
	}
	if !ok {
		t.Fatalf("dispatcher construction or loop outside startDispatcher: %v", hits)
	}
}

func TestLoopbackURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8090": true, "http://127.0.0.1:8097": true, "http://[::1]:8090": true, "https://localhost": true,
		"https://post.example.org": false, "https://localhost.example.com": false, "http://10.0.0.5": false, "file:///tmp": false, "": false,
	} {
		if got := loopbackURL(raw); got != want {
			t.Errorf("loopbackURL(%q) = %v", raw, got)
		}
	}
}

func TestBackupCommand(t *testing.T) {
	dbp := testEnv(t)
	var out, errw syncBuf
	if code := run(context.Background(), []string{"create-owner", "-db", dbp, "-email", "owner@example.com", "-name", "Sample Owner"}, &out, &errw); code != 0 {
		t.Fatalf("create-owner: %s", errw.String())
	}
	dest := filepath.Join(t.TempDir(), "b.db")
	if code := run(context.Background(), []string{"backup", "-db", dbp, "-out", dest}, &out, &errw); code != 0 {
		t.Fatalf("backup: %s", errw.String())
	}
	if code := run(context.Background(), []string{"backup", "-db", dbp, "-out", dest}, &out, &errw); code != 1 {
		t.Fatal("backup overwrote its destination")
	}
	if code := run(context.Background(), []string{"backup", "-db", dbp}, &out, &errw); code != 1 {
		t.Fatal("backup without -out")
	}
}

// XPOST_XSTATS_URL is optional; a bad value stops serve before it listens.
func TestXstatsURLAtStartup(t *testing.T) {
	dbp := testEnv(t)
	recordDispatch(t)
	code, logs := serveFor(t, "-db", dbp, "-base-url", "https://post.example.com")
	if code != 0 || !strings.Contains(logs, "xstats links off (XPOST_XSTATS_URL not set)") {
		t.Fatalf("unset: exit %d: %s", code, logs)
	}
	t.Setenv("XPOST_XSTATS_URL", "https://stats.example.com/")
	code, logs = serveFor(t, "-db", dbp, "-base-url", "https://post.example.com")
	if code != 0 || !strings.Contains(logs, "xstats links on (stats.example.com)") {
		t.Fatalf("valid: exit %d: %s", code, logs)
	}
	for _, v := range []string{"http://stats.example.com", "stats.example.com", "javascript:alert(1)", "https://user:pw@stats.example.com", "https://stats.example.com/?a=1", "https://stats.example.com/#post/1", "https://"} {
		t.Setenv("XPOST_XSTATS_URL", v)
		var out, errw syncBuf
		code := run(context.Background(), []string{"serve", "-addr", "127.0.0.1:0", "-db", dbp, "-base-url", "https://post.example.com"}, &out, &errw)
		if code != 1 || !strings.Contains(errw.String(), "XPOST_XSTATS_URL") || strings.Contains(errw.String(), " listening on ") {
			t.Fatalf("%q: exit %d: %s", v, code, errw.String())
		}
		if strings.Contains(errw.String(), "pw@") {
			t.Fatalf("%q: the refusal echoed credentials: %s", v, errw.String())
		}
	}
}

// serve logs the promo sweep once at startup, with no secrets or env values.
func TestServeLogsPromoSweepOnce(t *testing.T) {
	dbp := testEnv(t)
	recordDispatch(t)
	fakeXEnv(t)
	t.Setenv("XPOST_X_CLIENT_SECRET", "example-client-secret-not-real")
	code, logs := serveFor(t, "-db", dbp, "-base-url", "https://post.example.com", "-trust-proxy")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, logs)
	}
	if n := strings.Count(logs, "promo sweep every 1m"); n != 1 {
		t.Fatalf("promo sweep startup line %d times: %s", n, logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "promo sweep") && (strings.Contains(line, "example-client") || strings.Contains(line, "XPOST_") || strings.Contains(line, "=")) {
			t.Fatalf("sweep line carries config: %q", line)
		}
	}
	if strings.Contains(logs, "example-client-secret-not-real") {
		t.Fatal("a secret reached the logs")
	}
}
