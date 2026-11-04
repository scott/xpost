package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Invented sender key for tests only.
const hookKey = "sample-sender-key-7f3a9c21"

// hookRecv is a local TLS webhook receiver: the only place rewrite tests
// deliver to.
type hookRecv struct {
	mu     sync.Mutex
	srv    *httptest.Server
	status int
	got    []hookHit
}

type hookHit struct {
	h    http.Header
	body []byte
}

func newHookRecv(t *testing.T) *hookRecv {
	h := &hookRecv{status: http.StatusAccepted}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.got = append(h.got, hookHit{r.Header.Clone(), b})
		st := h.status
		h.mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hookRecv) setStatus(n int) { h.mu.Lock(); h.status = n; h.mu.Unlock() }
func (h *hookRecv) hits() []hookHit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookHit(nil), h.got...)
}

type rwEnv struct {
	*env
	c    *client
	hook *hookRecv
	tok  *store.AgentToken
	sec  string
}

// loopbackHooks runs xpost on a loopback base URL (like local dev), the
// only case where a loopback webhook such as the httptest receiver may be
// dialed. DNS is never used: any hostname lookup fails.
func loopbackHooks(h *hookRecv) func(*Config) {
	return func(c *Config) {
		c.BaseURL = testLoopbackBase
		c.WebhookHTTP = h.srv.Client()
		c.WebhookLookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no DNS in tests") }
	}
}

const testLoopbackBase = "http://localhost:8090"

// newRewriteEnv: an owner, an agent token with drafts:revise whose webhook
// points at the local receiver.
func newRewriteEnv(t *testing.T) *rwEnv {
	h := newHookRecv(t)
	e := newEnv(t, loopbackHooks(h))
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead, store.ScopeDraftsRevise)
	r := c.post("/settings", "/settings/webhooks/"+strconvI(tok.ID), url.Values{"op": {"save"}, "url": {h.srv.URL + "/hook"}, "key": {hookKey}})
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=hooksaved") {
		t.Fatalf("save webhook: %d %s %s", r.status, r.loc, r.body)
	}
	return &rwEnv{e, c, h, tok, sec}
}

func (e *rwEnv) rewrite(id int64, note string) resp {
	e.t.Helper()
	return e.c.post("/queue", path(id, "/rewrite"), e.ownerForm(id, url.Values{"note": {note}, "return": {"/queue"}}))
}

