package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// fastLive makes the live hub tail the feed every 10ms with a 50ms heartbeat.
func fastLive(c *Config) {
	c.LivePoll, c.LiveHeartbeat = 10*time.Millisecond, 50*time.Millisecond
}

type sseEvent struct {
	id, event, data string
}

type sseConn struct {
	t      *testing.T
	status int
	h      http.Header
	ev     chan sseEvent
	cancel context.CancelFunc
	mu     sync.Mutex
	raw    strings.Builder
	done   chan struct{}
}

// openSSE opens path with the client's cookies (and optional headers).
func openSSE(t *testing.T, e *env, c *http.Client, path string, hdr map[string]string) *sseConn {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := c.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &sseConn{t: t, status: r.StatusCode, h: r.Header, ev: make(chan sseEvent, 256), cancel: cancel, done: make(chan struct{})}
	t.Cleanup(s.close)
	go func() {
		defer close(s.done)
		defer r.Body.Close()
		sc := bufio.NewScanner(r.Body)
		var cur sseEvent
		for sc.Scan() {
			line := sc.Text()
			s.mu.Lock()
			s.raw.WriteString(line + "\n")
			s.mu.Unlock()
			switch {
			case line == "":
				if cur.event != "" || cur.data != "" {
					s.ev <- cur
				}
				cur = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				cur.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				cur.data = line[6:]
			case strings.HasPrefix(line, "id: "):
				cur.id = line[4:]
			case strings.HasPrefix(line, ":"):
				s.ev <- sseEvent{event: "comment", data: line}
			}
		}
	}()
	return s
}

func (s *sseConn) close() { s.cancel(); <-s.done }

func (s *sseConn) text() string { s.mu.Lock(); defer s.mu.Unlock(); return s.raw.String() }

// wait returns the first event named name (and matching ok, if given).
func (s *sseConn) wait(name string, ok func(sseEvent) bool) sseEvent {
	s.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, open := <-s.ev:
			if !open {
				s.t.Fatalf("stream closed waiting for %s", name)
			}
			if ev.event == name && (ok == nil || ok(ev)) {
				return ev
			}
		case <-s.done:
			s.t.Fatalf("stream ended waiting for %s", name)
		case <-deadline:
			s.t.Fatalf("no %s event within 5s; got:\n%s", name, s.text())
		}
	}
}

func changeFor(id int64, kind string) func(sseEvent) bool {
	return func(ev sseEvent) bool {
		var c store.Change
		return json.Unmarshal([]byte(ev.data), &c) == nil && c.ID == id && (kind == "" || c.Kind == kind)
	}
}

func newLiveRewriteEnv(t *testing.T) *rwEnv {
	h := newHookRecv(t)
	e := newEnv(t, func(c *Config) { loopbackHooks(h)(c); fastLive(c) })
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead, store.ScopeDraftsRevise)
	r := c.post("/settings", "/settings/webhooks/"+strconvI(tok.ID), url.Values{"op": {"save"}, "url": {h.srv.URL + "/hook"}, "key": {hookKey}})
	if r.status != http.StatusSeeOther {
		t.Fatalf("save webhook: %d", r.status)
	}
	return &rwEnv{e, c, h, tok, sec}
}

