package web

// These tests prove the hard rule: an agent token can never approve, edit
// an approved item, or post. Each limit is checked on the server, through
// the API and through direct POSTs to the owner's routes.

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/store"
)

const sm, jg = "sample_writer", "sample_maker"

// An agent cannot approve by POSTing the owner's approve form with its
// bearer token, with or without a valid CSRF pair.
func TestAgentCannotApproveViaWebRoute(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")

	anon := e.client()
	csrf := anon.csrf("/login") // a valid CSRF cookie and token, but no session
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer " + sec},
		{"Authorization": "Bearer " + sec, "Cookie": SessionCookie + "=" + sec},
	} {
		for _, p := range []string{"/approve", "", "/reschedule"} {
			f := e.ownerForm(id, url.Values{"csrf": {csrf}, "op": {"approve"}, "date": {"2026-06-01"}, "time": {"18:00"}, "parts": {"1"}, "text_0": {"Example edit"}})
			r := anon.postRaw(path(id, p), f, hdr)
			if r.status != http.StatusUnauthorized && r.status != http.StatusForbidden {
				t.Fatalf("POST %s with bearer: status %d, want 401/403", p, r.status)
			}
		}
	}
	if d := e.draft(id); d.Status != store.StatusPending || d.Parts[0].Text != "Example post" {
		t.Fatalf("draft changed: %s %q", d.Status, d.Parts[0].Text)
	}
}

