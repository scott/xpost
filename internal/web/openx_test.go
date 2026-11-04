package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type openXOut struct {
	OK        bool   `json:"ok"`
	ID        int64  `json:"id"`
	Version   int    `json:"version"`
	OpenedAt  string `json:"opened_at"`
	IntentURL string `json:"intent_url"`
	Error     string `json:"error"`
}

var openXHdr = map[string]string{"X-Xpost-Open": "1"}

// xpost.js records Open on X with fetch and gets JSON back (issue #60).
func TestOpenXJSON(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	text := "Example reply: ship it."
	id := e.proposeReply(sec, replyBody(sm, text, nil)).ID
	if r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve: %d", r.status)
	}
	q := c.get("/queue?status=approved&kind=reply")
	want := "https://x.com/intent/post?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20") + "&in_reply_to=" + replyOrigID
	mustHave(t, q.body, `target="_blank" data-openx="`+strings.ReplaceAll(want, "&", "&amp;")+`"`, `src="/static/xpost.js`)
	csp := q.h.Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://x.com;") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP changed: %q", csp)
	}

	var seq0 livePollOut
	_ = json.Unmarshal([]byte(c.get("/live/poll?since=0").body), &seq0)

	// No CSRF token: refused.
	if r := c.postRaw(path(id, "/open-x"), e.ownerForm(id, nil), openXHdr); r.status != http.StatusForbidden {
		t.Fatalf("no csrf: %d %s", r.status, r.body)
	}
	if e.draft(id).Reply.OpenedAt != nil {
		t.Fatal("opened without csrf")
	}
	tok := c.csrf("/queue")
	post := func(pid int64, f url.Values) (resp, openXOut) {
		t.Helper()
		f.Set("csrf", tok)
		r := c.postRaw(path(pid, "/open-x"), f, openXHdr)
		var o openXOut
		if err := json.Unmarshal([]byte(r.body), &o); err != nil {
			t.Fatalf("not JSON: %d %q", r.status, r.body)
		}
		return r, o
	}
	// Not a reply, an unknown id: 404 JSON.
	for _, bad := range []int64{xid, 9999} {
		if r, o := post(bad, url.Values{"v": {"1"}}); r.status != http.StatusNotFound || o.OK {
			t.Fatalf("id %d: %d %+v", bad, r.status, o)
		}
	}
	if r := c.postRaw("/drafts/x/open-x", url.Values{"csrf": {tok}}, openXHdr); r.status != http.StatusNotFound {
		t.Fatalf("bad id: %d", r.status)
	}
	// Stale version: a conflict, nothing recorded.
	if r, o := post(id, url.Values{"v": {"1"}}); r.status != http.StatusConflict || o.OK || o.Error == "" {
		t.Fatalf("stale: %d %+v", r.status, o)
	}
	v := e.draft(id).Version
	r, o := post(id, e.ownerForm(id, nil))
	if r.status != http.StatusOK || !o.OK || o.ID != id || o.Version != v+1 || o.IntentURL != want {
		t.Fatalf("open: %d %+v", r.status, o)
	}
	d := e.draft(id)
	if d.Reply.OpenedAt == nil || d.Reply.OpenedAt.UTC().Format(time.RFC3339) != o.OpenedAt {
		t.Fatalf("opened_at %v vs %q", d.Reply.OpenedAt, o.OpenedAt)
	}
	if !strings.HasPrefix(r.h.Get("Content-Type"), "application/json") {
		t.Fatalf("content type %q", r.h.Get("Content-Type"))
	}
	// Other tabs hear about it over /live (SSE and poll share live_changes).
	var lp livePollOut
	_ = json.Unmarshal([]byte(c.get("/live/poll?since="+strconvI(seq0.Seq)).body), &lp)
	if n := len(lp.Changes); n == 0 || lp.Changes[n-1].ID != id || lp.Changes[n-1].V != int64(v+1) {
		t.Fatalf("live changes %+v", lp.Changes)
	}
	// The card re-renders opened, with Mark posted.
	mustHave(t, replyCardHTML(t, c.get("/queue?status=approved&kind=reply").body, id), "Opened on X at", "Mark posted", "Open on X again")

	// No JS: the same form still POSTs and redirects to X.
	r = c.post("/queue", path(id, "/open-x"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("no-JS fallback: %d %q", r.status, r.loc)
	}
}

// The approve flash switches to its opened variant once Open on X ran.
func TestReplyFlashOpened(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	id := e.proposeReply(sec, replyBody(sm, "Example reply", nil)).ID
	r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	pg := pageOf(r.loc)
	mustHave(t, c.get(pg).body, `id="rflash"`, `data-openx="https://x.com/intent/post?`)
	f := e.ownerForm(id, nil)
	f.Set("csrf", c.csrf(pg))
	if r := c.postRaw(path(id, "/open-x"), f, openXHdr); r.status != http.StatusOK {
		t.Fatalf("open: %d %s", r.status, r.body)
	}
	b := c.get(pg).body
	mustHave(t, b, `id="rflash"`, "Opened on X at", "Pressed Post on X? Mark it posted here.",
		`href="/queue?status=approved&amp;kind=reply&amp;mark=`+strconvI(id)+`#d`+strconvI(id)+`">`)
	mustLack(t, b, "Approved. Open on X to post it.")
}

// The JS is served and carries the handler; live.js no longer treats a
// new-tab submit as leaving the page.
func TestOpenXScripts(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	js := c.get("/static/xpost.js")
	mustHave(t, js.body, "form[data-openx]", "window.open(url, '_blank')", "w.opener = null", "form.submit()", "'X-Xpost-Open': '1'")
	mustHave(t, c.get("/static/live.js").body, "getAttribute('target') === '_blank'")
}