// The stream reports a new agent draft, a rewrite sent and answered, a
// rewrite that ran out of time, an owner status change and a scheduler
// status change, each as ids, a kind and a version only.
func TestLiveStreamEvents(t *testing.T) {
	e := newLiveRewriteEnv(t)
	s := openSSE(t, e.env, e.c.c, "/live/events", nil)
	if s.status != http.StatusOK {
		t.Fatalf("stream: %d", s.status)
	}
	if ct := s.h.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") || s.h.Get("X-Accel-Buffering") != "no" || !strings.Contains(s.h.Get("Cache-Control"), "no-transform") {
		t.Fatalf("headers: %v", s.h)
	}
	s.wait("hello", nil)

	const secretText = "Sample secret-ish draft text 7f3a"
	id := e.propose(e.sec, sm, testNow.Add(3*time.Hour), secretText)
	s.wait("change", changeFor(id, "draft"))

	e.rewrite(id, "Sample rewrite note 9q")
	s.wait("change", changeFor(id, "rewrite"))
	req := e.lastRequestID(id)
	if r := e.revise(e.sec, id, req, "Sample revised text 2k"); r.status != http.StatusOK {
		t.Fatalf("revise: %d %s", r.status, r.body)
	}
	rv := s.wait("change", changeFor(id, "draft"))
	var c store.Change
	json.Unmarshal([]byte(rv.data), &c)
	if c.V < 2 {
		t.Fatalf("revised version %d", c.V)
	}

	// A rewrite that runs out of time writes nothing; the hub still says so.
	id2 := e.propose(e.sec, sm, testNow.Add(4*time.Hour), "Sample second draft")
	e.rewrite(id2, "Sample note")
	s.wait("change", changeFor(id2, "rewrite"))
	e.advance(61 * time.Minute)
	s.wait("change", changeFor(id2, "rewrite"))
	// The sweep noting it as well announces nothing more.
	if n, err := e.st.MarkRewriteTimeouts(context.Background(), e.st.Now()); err != nil || n != 0 {
		t.Fatalf("second note: %d %v", n, err)
	}

	// Owner approve, then the scheduler finishing the post.
	id3 := e.propose(e.sec, sm, testNow.Add(5*time.Hour), "Sample third draft")
	s.wait("change", changeFor(id3, "draft"))
	e.approveNow(id3)
	s.wait("change", changeFor(id3, "draft"))
	// The scheduler's own claim and finish, as the poster does it.
	d3 := e.draft(id3)
	if ok, err := e.st.Claim(context.Background(), store.Due{ID: id3, Version: d3.Version, Status: store.StatusApproved, Account: sm}, testNow.Add(6*time.Hour)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	s.wait("change", changeFor(id3, "draft"))
	if err := e.st.FinishDraft(context.Background(), id3, store.StatusPosted, ""); err != nil {
		t.Fatal(err)
	}
	s.wait("change", changeFor(id3, "draft"))

	// Every data payload is {seq, kind, id, v} and nothing else.
	raw := s.text()
	for _, bad := range []string{secretText, "Sample rewrite note", "Sample revised text", e.sec, hookKey, "sample-agent", sm, e.hook.srv.URL} {
		if strings.Contains(raw, bad) {
			t.Errorf("stream carries %q", bad)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line[6:]), &m); err != nil {
			t.Fatalf("data %q: %v", line, err)
		}
		for k := range m {
			if k != "seq" && k != "kind" && k != "id" && k != "v" {
				t.Errorf("data has field %q", k)
			}
		}
	}
	if strings.Contains(e.logs.String(), secretText) {
		t.Error("logs carry draft text")
	}
}

// Reconnecting with Last-Event-ID replays what was missed; a seq the feed no
// longer covers gets resync.
func TestLiveResume(t *testing.T) {
	e := newEnv(t, fastLive)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	s := openSSE(t, e, c.c, "/live/events", nil)
	s.wait("hello", nil)
	a := e.propose(sec, sm, testNow.Add(3*time.Hour), "Sample A")
	first := s.wait("change", changeFor(a, "draft"))
	s.close()
	b := e.propose(sec, sm, testNow.Add(4*time.Hour), "Sample B")
	s2 := openSSE(t, e, c.c, "/live/events", map[string]string{"Last-Event-ID": first.id})
	s2.wait("change", changeFor(b, "draft"))
	s3 := openSSE(t, e, c.c, "/live/events?since=999999", nil)
	s3.wait("resync", nil)
}

