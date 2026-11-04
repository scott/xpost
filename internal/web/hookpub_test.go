package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/hookpub"
	"github.com/scott/xpost/internal/blog/hookpub/hfake"
	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

const (
	pubKey  = "sample-signing-key-0000000000000000"
	pubAuth = "Bearer example-routine-value-0000"
)

// hookRig is xpost with one webhook blog wired to a TLS fake agent and
// site (hfake) through Server.BlogHTTP, the blog's Publishing token, and a
// dispatcher using the webhook adapter. The test plays the agent.
type hookRig struct {
	e      *env
	f      *hfake.Server
	ts     *httptest.Server
	b      *store.Blog
	c      *client
	d      *blog.Dispatcher
	tok    *store.AgentToken
	sec    string // the Publishing token's secret
	writer string // a drafts:write token for proposing posts
}

func newHookRig(t *testing.T) *hookRig {
	f := &hfake.Server{Key: pubKey}
	ts := httptest.NewTLSServer(f.Handler())
	t.Cleanup(ts.Close)
	n := 0
	hp := &hookpub.Publisher{NewID: func() string { n++; return fmt.Sprintf("d_sample%04d", n) }}
	hi := &hookpub.Publisher{}
	e := newEnv(t, func(c *Config) {
		c.BaseURL = "http://localhost:8090" // a loopback base: the guarded dialer may reach the loopback fake
		c.WebhookHTTP = ts.Client()         // contributes only its TLS roots
		c.BlogTest = func(ctx context.Context, b *store.Blog) (string, error) { return blog.Methods{Hook: hp}.Test(ctx, b) }
		c.BlogImport = hi.List
		c.ImportAllow = importAllowed
	})
	hp.HTTP, hp.Now, hp.Secret = e.srv.BlogHTTP, e.clock, e.st.BlogSecret
	hi.HTTP, hi.Now = e.srv.BlogImportHTTP, e.clock // the import's own reader: no Secret
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-publisher", store.ScopeBlogsPublish)
	_, writer := e.token("sample-agent")
	ctx := context.Background()
	b, err := e.st.CreateBlog(ctx, e.owner, store.BlogIn{Name: "notes.example.test", SiteURL: ts.URL, Method: store.MethodWebhook, Zone: "America/Denver", PublishTokenID: tok.ID})
	if err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{store.BlogHookURL: ts.URL + "/hook", store.BlogHookKey: pubKey, store.BlogHookAuth: pubAuth} {
		if err := e.st.SetBlogSecret(ctx, e.owner, b.ID, f, v); err != nil {
			t.Fatal(err)
		}
	}
	// Settings' Test sends a signed blog.test wake.
	if r := c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}}); !strings.Contains(r.loc, "done=tested") {
		t.Fatalf("test: %d %s", r.status, r.loc)
	}
	if w := f.Wakes(); len(w) != 1 || w[0].Event != "blog.test" || !w[0].SigOK || w[0].Auth != pubAuth {
		t.Fatalf("test wake %+v", w)
	}
	b, _ = e.st.BlogByID(ctx, b.ID)
	return &hookRig{e: e, f: f, ts: ts, b: b, c: c, tok: tok, sec: sec, writer: writer,
		d: &blog.Dispatcher{Store: e.st, Publisher: blog.Methods{Hook: hp}, NewID: hp.NewID}}
}

