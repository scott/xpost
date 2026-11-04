package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func pageOf(loc string) string { return strings.SplitN(loc, "#", 2)[0] }

// pending > 1, then == 1, then 0: Approve stays in Pending at the next
// card while anything is left, and lands on the approved card after the
// last one.
func TestApproveStaysInPendingUntilTheLast(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	a := e.propose(sec, sm, at(2, 9, 0), "Example post one")
	b := e.propose(sec, sm, at(2, 10, 0), "Example post two")
	d := e.propose(sec, jg, at(2, 11, 0), "Example post three")

	r := c.post("/queue", path(a, "/approve"), e.ownerForm(a, url.Values{"return": {"/queue"}}))
	if want := "/queue?done=approved#d" + strconvI(b); r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("approve 1 of 3: %d %q want %q", r.status, r.loc, want)
	}
	pg := c.get(pageOf(r.loc)).body
	mustHave(t, pg, "<span>Approved. 2 left to review.</span>", `class="on" aria-current="true">Pending `, `id="d`+strconvI(b)+`"`)

	// From the edit page (Save and approve), last card in the list: anchor
	// to the one before it.
	r = c.post(path(d, ""), path(d, ""), e.ownerForm(d, url.Values{"op": {"approve"}, "date": {"2026-06-02"}, "time": {"11:00"}, "parts": {"1"}, "text_0": {"Example post three"}}))
	if want := "/queue?done=approved#d" + strconvI(b); r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("edit approve: %d %q want %q", r.status, r.loc, want)
	}
	mustHave(t, c.get(pageOf(r.loc)).body, "<span>Approved. 1 left to review.</span>")

	// The last one: Approved, at its card.
	r = c.post("/queue", path(b, "/approve"), e.ownerForm(b, url.Values{"return": {"/queue"}}))
	if want := "/queue?status=approved&done=lastapproved#d" + strconvI(b); r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("approve last: %d %q want %q", r.status, r.loc, want)
	}
	mustHave(t, c.get(pageOf(r.loc)).body, "<span>Approved. Nothing left to review.</span>", `class="on" aria-current="true">Approved `, `id="d`+strconvI(b)+`"`)
}

// A filtered Pending view that empties falls back to all of Pending.
func TestApproveLeavesAnEmptyFilter(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	a := e.propose(sec, sm, at(2, 9, 0), "Example post one")
	b := e.propose(sec, jg, at(2, 10, 0), "Example post two")
	r := c.post("/queue?account=sample_writer", path(a, "/approve"), e.ownerForm(a, url.Values{"return": {"/queue?account=sample_writer"}}))
	if r.loc != "/queue?done=approved" {
		t.Fatalf("emptied filter: %q", r.loc)
	}
	// One still left under the filter: stay there, at the next card.
	c2 := e.propose(sec, jg, at(2, 11, 0), "Example post three")
	r = c.post("/queue?account=sample_maker", path(b, "/approve"), e.ownerForm(b, url.Values{"return": {"/queue?account=sample_maker&done=approved"}}))
	if want := "/queue?account=sample_maker&done=approved#d" + strconvI(c2); r.loc != want {
		t.Fatalf("filter kept: %q want %q", r.loc, want)
	}
}