func (e *rwEnv) lastRequestID(id int64) int64 {
	e.t.Helper()
	q, err := e.st.LatestRewrite(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return q.ID
}

func (e *rwEnv) revise(bearer string, id, reqID int64, texts ...string) resp {
	e.t.Helper()
	var parts []map[string]any
	for _, s := range texts {
		parts = append(parts, map[string]any{"text": s})
	}
	return e.api("POST", "/api/v1"+path(id, "/revise"), bearer, map[string]any{"request_id": reqID, "parts": parts})
}

func mustHaveRW(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

func mustLackRW(t *testing.T, body string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(body, b) {
			t.Errorf("page has %q", b)
		}
	}
}

// The whole state machine: Pending -> Rewriting -> Pending (revised), with
// Approve and Edit blocked while waiting and the request signed.
func TestRewriteStateMachine(t *testing.T) {
	e := newRewriteEnv(t)
	at := testNow.Add(2 * time.Hour)
	id := e.propose(e.sec, sm, at, "Sample first part about sample tools.", "Sample second part.")
	q := e.c.get("/queue").body
	mustHaveRW(t, q, `class="btn rwb"`, `>from sample-agent<`)

	// The note box opens without JS.
	open := e.c.get("/queue?rewrite=" + strconvI(id)).body
	mustHaveRW(t, open, `class="rwnote"`, `Send back for a rewrite`, `name="note"`, `maxlength="280"`)

	r := e.rewrite(id, "Shorter, sample note")
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=rewrite") || strings.Contains(r.loc, "rewrite=") && !strings.Contains(r.loc, "done=rewrite") {
		t.Fatalf("rewrite: %d %s %s", r.status, r.loc, r.body)
	}
	hits := e.hook.hits()
	if len(hits) != 1 {
		t.Fatalf("webhook got %d requests", len(hits))
	}
	h := hits[0]
	if h.h.Get("X-Xpost-Event") != "rewrite.requested" {
		t.Fatalf("event %q", h.h.Get("X-Xpost-Event"))
	}
	ts := h.h.Get("X-Xpost-Timestamp")
	mac := hmac.New(sha256.New, []byte(hookKey))
	mac.Write([]byte(ts + "." + string(h.body)))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); h.h.Get("X-Xpost-Signature") != want {
		t.Fatal("signature does not verify")
	}
	raw := string(h.body)
	for k, v := range h.h {
		if strings.Contains(strings.Join(v, " "), hookKey) {
			t.Fatalf("header %s carries the key", k)
		}
	}
	mustLackRW(t, raw, hookKey, e.sec, "Bearer", "token_hash")
	var p struct {
		Type      string `json:"type"`
		RequestID int64  `json:"request_id"`
		DraftID   int64  `json:"draft_id"`
		Note      string `json:"note"`
		Parts     []struct {
			Text   string `json:"text"`
			Images int    `json:"images"`
		} `json:"parts"`
		ExpiresAt string `json:"expires_at"`
		Callback  struct {
			Method, URL, Scope string
		} `json:"callback"`
	}
	if err := json.Unmarshal(h.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Type != "rewrite.requested" || p.DraftID != id || p.Note != "Shorter, sample note" || len(p.Parts) != 2 || p.Parts[1].Text != "Sample second part." ||
		p.Callback.Method != "POST" || p.Callback.URL != testLoopbackBase+"/api/v1/drafts/"+strconvI(id)+"/revise" || p.Callback.Scope != "drafts:revise" || p.RequestID == 0 {
		t.Fatalf("payload: %+v", p)
	}
	if exp, _ := time.Parse(time.RFC3339, p.ExpiresAt); !exp.Equal(testNow.Add(60 * time.Minute)) {
		t.Fatalf("expires_at %s", p.ExpiresAt)
	}

	// Rewriting: pill, waiting box, Approve and Edit blocked; Reschedule and Reject stay.
	q = e.c.get("/queue").body
	mustHaveRW(t, q, `st rewriting`, `Waiting on sample-agent, sent 3:00 PM`, `Shorter, sample note`, `Cancel request`, `href="/drafts/`+strconvI(id)+`/reschedule"`, `Reject draft`, `Rejecting also cancels the rewrite request.`)
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, nil)); r.status != http.StatusConflict || !strings.Contains(r.body, "Waiting on sample-agent for a rewrite") {
		t.Fatalf("approve while rewriting: %d", r.status)
	}
	for _, op := range []string{"save", "approve"} {
		if r := e.c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm(op, "Owner edit", at))); r.status != http.StatusConflict || !strings.Contains(r.body, "Waiting on sample-agent for a rewrite") {
			t.Fatalf("edit %s while rewriting: %d", op, r.status)
		}
	}
	if d := e.draft(id); d.Status != store.StatusPending {
		t.Fatalf("status %s", d.Status)
	}
	// Reschedule stays open; reschedule-and-approve is blocked like Approve.
	if r := e.c.post(path(id, "/reschedule"), path(id, "/reschedule"), e.ownerForm(id, url.Values{"op": {"approve"}, "date": {"2026-06-02"}, "time": {"09:30"}})); r.status != http.StatusConflict {
		t.Fatalf("reschedule and approve while rewriting: %d", r.status)
	}
	if r := e.c.post(path(id, "/reschedule"), path(id, "/reschedule"), e.ownerForm(id, url.Values{"op": {"move"}, "date": {"2026-06-02"}, "time": {"09:30"}})); r.status != http.StatusSeeOther {
		t.Fatalf("reschedule while rewriting: %d %s", r.status, r.body)
	}
	if d := e.draft(id); d.Status != store.StatusPending {
		t.Fatalf("reschedule moved status to %s", d.Status)
	}
	mustHaveRW(t, e.c.get("/queue").body, "st rewriting")
	// A second request while one waits is refused.
	if r := e.rewrite(id, ""); r.status != http.StatusConflict {
		t.Fatalf("second rewrite: %d", r.status)
	}
	// The agent API shows the open request.
	g := e.api("GET", "/api/v1"+path(id, ""), e.sec, nil)
	if !strings.Contains(g.body, `"rewrite":{"request_id":`) || !strings.Contains(g.body, `"state":"open"`) {
		t.Fatalf("api get: %s", g.body)
	}

	// The agent answers with a new text and a different part count.
	e.advance(4 * time.Minute)
	v0 := e.draft(id).Version
	r = e.revise(e.sec, id, p.RequestID, "Sample first part, shorter.")
	if r.status != http.StatusOK || !strings.Contains(r.body, `"result":"revised"`) {
		t.Fatalf("revise: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if d.Status != store.StatusPending || len(d.Parts) != 1 || d.Parts[0].Text != "Sample first part, shorter." || d.Version <= v0 {
		t.Fatalf("after revise: %s %d parts v%d", d.Status, len(d.Parts), d.Version)
	}
	q = e.c.get("/queue").body
	mustHaveRW(t, q, `revised by sample-agent`, `revised 3:04 PM`, `Previous version`, `from sample-agent, 3:00 PM`, `<ins>`, `<del>`, `Your note to sample-agent`, `Approve</button>`)
	mustLackRW(t, q, `st rewriting`, `Cancel request`)
	revs, _ := e.st.Revisions(context.Background(), id)
	if len(revs) != 1 || len(revs[0].Parts) != 2 || revs[0].Parts[1].Text != "Sample second part." {
		t.Fatalf("revisions: %+v", revs)
	}
	// The same request can't be answered twice.
	if r := e.revise(e.sec, id, p.RequestID, "Again"); r.status != http.StatusConflict {
		t.Fatalf("second answer: %d %s", r.status, r.body)
	}
	// A second round keeps the full history; the card shows the latest previous version.
	if r := e.rewrite(id, ""); r.status != http.StatusSeeOther {
		t.Fatalf("round 2: %d %s", r.status, r.body)
	}
	if r := e.revise(e.sec, id, e.lastRequestID(id), "Round two sample text."); r.status != http.StatusOK {
		t.Fatalf("round 2 revise: %d %s", r.status, r.body)
	}
	revs, _ = e.st.Revisions(context.Background(), id)
	if len(revs) != 2 || revs[0].Parts[0].Text != "Sample first part, shorter." {
		t.Fatalf("history: %+v", revs)
	}
	// An owner edit hides the outcome; Approve works again.
	if r := e.c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm("save", "Owner sample edit.", at))); r.status != http.StatusSeeOther {
		t.Fatalf("owner edit after revise: %d %s", r.status, r.body)
	}
	mustLackRW(t, e.c.get("/queue").body, "revised by", "Previous version")
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve after revise: %d %s", r.status, r.body)
	}
	// No X traffic and no spend: rewrites never touch the post log.
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM post_log`).Scan(&n)
	if n != 0 {
		t.Fatalf("post_log has %d rows", n)
	}
}

func TestRewriteUnchanged(t *testing.T) {
	e := newRewriteEnv(t)
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text stays.")
	e.rewrite(id, "")
	v := e.draft(id).Version
	r := e.revise(e.sec, id, e.lastRequestID(id), "Sample text stays.")
	if r.status != http.StatusOK || !strings.Contains(r.body, `"result":"unchanged"`) {
		t.Fatalf("revise: %d %s", r.status, r.body)
	}
	if d := e.draft(id); d.Version != v || d.Status != store.StatusPending {
		t.Fatalf("unchanged moved the draft: v%d %s", d.Version, d.Status)
	}
	q := e.c.get("/queue").body
	mustHaveRW(t, q, "No changes from sample-agent.")
	mustLackRW(t, q, "revised by", "Previous version", "st rewriting")
	revs, _ := e.st.Revisions(context.Background(), id)
	if len(revs) != 0 {
		t.Fatalf("%d revisions for unchanged text", len(revs))
	}
}

// Agents revise only their own drafts, and only with drafts:revise.
func TestRewriteScopeIsolation(t *testing.T) {
	e := newRewriteEnv(t)
	_, other := e.token("sample-agent-2", store.ScopeDraftsWrite, store.ScopeDraftsRevise)
	_, noRevise := e.token("sample-agent-3")
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text one.")
	e.rewrite(id, "")
	req := e.lastRequestID(id)
	if r := e.revise(other, id, req, "Hijack"); r.status != http.StatusNotFound {
		t.Fatalf("other agent: %d %s", r.status, r.body)
	}
	if r := e.revise(noRevise, id, req, "Hijack"); r.status != http.StatusForbidden {
		t.Fatalf("no scope: %d %s", r.status, r.body)
	}
	od, _, err := e.st.CreateOwnerDraft(context.Background(), e.owner, store.OwnerDraft{Account: sm, Parts: []store.PartIn{{Text: "Owner sample."}}, When: testNow.Add(3 * time.Hour), Key: "k1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.revise(e.sec, od.ID, req, "Hijack"); r.status != http.StatusNotFound {
		t.Fatalf("owner draft: %d %s", r.status, r.body)
	}
	if r := e.rewrite(od.ID, ""); r.status == http.StatusSeeOther {
		t.Fatal("owner draft sent for rewrite")
	}
	// A request id from another draft does not answer this one.
	id2 := e.propose(e.sec, sm, testNow.Add(2*time.Hour), "Sample text two.")
	if r := e.revise(e.sec, id2, req, "Wrong request"); r.status == http.StatusOK {
		t.Fatalf("cross-draft request id accepted: %s", r.body)
	}
	if e.draft(id).Parts[0].Text != "Sample text one." || e.draft(od.ID).Parts[0].Text != "Owner sample." {
		t.Fatal("text changed")
	}
	// Owner posts get no Rewrite button; agents still can't approve.
	q := e.c.get("/queue").body
	// id2 has a live Rewrite link, id (rewriting) a disabled one, the owner draft none.
	if a, sp := strings.Count(q, `<a class="btn rwb`), strings.Count(q, `<span class="btn rwb dis"`); a != 1 || sp != 1 {
		t.Fatalf("rewrite buttons: %d links, %d disabled", a, sp)
	}
	if r := e.api("POST", "/api/v1"+path(id2, "/approve"), e.sec, map[string]any{}); r.status == http.StatusOK {
		t.Fatal("agent approved")
	}
}

