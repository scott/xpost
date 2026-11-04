package web

import (
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

const cspSelfOnly = "form-action 'self';"

// Open on X POSTs to /drafts/{id}/open-x, which 303s to the x.com intent
// URL. Browsers apply form-action to that redirect, so the pages with that
// form allow the X web origin, and no other page does.
func TestReplyOpenOnXFormAction(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	text := "Example reply: small releases, every day."
	id := e.proposeReply(sec, replyBody(sm, text, nil)).ID
	allowX := "form-action 'self' https://x.com;"
	for _, p := range []string{"/queue", "/queue?kind=reply", "/queue?status=approved&kind=reply", "/drafts/" + strconvI(id)} {
		if csp := c.get(p).h.Get("Content-Security-Policy"); !strings.Contains(csp, allowX) {
			t.Errorf("%s CSP %q lacks %q", p, csp, allowX)
		}
	}
	checked := 0
	for _, p := range []string{"/drafts/" + strconvI(xid), "/schedule", "/log", "/settings", "/compose"} {
		r := c.get(p)
		if r.status != http.StatusOK {
			t.Errorf("%s: %d", p, r.status)
			continue
		}
		checked++
		if csp := r.h.Get("Content-Security-Policy"); !strings.Contains(csp, cspSelfOnly) || strings.Contains(csp, "x.com") {
			t.Errorf("%s CSP %q: want only form-action 'self'", p, csp)
		}
	}
	if checked == 0 {
		t.Fatal("no other page checked")
	}
	// Everything else in the policy is unchanged on the queue.
	if csp := c.get("/queue").h.Get("Content-Security-Policy"); strings.Replace(csp, " https://x.com", "", 1) != CSP {
		t.Errorf("queue CSP changed more than form-action: %q", csp)
	}

	c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	r := c.post("/queue?status=approved&kind=reply", path(id, "/open-x"), e.ownerForm(id, url.Values{"return": {"/queue?status=approved&kind=reply"}}))
	want := "https://x.com/intent/post?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20") + "&in_reply_to=" + replyOrigID
	if r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("open-x: %d %q want %q", r.status, r.loc, want)
	}
	if d := e.draft(id); d.Reply == nil || d.Reply.OpenedAt == nil {
		t.Fatal("open-x did not record opened_at")
	}
}

// Approving the last pending item, a reply, lands on Approved under
// Replies at that card, with the next step named and its buttons right
// there. (With others pending it stays in Pending: reviewflow_test.go.)
func TestReplyApproveLandsOnTheNextStep(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	text := "Example reply: I re-read the diff once."
	id := e.proposeReply(sec, replyBody(sm, text, nil)).ID

	// X posts with others pending stay in the filtered Pending view.
	r := c.post("/queue", path(xid, "/approve"), e.ownerForm(xid, url.Values{"return": {"/queue?kind=x"}}))
	if r.status != http.StatusSeeOther || r.loc != "/queue?done=approved" {
		t.Fatalf("approve post: %d %q", r.status, r.loc)
	}
	r = c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	want := "/queue?status=approved&kind=reply&done=replyapproved#d" + strconvI(id)
	if r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("approve reply: %d %q want %q", r.status, r.loc, want)
	}
	page := c.get(strings.SplitN(want, "#", 2)[0]).body
	mustHave(t, page, "Approved. Open on X to post it.", `id="d`+strconvI(id)+`"`, `class="on" aria-current="true">Approved `, `class="on" aria-current="true">Replies<`)
	card := replyCardHTML(t, page, id)
	mustHave(t, card, `action="/drafts/`+strconvI(id)+`/open-x" target="_blank"`, "Open on X to post", "Copy text",
		`id="copy`+strconvI(id)+`"`, `data-copy="copy`+strconvI(id)+`"`, text)

}

// Copy text is the static data-copy handler: no inline script or handler
// attribute on the page, and the script copies the hidden textarea.
func TestReplyCopyTextUnderCSP(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.proposeReply(sec, replyBody(sm, "Example reply: copy me.", nil)).ID
	c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	page := c.get("/queue?status=approved&kind=reply").body
	mustLack(t, page, "<script>", "onclick=", "javascript:", ` style="`)
	mustHave(t, page, `<script src="/static/xpost.js`)
	js, err := os.ReadFile("static/xpost.js")
	if err != nil {
		t.Fatal(err)
	}
	mustHave(t, string(js), "closest('[data-copy]')", "navigator.clipboard")
}