// The API has no approve, edit or post route; every verb on every likely
// path is refused and nothing changes or posts.
func TestAgentAPIRouteSweepChangesNothing(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	pend := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example pending")
	appr := e.propose(sec, jg, testNow.Add(3*time.Hour), "Example approved")
	e.approveNow(appr)
	before := map[int64]*store.Draft{pend: e.draft(pend), appr: e.draft(appr)}

	fake := &poster.Fake{}
	paths := []string{}
	for _, id := range []int64{pend, appr} {
		b := "/api/v1/drafts/" + itoa(id)
		paths = append(paths, b, b+"/approve", b+"/approval", b+"/post", b+"/publish", b+"/schedule", b+"/reschedule", b+"/edit", b+"/parts", b+"/status")
	}
	paths = append(paths, "/api/v1/approve", "/api/v1/post", "/api/v1/posts", "/api/v1/dispatch", "/api/v1/run", "/api/v1/tokens", "/api/v1/accounts", "/api/v2/drafts")
	body := `{"status":"approved","approved":true,"parts":[{"text":"Example takeover"}]}`
	for _, m := range []string{"POST", "PUT", "PATCH"} {
		for _, p := range paths {
			r := e.api(m, p, sec, body)
			if r.status < 400 {
				t.Errorf("%s %s: status %d, want an error", m, p, r.status)
			}
		}
	}
	for _, id := range []int64{pend, appr} {
		d := e.draft(id)
		if d.Status != before[id].Status || d.Version != before[id].Version || d.Parts[0].Text != before[id].Parts[0].Text {
			t.Errorf("draft %d changed: %s v%d %q", id, d.Status, d.Version, d.Parts[0].Text)
		}
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("poster called %d times", n)
	}
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM post_log`).Scan(&n)
	if n != 0 {
		t.Fatalf("post_log has %d rows after the sweep", n)
	}
}

// PUT and PATCH on a draft are 405: there is no agent edit.
func TestAgentCannotEditViaPutOrPatch(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post")
	e.approveNow(id)
	for _, m := range []string{"PUT", "PATCH"} {
		r := e.api(m, "/api/v1/drafts/"+itoa(id), sec, `{"parts":[{"text":"Example edit"}]}`)
		if r.status != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, r.status)
		}
	}
	if d := e.draft(id); d.Status != store.StatusApproved || d.Parts[0].Text != "Example post" {
		t.Fatalf("approved draft changed: %s %q", d.Status, d.Parts[0].Text)
	}
}

// Replaying an idempotency key with new content cannot rewrite an approved
// draft: the stored draft comes back untouched.
func TestAgentIdempotentReplayCannotEditApproved(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	b := draftBody(sm, testNow.Add(2*time.Hour), "Example original")
	b["idempotency_key"] = "example-key-1"
	r := e.api("POST", "/api/v1/drafts", sec, b)
	if r.status != http.StatusCreated {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	id := decode(t, r).ID
	e.approveNow(id)
	v := e.draft(id).Version

	b2 := draftBody(sm, testNow.Add(5*time.Hour), "Example rewrite")
	b2["idempotency_key"] = "example-key-1"
	r = e.api("POST", "/api/v1/drafts", sec, b2)
	if r.status != http.StatusOK || decode(t, r).ID != id {
		t.Fatalf("replay: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if d.Status != store.StatusApproved || d.Version != v || d.Parts[0].Text != "Example original" || !d.ScheduledAt.Equal(testNow.Add(2*time.Hour)) {
		t.Fatalf("replay changed the approved draft: %s v%d %q %v", d.Status, d.Version, d.Parts[0].Text, d.ScheduledAt)
	}
}

// DELETE works only on the agent's own pending drafts: an approved one is
// 409, another token's is 404, and both survive.
func TestAgentDeleteOnlyOwnPending(t *testing.T) {
	e := newEnv(t, nil)
	_, a := e.token("sample-agent-1")
	_, b := e.token("sample-agent-2")
	mine := e.propose(a, sm, testNow.Add(2*time.Hour), "Example mine")
	appr := e.propose(a, sm, testNow.Add(4*time.Hour), "Example approved")
	e.approveNow(appr)
	theirs := e.propose(b, jg, testNow.Add(2*time.Hour), "Example theirs")

	if r := e.api("DELETE", "/api/v1/drafts/"+itoa(appr), a, nil); r.status != http.StatusConflict {
		t.Errorf("delete approved: %d, want 409", r.status)
	}
	if r := e.api("DELETE", "/api/v1/drafts/"+itoa(theirs), a, nil); r.status != http.StatusNotFound {
		t.Errorf("delete other token's draft: %d, want 404", r.status)
	}
	if r := e.api("GET", "/api/v1/drafts/"+itoa(theirs), a, nil); r.status != http.StatusNotFound {
		t.Errorf("read other token's draft: %d, want 404", r.status)
	}
	if e.draft(appr).Status != store.StatusApproved || e.draft(theirs).Status != store.StatusPending {
		t.Fatal("refused deletes changed a draft")
	}
	if r := e.api("DELETE", "/api/v1/drafts/"+itoa(mine), a, nil); r.status != http.StatusNoContent && r.status != http.StatusOK {
		t.Fatalf("delete own pending: %d %s", r.status, r.body)
	}
	if _, err := e.st.Draft(context.Background(), mine); err == nil {
		t.Fatal("own pending draft still there")
	}
}

// A create body cannot set a status, approval or schedule: unknown fields
// are refused, and a plain create is always pending.
func TestAgentCannotCreateApproved(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	for _, extra := range []string{`"status":"approved"`, `"approved":true`, `"scheduled_at":"2026-06-01T18:00:00Z"`, `"version":9`} {
		body := `{"account":"sample_writer","proposed_at":"2026-06-01T18:00:00Z","parts":[{"text":"Example"}],` + extra + `}`
		if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest {
			t.Errorf("create with %s: %d, want 400", extra, r.status)
		}
	}
	var n int
	e.st.DB.QueryRow(`SELECT COUNT(*) FROM drafts`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d drafts created by refused bodies", n)
	}
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example")
	if d := e.draft(id); d.Status != store.StatusPending {
		t.Fatalf("new draft is %s scheduled %v", d.Status, d.ScheduledAt)
	}
}

// Only the dispatcher posts, and only approved, due drafts. Agent traffic
// never reaches the poster; a pending draft past its time is never posted.
func TestAgentCannotPost(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	pend := e.propose(sec, sm, testNow.Add(20*time.Minute), "Example pending")
	appr := e.propose(sec, jg, testNow.Add(20*time.Minute), "Example approved")
	e.approveNow(appr)
	fake := &poster.Fake{}
	disp := &poster.Dispatcher{Store: e.st, Poster: fake}
	e.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	e.st.SetSetting(context.Background(), store.SettingCapCents, "2000")

	e.advance(time.Hour) // both are now due
	for _, p := range []string{"/api/v1/drafts/" + itoa(pend), "/api/v1/drafts/" + itoa(pend) + "/post", "/api/v1/post"} {
		e.api("POST", p, sec, `{}`)
	}
	if len(fake.Calls()) != 0 {
		t.Fatal("agent traffic reached the poster")
	}
	if _, err := disp.RunDue(context.Background(), e.clock()); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Text != "Example approved" {
		t.Fatalf("poster calls: %+v", calls)
	}
	if d := e.draft(pend); d.Status != store.StatusPending {
		t.Fatalf("pending draft became %s", d.Status)
	}
	if !strings.Contains(fake.Calls()[0].IdempotencyKey, "d"+itoa(appr)+"-") {
		t.Fatalf("idempotency key %q", fake.Calls()[0].IdempotencyKey)
	}
}

// A password-only session (passkey not yet confirmed) cannot approve.
func TestPasswordStageCannotApprove(t *testing.T) {
	e := newEnv(t, nil)
	_, k := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example")
	c := e.passwordStep()
	csrf := c.csrf("/login/passkey")
	if r := c.postRaw(path(id, "/approve"), e.ownerForm(id, url.Values{"csrf": {csrf}}), nil); r.status != http.StatusUnauthorized {
		t.Fatalf("password-stage approve: %d, want 401", r.status)
	}
	if g := c.get("/queue"); g.status != http.StatusSeeOther {
		t.Fatalf("password-stage queue: %d, want redirect", g.status)
	}
	if e.draft(id).Status != store.StatusPending {
		t.Fatal("approved by a password-only session")
	}
	_ = k
}

// The owner's approve needs CSRF; without it the request is 403.
func TestOwnerApproveNeedsCSRF(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example")
	if r := c.postRaw(path(id, "/approve"), e.ownerForm(id, nil), nil); r.status != http.StatusForbidden {
		t.Fatalf("approve without csrf: %d, want 403", r.status)
	}
	if r := c.postRaw(path(id, "/approve"), e.ownerForm(id, url.Values{"csrf": {"forged"}}), nil); r.status != http.StatusForbidden {
		t.Fatalf("approve with forged csrf: %d, want 403", r.status)
	}
	csrf := c.csrf("/queue")
	if r := c.postRaw(path(id, "/approve"), e.ownerForm(id, url.Values{"csrf": {csrf}}), map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site approve: %d, want 403", r.status)
	}
	if e.draft(id).Status != store.StatusPending {
		t.Fatal("approved without CSRF")
	}
	if r := c.postRaw(path(id, "/approve"), e.ownerForm(id, url.Values{"csrf": {csrf}}), nil); r.status != http.StatusSeeOther {
		t.Fatalf("owner approve: %d %s", r.status, r.body)
	}
	if e.draft(id).Status != store.StatusApproved {
		t.Fatal("owner approve did not approve")
	}
}

// Tokens are scoped, and a revoked token is refused everywhere.
func TestAgentScopesAndRevoke(t *testing.T) {
	e := newEnv(t, nil)
	_, ro := e.token("example-readonly", store.ScopeLogRead)
	_, wo := e.token("sample-agent-2", store.ScopeDraftsWrite)
	tok, full := e.token("sample-agent-1")
	at := testNow.Add(2 * time.Hour)
	if r := e.api("POST", "/api/v1/drafts", ro, draftBody(sm, at, "Example")); r.status != http.StatusForbidden {
		t.Errorf("log:read create: %d, want 403", r.status)
	}
	if r := e.api("GET", "/api/v1/log", wo, nil); r.status != http.StatusForbidden {
		t.Errorf("drafts:write log: %d, want 403", r.status)
	}
	if r := e.api("GET", "/api/v1/log", ro, nil); r.status != http.StatusOK {
		t.Errorf("log:read log: %d", r.status)
	}
	if r := e.api("GET", "/api/v1/drafts", "", nil); r.status != http.StatusUnauthorized {
		t.Errorf("no token: %d", r.status)
	}
	if r := e.api("GET", "/api/v1/drafts", "xpt_notarealtoken", nil); r.status != http.StatusUnauthorized {
		t.Errorf("bad token: %d", r.status)
	}
	if err := e.st.RevokeAgentToken(context.Background(), e.owner, tok.ID); err != nil {
		t.Fatal(err)
	}
	if r := e.api("POST", "/api/v1/drafts", full, draftBody(sm, at, "Example")); r.status != http.StatusUnauthorized {
		t.Errorf("revoked create: %d, want 401", r.status)
	}
	if strings.Contains(e.logs.String(), full) || strings.Contains(e.logs.String(), ro) {
		t.Fatal("a token secret reached the logs")
	}
}

func itoa(n int64) string { return strconvI(n) }