// Only a full owner session may subscribe or poll: no session, a
// half-finished sign-in, and an agent bearer token all get 401 (never a
// redirect), and nothing is streamed.
func TestLiveAuthRequired(t *testing.T) {
	e := newEnv(t, fastLive)
	e.setupOwner()
	_, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead, store.ScopeDraftsRevise)
	step := e.passwordStep()
	for _, tc := range []struct {
		name string
		c    *http.Client
		hdr  map[string]string
	}{
		{"no session", e.client().c, nil},
		{"agent bearer", e.client().c, map[string]string{"Authorization": "Bearer " + sec}},
		{"password step only", step.c, nil},
	} {
		for _, p := range []string{"/live/events", "/live/poll?since=0"} {
			req, _ := http.NewRequest("GET", e.ts.URL+p, nil)
			for k, v := range tc.hdr {
				req.Header.Set(k, v)
			}
			r := e.do(tc.c, req)
			if r.status != http.StatusUnauthorized || r.loc != "" || strings.Contains(r.body, "event:") {
				t.Errorf("%s %s: %d %q", tc.name, p, r.status, r.loc)
			}
		}
	}
	// The agent API has no live route.
	if r := e.api("GET", "/api/v1/live/events", sec, nil); r.status == http.StatusOK {
		t.Errorf("agent API live route answered %d", r.status)
	}
}

// The poll fallback returns the changes after since, the newest seq, and
// resync when since is ahead of the feed.
func TestLivePoll(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	var out livePollOut
	read := func(p string) {
		t.Helper()
		r := c.get(p)
		if r.status != http.StatusOK || !strings.HasPrefix(r.h.Get("Content-Type"), "application/json") || r.h.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", p, r.status, r.h)
		}
		out = livePollOut{}
		if err := json.Unmarshal([]byte(r.body), &out); err != nil {
			t.Fatal(err)
		}
	}
	read("/live/poll")
	start := out.Seq
	id := e.propose(sec, sm, testNow.Add(3*time.Hour), "Sample poll text")
	read("/live/poll?since=" + strconvI(start))
	if len(out.Changes) == 0 || out.Changes[0].ID != id || out.Changes[0].Kind != "draft" || out.Seq <= start || out.Resync {
		t.Fatalf("poll: %+v", out)
	}
	read("/live/poll?since=" + strconvI(out.Seq))
	if len(out.Changes) != 0 {
		t.Fatalf("nothing new, got %+v", out.Changes)
	}
	read("/live/poll?since=999999")
	if !out.Resync {
		t.Fatal("since ahead of the feed should resync")
	}
	if strings.Contains(c.get("/live/poll?since=0").body, "Sample poll text") {
		t.Fatal("poll carries text")
	}
}