func TestRewriteTimeoutAndLateReply(t *testing.T) {
	e := newRewriteEnv(t)
	id := e.propose(e.sec, sm, testNow.Add(3*time.Hour), "Sample text.")
	e.rewrite(id, "Sample note")
	req := e.lastRequestID(id)
	e.advance(59 * time.Minute)
	mustHaveRW(t, e.c.get("/queue").body, "st rewriting")
	e.advance(2 * time.Minute)
	q := e.c.get("/queue").body
	mustHaveRW(t, q, "no rewrite yet", "has not replied since 3:00 PM (61 min)", `name="retry" value="1"`, "Edit it yourself")
	mustLackRW(t, q, "st rewriting")
	r := e.revise(e.sec, id, req, "Too late")
	if r.status != http.StatusConflict || !strings.Contains(r.body, "expired") {
		t.Fatalf("late reply: %d %s", r.status, r.body)
	}
	if e.draft(id).Parts[0].Text != "Sample text." {
		t.Fatal("late reply changed the text")
	}
	evs, _ := e.st.Events(context.Background(), id)
	found := false
	for _, ev := range evs {
		found = found || strings.Contains(ev.Kind+" "+ev.Detail, "late rewrite reply refused")
		if strings.Contains(ev.Detail, "Too late") {
			t.Fatal("event logs the reply text")
		}
	}
	if !found {
		t.Fatalf("no late-reply event: %+v", evs)
	}
	// Retry re-sends the same note; the old request stays refused.
	r = e.c.post("/queue", path(id, "/rewrite"), e.ownerForm(id, url.Values{"retry": {"1"}, "return": {"/queue"}}))
	if r.status != http.StatusSeeOther {
		t.Fatalf("retry: %d %s", r.status, r.body)
	}
	hits := e.hook.hits()
	if len(hits) != 2 || !strings.Contains(string(hits[1].body), `"note":"Sample note"`) {
		t.Fatalf("retry payload: %d", len(hits))
	}
	if r := e.revise(e.sec, id, req, "Old request"); r.status != http.StatusConflict {
		t.Fatalf("old request after retry: %d", r.status)
	}
	// Approve after a timeout works.
	e.advance(61 * time.Minute)
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve after timeout: %d %s", r.status, r.body)
	}
}