// The bare /queue: pending empty with approvals today shows Approved, today
// first; nothing today shows Pending; an explicit status or filter wins.
func TestDefaultQueueView(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	old := e.propose(sec, sm, at(3, 9, 0), "Example post approved days ago")
	today := e.propose(sec, sm, at(9, 9, 0), "Example post approved today")
	for _, id := range []int64{old, today} {
		c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	}
	// Make "old" an approval from three days back, due next week.
	ago := testNow.Add(-72 * time.Hour).Unix()
	if _, err := e.st.DB.ExecContext(context.Background(), `UPDATE drafts SET approved_at = ?, scheduled_at = ? WHERE id = ?`, ago, at(8, 9, 0).Unix(), old); err != nil {
		t.Fatal(err)
	}
	q := c.get("/queue").body
	mustHave(t, q, `class="on" aria-current="true">Approved `, "Nothing left to review. Showing what is approved, today first.", `href="/queue?status=pending">Show Pending</a>`)
	if i, j := strings.Index(q, `id="d`+strconvI(today)+`"`), strings.Index(q, `id="d`+strconvI(old)+`"`); i < 0 || j < 0 || i > j {
		t.Fatalf("today first: today at %d, older at %d", i, j)
	}
	// Explicit links keep working.
	for _, p := range []string{"/queue?status=pending", "/queue?kind=x", "/queue?account=sample_writer"} {
		pg := c.get(p).body
		mustHave(t, pg, `aria-current="true">Pending `)
		mustLack(t, pg, "Showing what is approved, today first.")
	}
	mustHave(t, c.get("/queue?status=rejected").body, `aria-current="true">Rejected `)
	// The Pending chip on that view names its status.
	mustHave(t, q, `href="/queue?status=pending">Pending `)

	// Nothing approved today: Pending as before.
	if _, err := e.st.DB.ExecContext(context.Background(), `UPDATE drafts SET approved_at = ?, scheduled_at = ?`, ago, at(8, 9, 0).Unix()); err != nil {
		t.Fatal(err)
	}
	q = c.get("/queue").body
	mustHave(t, q, `aria-current="true">Pending `)
	mustLack(t, q, "Showing what is approved, today first.")

	// Something pending: Pending, even with approvals today.
	e.propose(sec, jg, at(9, 10, 0), "Example post waiting")
	if _, err := e.st.DB.ExecContext(context.Background(), `UPDATE drafts SET approved_at = ? WHERE status = 'approved'`, testNow.Unix()); err != nil {
		t.Fatal(err)
	}
	mustHave(t, c.get("/queue").body, `aria-current="true">Pending `, "Example post waiting")
}

// A reply approved while others wait: back to Pending with a flash that
// carries Open on X (a POST, so opened_at is recorded; the queue's CSP
// allows the X origin) and a View link to the approved card.
func TestReplyApproveWithOthersPending(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	text := "Example reply: small releases."
	id := e.proposeReply(sec, replyBody(sm, text, nil)).ID
	r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	if want := "/queue?done=replyleft&reply=" + strconvI(id) + "#d" + strconvI(xid); r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("approve reply: %d %q want %q", r.status, r.loc, want)
	}
	pg := c.get(pageOf(r.loc))
	mustHave(t, pg.body, `class="hint ok flash rflash"`, "Approved. Open on X to post it. 1 left to review.",
		`<form method="post" action="/drafts/`+strconvI(id)+`/open-x" target="_blank"`, `name="v" value="`,
		`href="/queue?status=approved&amp;kind=reply#d`+strconvI(id)+`">View</a>`, `aria-current="true">Pending `)
	if csp := pg.h.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://x.com;") {
		t.Fatalf("pending queue CSP %q", csp)
	}
	r = c.post(pageOf(r.loc), path(id, "/open-x"), e.ownerForm(id, url.Values{"return": {pageOf(r.loc)}}))
	want := "https://x.com/intent/post?text=" + strings.ReplaceAll(url.QueryEscape(text), "+", "%20") + "&in_reply_to=" + replyOrigID
	if r.status != http.StatusSeeOther || r.loc != want {
		t.Fatalf("open-x from the flash: %d %q", r.status, r.loc)
	}
	if d := e.draft(id); d.Reply == nil || d.Reply.OpenedAt == nil {
		t.Fatal("opened_at not recorded")
	}
	// A stale or wrong reply id shows no flash buttons.
	for _, bad := range []string{"999", strconvI(xid), "x"} {
		mustLack(t, c.get("/queue?done=replyleft&reply="+bad).body, "rflash")
	}
}

// Reject and send back already return to the page they came from, so
// they stay in Pending while anything is left.
func TestRejectAndSendBackStayInPending(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	a := e.propose(sec, sm, at(2, 9, 0), "Example post one")
	e.propose(sec, sm, at(2, 10, 0), "Example post two")
	r := c.post("/queue", path(a, "/reject"), e.ownerForm(a, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusSeeOther || r.loc != "/queue?done=rejected" {
		t.Fatalf("reject: %d %q", r.status, r.loc)
	}
	mustHave(t, c.get(r.loc).body, `aria-current="true">Pending `, "Example post two", "<span>Rejected.</span>")

	rw := newRewriteEnv(t)
	id := rw.propose(rw.sec, sm, testNow.Add(2*time.Hour), "Sample first part.")
	rw.propose(rw.sec, sm, testNow.Add(3*time.Hour), "Sample other draft.")
	r = rw.rewrite(id, "Example note")
	if r.status != http.StatusSeeOther || r.loc != "/queue?done=rewrite" {
		t.Fatalf("send back: %d %q", r.status, r.loc)
	}
	mustHave(t, rw.c.get(r.loc).body, `aria-current="true">Pending `, "Sample other draft.")
}