// A session gets at most LiveMaxPerSession streams; another session is
// counted separately; the total cap holds too; closing one frees a slot.
func TestLiveConnectionCap(t *testing.T) {
	e := newEnv(t, func(c *Config) { fastLive(c); c.LiveMaxPerSession, c.LiveMaxTotal = 2, 3 })
	c, k := e.setupOwner()
	a := openSSE(t, e, c.c, "/live/events", nil)
	openSSE(t, e, c.c, "/live/events", nil)
	if a.status != 200 {
		t.Fatalf("first: %d", a.status)
	}
	third := openSSE(t, e, c.c, "/live/events", nil)
	if third.status != http.StatusTooManyRequests || third.h.Get("Retry-After") == "" {
		t.Fatalf("third stream for one session: %d", third.status)
	}
	other := e.login(k)
	if s := openSSE(t, e, other.c, "/live/events", nil); s.status != 200 {
		t.Fatalf("another session: %d", s.status)
	}
	if s := openSSE(t, e, other.c, "/live/events", nil); s.status != http.StatusTooManyRequests {
		t.Fatalf("over the total cap: %d", s.status)
	}
	a.close()
	waitFor(t, func() bool { return e.srv.live.count() == 2 })
	if s := openSSE(t, e, c.c, "/live/events", nil); s.status != 200 {
		t.Fatalf("after closing one: %d", s.status)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 5s")
}

// Streams and polls never move the session's last_seen_at (so an open tab
// still reaches the idle timeout); a live page refetch neither. Signing out
// ends an open stream at the next heartbeat.
func TestLiveIsNotActivity(t *testing.T) {
	e := newEnv(t, fastLive)
	c, _ := e.setupOwner()
	seen := func() int64 {
		var n int64
		if err := e.st.DB.QueryRow(`SELECT MAX(last_seen_at) FROM sessions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := seen()
	e.advance(5 * time.Minute)
	s := openSSE(t, e, c.c, "/live/events", nil)
	s.wait("hello", nil)
	s.wait("comment", nil) // a heartbeat re-checked the session
	c.get("/live/poll?since=0")
	req, _ := http.NewRequest("GET", e.ts.URL+"/queue", nil)
	req.Header.Set(liveHeader, "1")
	e.do(c.c, req)
	if seen() != before {
		t.Fatal("live traffic moved last_seen_at")
	}
	c.get("/queue")
	if seen() == before {
		t.Fatal("a normal page view should count as activity")
	}
	// Sign out: the stream says so and ends.
	if r := c.post("/queue", "/logout", nil); r.status != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.status)
	}
	s.wait("signedout", nil)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after sign-out")
	}
}

// Live pages carry their feed position; POST re-renders and a Log page
// showing a new token do not (nothing may refetch them).
func TestLivePageMarkers(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(3*time.Hour), "Sample marker text")
	for p, kind := range map[string]string{"/queue": "queue", "/queue?status=approved&kind=x": "queue", "/schedule": "schedule", "/log": "log", path(id, ""): "draft"} {
		b := c.get(p).body
		if !strings.Contains(b, `data-live-page="`+kind+`" data-live-seq="`) || !strings.Contains(b, `src="/static/live.js"`) {
			t.Errorf("%s: not marked live (%s)", p, kind)
		}
	}
	if b := c.get(path(id, "")).body; !strings.Contains(b, `data-live-draft="`+strconvI(id)+`" data-live-v="1"`) {
		t.Error("draft page lacks its id and version")
	}
	for _, p := range []string{"/compose", "/settings", "/security"} {
		if strings.Contains(c.get(p).body, "data-live-page") {
			t.Errorf("%s is marked live", p)
		}
	}
	r := c.post("/log", "/tokens", url.Values{"name": {"sample-new"}, "drafts_write": {"1"}})
	if r.status != http.StatusOK || strings.Contains(r.body, "data-live-page") {
		t.Fatalf("new-token page: %d live=%v", r.status, strings.Contains(r.body, "data-live-page"))
	}
	q := c.get("/queue").body
	for _, want := range []string{`data-live="qtop"`, `data-live-list="cards"`, `data-live="nav"`, `data-live="tabs"`, `id="d` + strconvI(id) + `"`} {
		if !strings.Contains(q, want) {
			t.Errorf("queue lacks %s", want)
		}
	}
	if l := c.get("/log").body; !strings.Contains(l, `data-live="spend"`) || !strings.Contains(l, `data-live="postlog"`) {
		t.Error("log lacks its live units")
	}
}

// Shutting down ends open streams at once: the graceful stop does not wait
// for tabs (it would otherwise hold for its whole timeout).
func TestLiveShutdownDoesNotHang(t *testing.T) {
	e := newEnv(t, fastLive)
	c, _ := e.setupOwner()
	srv, err := New(e.st, e.srv.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()
	u, _ := url.Parse(e.ts.URL)
	cookies := c.c.Jar.Cookies(u)
	base := "http://" + ln.Addr().String()
	bu, _ := url.Parse(base)
	c.c.Jar.SetCookies(bu, cookies)
	req, _ := http.NewRequest("GET", base+"/live/events", nil)
	resp, err := c.c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("stream on the real server: %v %v", err, resp)
	}
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var all strings.Builder
		for sc.Scan() {
			all.WriteString(sc.Text() + "\n")
		}
		got <- all.String()
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("graceful stop hung on an open stream")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stop took %v", d)
	}
	select {
	case body := <-got:
		if !strings.Contains(body, "event: bye") {
			t.Fatalf("stream did not say bye: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client stream never ended")
	}
	resp.Body.Close()
	if r, err := http.Get(base + "/live/events"); err == nil {
		r.Body.Close()
		t.Fatal("server still answering after stop")
	}
}