// propose submits a post (with a cover when withCover) and publishes it now.
func (h *hookRig) propose(t *testing.T, slug string, withCover bool) int64 {
	body := blogBody("notes.example.test", slug, "Example opening paragraph.\n\nA second paragraph.")
	if withCover {
		var out struct {
			Ref string `json:"media_ref"`
		}
		json.Unmarshal([]byte(upload(h.e, h.writer, pngBytes(t)).body), &out)
		body["cover"] = map[string]string{"ref": out.Ref, "alt": "Example alt text."}
	}
	o := decodeBlog(t, h.e.api("POST", "/api/v1/drafts", h.writer, body))
	if o.ID == 0 {
		t.Fatalf("propose: %+v", o)
	}
	d := h.e.draft(o.ID)
	if err := h.e.st.PublishNow(context.Background(), h.e.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func (h *hookRig) run(t *testing.T) {
	t.Helper()
	if _, err := h.d.RunDue(context.Background(), h.e.clock()); err != nil {
		t.Fatal(err)
	}
}

type pulled struct {
	PublishID int64  `json:"publish_id"`
	PostID    int64  `json:"post_id"`
	Version   int    `json:"version"`
	Attempt   int    `json:"attempt"`
	State     string `json:"state"`
	Key       string `json:"idempotency_key"`
	blog.HookPayload
	ExpiresAt string `json:"expires_at"`
}

func (h *hookRig) pull(t *testing.T, id int64, bearer string) (pulled, resp) {
	t.Helper()
	r := h.e.api("GET", "/api/v1/blog-publishes/"+strconvI(id), bearer, nil)
	var p pulled
	json.Unmarshal([]byte(r.body), &p)
	return p, r
}

func (h *hookRig) report(id int64, p pulled, extra map[string]any) resp {
	body := map[string]any{"attempt": p.Attempt, "idempotency_key": p.Key, "content_sha256": p.ContentSHA}
	for k, v := range extra {
		body[k] = v
	}
	return h.e.api("POST", "/api/v1/blog-publishes/"+strconvI(id)+"/report", h.sec, body)
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

const mergeSHA = "0123456789abcdef0123456789abcdef01234567"

func TestWebhookPublishFlow(t *testing.T) {
	h := newHookRig(t)
	e, ctx := h.e, context.Background()
	id := h.propose(t, "sample-week", true)
	h.run(t)
	// The wake is thin and signed.
	w := h.f.Wakes()
	if len(w) != 2 || w[1].Event != "blog.publish" || !w[1].SigOK || w[1].Auth != pubAuth || w[1].Delivery == "" {
		t.Fatalf("wakes %+v", w)
	}
	if len(w[1].Body) != 5 || w[1].Body["publish_id"] != float64(1) || w[1].Body["delivery_id"] != w[1].Delivery {
		t.Fatalf("wake body %s", w[1].Raw)
	}
	if d := e.draft(id); d.Status != store.StatusPosting {
		t.Fatalf("status %s", d.Status)
	}
	if q := h.c.get("/queue?kind=blog&status=approved").body; !strings.Contains(q, "Sent to the publishing agent at") {
		t.Error("card lacks the publish status line")
	}
	// The agent pulls the authoritative payload and proves the hash.
	p, r := h.pull(t, 1, h.sec)
	if r.status != 200 || p.PostID != id || p.Attempt != 1 || p.Key != fmt.Sprintf("xpost-blog-%d-1", id) || float64(p.Version) != w[1].Body["version"] {
		t.Fatalf("pull %d %s", r.status, r.body)
	}
	if p.HTML != string(blogtext.Render(p.Markdown)) || p.Zone != "America/Denver" || p.DateLocal != "2026-06-01" || p.DateUTC != "2026-06-01" || p.LiveURL != h.ts.URL+"/blog/sample-week/" {
		t.Fatalf("payload %+v", p.HookPayload)
	}
	if strings.Contains(r.body, "base64") || p.Cover == nil || p.Cover.Path != "/api/v1/blog-publishes/1/cover" {
		t.Fatalf("cover %s", r.body)
	}
	cv := e.api("GET", p.Cover.Path, h.sec, nil)
	if cv.status != 200 || sum([]byte(cv.body)) != p.Cover.SHA256 {
		t.Fatalf("cover bytes %d", cv.status)
	}
	again := p.HookPayload
	again.Canonical, again.ContentSHA, again.Marker = "", "", ""
	if c := blog.Canonical(again); c != p.Canonical || sum([]byte(c)) != p.ContentSHA {
		t.Fatal("the canonical form does not rebuild from the pulled fields")
	}
	// Reports: opened, then merged.
	if rr := h.report(1, p, map[string]any{"status": "opened", "pr": 5}); rr.status != 200 || !strings.Contains(rr.body, `"opened"`) {
		t.Fatalf("opened %d %s", rr.status, rr.body)
	}
	if rr := h.report(1, p, map[string]any{"status": "merged", "pr": 5, "merge_sha": mergeSHA}); rr.status != 200 {
		t.Fatalf("merged %d %s", rr.status, rr.body)
	}
	if rr := h.report(1, p, map[string]any{"status": "merged", "pr": 5, "merge_sha": mergeSHA}); rr.status != 200 {
		t.Fatalf("merged again %d", rr.status)
	}
	if q := h.c.get("/queue?kind=blog&status=approved").body; !strings.Contains(q, "Pull request #5 merged as 0123456. Checking the live page") {
		t.Error("card lacks the merged line")
	}
	// The deploy lands; the first live check (merge + 1 min) confirms it.
	h.f.Deploy("/blog/sample-week/", hfake.Page("Example", p.Marker, p.HTML))
	e.advance(61 * time.Second)
	h.run(t)
	d := e.draft(id)
	if d.Status != store.StatusPosted || d.Post.PublishedURL != p.LiveURL || d.Post.Ref != "hook:sample-week@"+strconvI(id) {
		t.Fatalf("published: %s %q %q %q", d.Status, d.Post.PublishedURL, d.Post.Ref, d.Error)
	}
	for _, g := range h.f.Gets() {
		if strings.HasSuffix(g, "auth=true") {
			t.Fatal("the live check sent credentials")
		}
	}
	// Done: nothing more is served or accepted.
	if _, r := h.pull(t, 1, h.sec); r.status != 409 || !strings.Contains(r.body, "not_in_flight") {
		t.Fatalf("after live: %d %s", r.status, r.body)
	}
	if rr := h.report(1, p, map[string]any{"status": "opened", "pr": 5}); rr.status != 409 {
		t.Fatalf("late report %d", rr.status)
	}
	hist := h.c.get("/drafts/" + strconvI(id)).body
	if !strings.Contains(hist, "PR #5 merged as 0123456") {
		t.Error("history lacks the agent report")
	}
	for _, bad := range []string{pubKey, pubAuth, h.sec, "/hook"} {
		if strings.Contains(e.logs.String(), bad) {
			t.Errorf("logs show %q", bad)
		}
	}
	_ = ctx
}

// Only the blog's Publishing token sees its publishes; other scopes are
// refused and other tokens get 404.
func TestWebhookPublishOwnOnly(t *testing.T) {
	h := newHookRig(t)
	h.propose(t, "sample-week", false)
	h.run(t)
	_, other := h.e.token("other-publisher", store.ScopeBlogsPublish)
	if _, r := h.pull(t, 1, other); r.status != 404 {
		t.Fatalf("other token %d", r.status)
	}
	if _, r := h.pull(t, 1, h.writer); r.status != 403 {
		t.Fatalf("drafts:write token %d", r.status)
	}
	if r := h.e.api("GET", "/api/v1/blog-publishes/1/cover", h.sec, nil); r.status != 404 {
		t.Fatalf("no cover %d", r.status)
	}
	p, r := h.pull(t, 1, h.sec)
	if r.status != 200 {
		t.Fatal(r.body)
	}
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"status": "merged", "pr": 5, "merge_sha": "https://example.test/x"}, 400},
		{map[string]any{"status": "failed", "code": "something_else"}, 400},
		{map[string]any{"status": "opened", "pr": 5, "url": "https://example.test/pr"}, 400},
		{map[string]any{"status": "opened", "pr": 5, "attempt": 2}, 409},
	} {
		if rr := h.report(1, p, c.body); rr.status != c.want {
			t.Errorf("%v: %d %s", c.body, rr.status, rr.body)
		}
	}
	// Rebinding the blog cuts the old token off at once.
	in := store.BlogIn{Name: h.b.Name, SiteURL: h.b.SiteURL, Method: store.MethodWebhook}
	if _, err := h.e.st.UpdateBlog(context.Background(), h.e.owner, h.b.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, r := h.pull(t, 1, h.sec); r.status != 404 {
		t.Fatalf("unbound %d", r.status)
	}
}

// No answer: live checks at +10, +30 and +60 min, then Failed with one
// Retry. Retry reuses the publish (same key, attempt 2).
func TestWebhookPublishTimeoutAndRetry(t *testing.T) {
	h := newHookRig(t)
	e, ctx := h.e, context.Background()
	id := h.propose(t, "sample-week", false)
	h.run(t)
	first, _ := h.pull(t, 1, h.sec)
	for _, m := range []time.Duration{10, 20, 30} {
		e.advance(m*time.Minute + time.Second)
		h.run(t)
	}
	d := e.draft(id)
	if d.Status != store.StatusFailed || !strings.HasPrefix(d.Error, "No answer from the publishing agent within 60 minutes.") {
		t.Fatalf("timeout: %s %q", d.Status, d.Error)
	}
	if n := len(h.f.Gets()); n != 3 {
		t.Fatalf("live checks %d, want 3", n)
	}
	if strings.Contains(d.Error, h.ts.URL) {
		t.Fatal("error shows the URL")
	}
	q := h.c.get("/queue?kind=blog").body
	if n := strings.Count(q, "Retry publish</button>"); n != 1 {
		t.Fatalf("failed card has %d Retry buttons, want 1", n)
	}
	if err := e.st.PublishNow(ctx, e.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	w := h.f.Wakes()
	p, r := h.pull(t, 1, h.sec)
	if r.status != 200 || p.Attempt != 2 || p.Key != fmt.Sprintf("xpost-blog-%d-1", id) || w[len(w)-1].Body["publish_id"] != float64(1) || w[len(w)-1].Delivery == w[len(w)-2].Delivery {
		t.Fatalf("retry: %d %+v", r.status, p)
	}
	// Same local day: everything is reused, dates and hash included.
	if p.ContentSHA != first.ContentSHA || p.DateLocal != first.DateLocal || p.DateUTC != first.DateUTC || p.PublishedAt != first.PublishedAt {
		t.Fatal("a same-day Retry changed the payload")
	}
	// The agent finds a hand-made post on the slug: Blocked.
	if rr := h.report(1, p, map[string]any{"status": "failed", "code": "slug_exists"}); rr.status != 200 {
		t.Fatalf("failed report %d %s", rr.status, rr.body)
	}
	d = e.draft(id)
	if d.Status != store.StatusFailed || !strings.HasPrefix(d.Error, store.BlogSlugTakenPrefix+"notes.example.test.") {
		t.Fatalf("blocked: %q", d.Error)
	}
	q = h.c.get("/queue?kind=blog").body
	if !strings.Contains(q, "Blocked at ") || strings.Contains(q, "Retry publish") {
		t.Error("slug_exists is not shown as Blocked")
	}
}

// A refused wake fails at once with the status only; a merged post whose
// page never shows the marker fails after +30 min.
func TestWebhookPublishFailures(t *testing.T) {
	h := newHookRig(t)
	e := h.e
	h.f.SetStatus(500)
	id := h.propose(t, "sample-week", false)
	h.run(t)
	if d := e.draft(id); d.Status != store.StatusFailed || d.Error != "Publish webhook failed: HTTP 500. Nothing more was sent." {
		t.Fatalf("wake 500: %q", d.Error)
	}
	h.f.SetStatus(0)
	id2 := h.propose(t, "sample-two", false)
	h.run(t)
	p, _ := h.pull(t, 2, h.sec)
	h.report(2, p, map[string]any{"status": "merged", "pr": 6, "merge_sha": mergeSHA})
	h.f.Deploy("/blog/sample-two/", hfake.Page("Example", blog.Marker(strings.Repeat("c", 64)), "")) // another version
	for _, m := range []time.Duration{1, 2, 7, 20} {
		e.advance(m*time.Minute + time.Second)
		h.run(t)
	}
	if d := e.draft(id2); d.Status != store.StatusFailed || !strings.HasPrefix(d.Error, "Merged, but the live page did not show this version yet.") {
		t.Fatalf("merged no marker: %s %q", d.Status, d.Error)
	}
}

// Settings: the Publishing token picker lists only active blogs:publish
// tokens; the zone is validated.
func TestWebhookBlogSettings(t *testing.T) {
	h := newHookRig(t)
	page := h.c.get("/settings/blogs?open=" + strconvI(h.b.ID)).body
	if !strings.Contains(page, "sample-publisher (xpt_") || strings.Contains(page, ">sample-agent (") || !strings.Contains(page, `name="zone" value="America/Denver"`) {
		t.Fatal("settings lack the token picker or zone")
	}
	form := url.Values{"op": {"save"}, "name": {h.b.Name}, "site_url": {h.b.SiteURL}, "method": {"webhook"}, "clock": {"07:00"}, "zone": {"Mars/Base"}, "publish_token": {strconvI(h.tok.ID)}}
	if r := h.c.post("/settings/blogs?open="+strconvI(h.b.ID), "/settings/blogs/"+strconvI(h.b.ID), form); r.status != 400 || !strings.Contains(r.body, "Unknown time zone Mars/Base") {
		t.Fatalf("bad zone: %d", r.status)
	}
	writer, _ := h.e.st.AgentTokens(context.Background())
	for _, tk := range writer {
		if tk.Name == "sample-agent" {
			form.Set("zone", "")
			form.Set("publish_token", strconvI(tk.ID))
			if r := h.c.post("/settings/blogs?open="+strconvI(h.b.ID), "/settings/blogs/"+strconvI(h.b.ID), form); r.status != 400 {
				t.Fatalf("a drafts:write token was accepted as the publishing token: %d", r.status)
			}
		}
	}
	// The token form offers blogs:publish.
	if !strings.Contains(h.c.get("/log").body, `name="blogs_publish"`) {
		t.Fatal("no blogs:publish checkbox")
	}
}

// A webhook blog with no Publishing token fails before any call.
func TestWebhookNeedsPublishingToken(t *testing.T) {
	h := newHookRig(t)
	in := store.BlogIn{Name: h.b.Name, SiteURL: h.b.SiteURL, Method: store.MethodWebhook}
	h.e.st.UpdateBlog(context.Background(), h.e.owner, h.b.ID, in)
	id := h.propose(t, "sample-week", false)
	h.run(t)
	if d := h.e.draft(id); d.Status != store.StatusFailed || d.Error != "Pick a publishing token for notes.example.test in Settings > Blogs, then retry." {
		t.Fatalf("%q", d.Error)
	}
	if n := len(h.f.Wakes()); n != 1 { // only the setup's blog.test
		t.Fatalf("wakes %d", n)
	}
	_ = http.StatusOK
}

// A Retry on a later day in the blog's zone takes the new dates, with a
// new publish, key and hash; the old publish is no longer served.
func TestWebhookRetryLaterDayTakesNewDate(t *testing.T) {
	h := newHookRig(t)
	e, ctx := h.e, context.Background()
	h.f.SetStatus(500)
	id := h.propose(t, "sample-week", false)
	h.run(t)
	d := e.draft(id)
	if d.Status != store.StatusFailed {
		t.Fatalf("status %s", d.Status)
	}
	old, _ := e.st.BlogPublishByID(ctx, 1)
	// 9 AM in Denver on June 1; Retry at 9 AM on June 2.
	e.advance(24 * time.Hour)
	h.f.SetStatus(0)
	if err := e.st.PublishNow(ctx, e.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	h.run(t)
	p, r := h.pull(t, 2, h.sec)
	if r.status != 200 || p.Attempt != 1 || p.Key != fmt.Sprintf("xpost-blog-%d-2", id) || p.DateLocal != "2026-06-02" || p.DateUTC != "2026-06-02" || p.ContentSHA == old.ContentSHA {
		t.Fatalf("later-day retry: %d %+v", r.status, p)
	}
	if w := h.f.Wakes(); w[len(w)-1].Body["publish_id"] != float64(2) {
		t.Fatalf("wake %s", w[len(w)-1].Raw)
	}
	if _, r := h.pull(t, 1, h.sec); r.status != 409 {
		t.Fatalf("old publish still served: %d", r.status)
	}
}