func TestRewriteCancelAndReject(t *testing.T) {
	e := newRewriteEnv(t)
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text A.")
	e.rewrite(id, "")
	req := e.lastRequestID(id)
	r := e.c.post("/queue", path(id, "/rewrite/cancel"), url.Values{"return": {"/queue"}})
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=rwcancel") {
		t.Fatalf("cancel: %d %s", r.status, r.loc)
	}
	if r := e.revise(e.sec, id, req, "After cancel"); r.status != http.StatusConflict || !strings.Contains(r.body, "cancel") {
		t.Fatalf("reply after cancel: %d %s", r.status, r.body)
	}
	mustLackRW(t, e.c.get("/queue").body, "st rewriting")

	id2 := e.propose(e.sec, sm, testNow.Add(2*time.Hour), "Sample text B.")
	e.rewrite(id2, "")
	req2 := e.lastRequestID(id2)
	if r := e.c.post("/queue", path(id2, "/reject"), e.ownerForm(id2, url.Values{"reason": {"Sample reason"}, "return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("reject while rewriting: %d %s", r.status, r.body)
	}
	if q, _ := e.st.LatestRewrite(context.Background(), id2); q.State != store.RewriteCancelled {
		t.Fatalf("reject left the request %s", q.State)
	}
	if r := e.revise(e.sec, id2, req2, "After reject"); r.status != http.StatusConflict {
		t.Fatalf("reply after reject: %d %s", r.status, r.body)
	}
	if d := e.draft(id2); d.Status != store.StatusRejected || d.Parts[0].Text != "Sample text B." {
		t.Fatalf("after reject: %s %q", d.Status, d.Parts[0].Text)
	}
	// Owner routes keep CSRF.
	id3 := e.propose(e.sec, sm, testNow.Add(4*time.Hour), "Sample text C.")
	if r := e.c.postRaw(path(id3, "/rewrite"), url.Values{"v": {"1"}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("rewrite without csrf: %d", r.status)
	}
	if r := e.client().postRaw(path(id3, "/rewrite"), url.Values{"v": {"1"}}, nil); r.status == http.StatusSeeOther && !strings.HasPrefix(r.loc, "/login") || r.status == http.StatusOK {
		t.Fatalf("rewrite signed out: %d %s", r.status, r.loc)
	}
	if len(e.hook.hits()) != 2 {
		t.Fatalf("webhook hits %d", len(e.hook.hits()))
	}
}

// A rewrite can't touch images: a media field is refused, and dropping a
// part that has images is refused; images stay on their parts.
func TestRewriteImagesUnchangeable(t *testing.T) {
	e := newRewriteEnv(t)
	up := upload(e.env, e.sec, pngBytes(t))
	var out struct {
		Ref string `json:"media_ref"`
	}
	json.Unmarshal([]byte(up.body), &out)
	body := map[string]any{"account": sm, "proposed_at": testNow.Add(time.Hour).UTC().Format(time.RFC3339),
		"parts": []map[string]any{{"text": "Sample part one."}, {"text": "Sample part with image.", "media": []any{map[string]string{"ref": out.Ref, "alt": "Sample alt"}}}}}
	r := e.api("POST", "/api/v1/drafts", e.sec, body)
	if r.status != http.StatusCreated {
		t.Fatalf("propose: %d %s", r.status, r.body)
	}
	id := decode(t, r).ID
	e.rewrite(id, "")
	var p struct {
		Parts []struct{ Images int } `json:"parts"`
	}
	json.Unmarshal(e.hook.hits()[0].body, &p)
	if len(p.Parts) != 2 || p.Parts[1].Images != 1 {
		t.Fatalf("payload image counts: %+v", p)
	}
	req := e.lastRequestID(id)
	withMedia := map[string]any{"request_id": req, "parts": []map[string]any{{"text": "A"}, {"text": "B", "media": []any{}}}}
	if r := e.api("POST", "/api/v1"+path(id, "/revise"), e.sec, withMedia); r.status != http.StatusBadRequest || !strings.Contains(r.body, "can't change images") {
		t.Fatalf("media field: %d %s", r.status, r.body)
	}
	if r := e.revise(e.sec, id, req, "Only one part now."); r.status != http.StatusBadRequest {
		t.Fatalf("dropping the image part: %d %s", r.status, r.body)
	}
	if r := e.revise(e.sec, id, req, "Part one, new.", "Image part, new.", "A new third part."); r.status != http.StatusOK {
		t.Fatalf("revise keeping images: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if len(d.Parts) != 3 || len(d.Parts[1].Media) != 1 || len(d.Parts[0].Media) != 0 || len(d.Parts[2].Media) != 0 {
		t.Fatalf("images moved: %+v", d.Parts)
	}
	if r := e.api("POST", "/api/v1"+path(id, "/revise"), e.sec, `{"request_id":1,"parts":[{"text":"x"}],"account":"x"}`); r.status != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", r.status)
	}
}

// Webhook settings: the key is sealed, never rendered, never logged; a
// failed test only warns.
func TestRewriteWebhookSettings(t *testing.T) {
	e := newRewriteEnv(t)
	var sealed []byte
	var last4 string
	if err := e.st.DB.QueryRow(`SELECT key_sealed, key_last4 FROM agent_webhooks WHERE token_id = ?`, e.tok.ID).Scan(&sealed, &last4); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), hookKey) || strings.Contains(string(sealed), hookKey[4:20]) || last4 != hookKey[len(hookKey)-4:] {
		t.Fatalf("key stored in the clear (last4 %q)", last4)
	}
	page := e.c.get("/settings").body
	mustHaveRW(t, page, `id="agent-`+strconvI(e.tok.ID)+`"`, "Rewrite webhooks", "Set · not tested", "ends "+last4, `type="password"`, e.hook.srv.URL+"/hook")
	mustLackRW(t, page, hookKey, hookKey[:len(hookKey)-4])
	// Saving the URL alone keeps the key.
	if r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/hook2"}}); r.status != http.StatusSeeOther {
		t.Fatalf("save url only: %d %s", r.status, r.body)
	}
	if _, k, _ := e.st.WebhookSecret(context.Background(), e.tok.ID); k != hookKey {
		t.Fatal("key lost on url-only save")
	}
	// Test passes.
	r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"test"}})
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=hooktested") {
		t.Fatalf("test: %d %s", r.status, r.loc)
	}
	if h := e.hook.hits(); len(h) != 1 || h[0].h.Get("X-Xpost-Event") != "rewrite.test" {
		t.Fatal("test not delivered")
	}
	mustHaveRW(t, e.c.get("/settings?done=hooktested").body, "Test passed 3:00 PM", "Test sent and answered.")
	// A failed test only warns: Rewrite stays enabled.
	e.hook.setStatus(http.StatusInternalServerError)
	r = e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"test"}})
	if !strings.Contains(r.loc, "done=hookfailed") {
		t.Fatalf("failed test: %s", r.loc)
	}
	mustHaveRW(t, e.c.get("/settings").body, "Test failed", "HTTP 500")
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text.")
	q := e.c.get("/queue").body
	mustHaveRW(t, q, `class="btn rwb"`, "failed its last test (HTTP 500)")
	// A failed delivery leaves the draft pending and unblocked.
	r = e.rewrite(id, "Sample note text here")
	if r.status != http.StatusConflict || !strings.Contains(r.body, "Couldn&#39;t deliver the request to sample-agent&#39;s rewrite webhook (HTTP 500)") {
		t.Fatalf("failed delivery: %d", r.status)
	}
	mustLackRW(t, e.c.get("/queue").body, "st rewriting")
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve after failed delivery: %d", r.status)
	}
	// URL rules: https only (http loopback only for an http loopback server).
	for _, bad := range []string{"http://10.0.0.1/hook", "https://169.254.169.254/latest", "https://[fd00::1]/x", "https://[::ffff:192.168.1.1]/x", "https://100.64.0.1/x", "http://hooks.example.test/x", "ftp://hooks.example.test/", "not a url", "https://user:pw@hooks.example.test/"} {
		r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"save"}, "url": {bad}, "key": {hookKey}})
		if r.status != http.StatusBadRequest {
			t.Errorf("url %q: %d", bad, r.status)
		}
		mustLackRW(t, r.body, hookKey)
	}
	if r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"save"}, "url": {e.hook.srv.URL}, "key": {"short"}}); r.status != http.StatusBadRequest {
		t.Errorf("short key: %d", r.status)
	}
	// Remove turns Rewrite off with a Settings link.
	if r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"remove"}}); !strings.Contains(r.loc, "done=hookremoved") {
		t.Fatalf("remove: %s", r.loc)
	}
	id2 := e.propose(e.sec, sm, testNow.Add(2*time.Hour), "Sample text two.")
	mustHaveRW(t, e.c.get("/queue").body, "Set up a rewrite webhook for sample-agent in Settings", `href="/settings#agent-`+strconvI(e.tok.ID)+`"`, `btn rwb dis`)
	if r := e.rewrite(id2, ""); r.status == http.StatusSeeOther {
		t.Fatal("rewrite with no webhook")
	}
	// Nothing secret or private in the logs.
	logs := e.logs.String()
	mustLackRW(t, logs, hookKey, e.hook.srv.URL, "Sample note text here", "Sample text.")
	if !strings.Contains(logs, "rewrite webhook: token "+strconvI(e.tok.ID)) {
		t.Fatal("delivery not logged at all")
	}
}

// A token without drafts:revise gets a disabled Rewrite with a hint.
func TestRewriteNeedsReviseScope(t *testing.T) {
	h := newHookRecv(t)
	e := newEnv(t, loopbackHooks(h))
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-agent")
	c.post("/settings", "/settings/webhooks/"+strconvI(tok.ID), url.Values{"op": {"save"}, "url": {h.srv.URL}, "key": {hookKey}})
	id := e.propose(sec, sm, testNow.Add(time.Hour), "Sample text.")
	q := c.get("/queue").body
	mustHaveRW(t, q, "btn rwb dis", "lacks the drafts:revise scope")
	if r := c.post("/queue", path(id, "/rewrite"), url.Values{"v": {strconv.Itoa(e.draft(id).Version)}}); r.status == http.StatusSeeOther {
		t.Fatal("rewrite without revise scope")
	}
	if len(h.hits()) != 0 {
		t.Fatal("delivered without revise scope")
	}
	mustHaveRW(t, c.get("/settings").body, "This token lacks drafts:revise")
	// The Log page can mint a token with drafts:revise.
	mustHaveRW(t, c.get("/log").body, `name="drafts_revise"`)
}

func TestWordDiff(t *testing.T) {
	a, b := wordDiff("Sample tools ship today for all.", "Sample tools ship now for all, again.")
	join := func(ss []seg, k string) (out []string) {
		for _, s := range ss {
			if s.K == k {
				out = append(out, s.T)
			}
		}
		return
	}
	if got := strings.Join(join(a, "del"), "|"); got != "today|all." {
		t.Fatalf("del %q", got)
	}
	if got := strings.Join(join(b, "add"), "|"); got != "now|all, again." {
		t.Fatalf("add %q", got)
	}
	var re string
	for _, s := range b {
		re += s.T
	}
	if re != "Sample tools ship now for all, again." {
		t.Fatalf("segments lose text: %q", re)
	}
	if _, b := wordDiff("", "New part."); len(b) != 1 || b[0].K != "add" {
		t.Fatalf("new part: %+v", b)
	}
}
