package web

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// editForm is the edit page's form for a one-part draft at a UTC time.
func editForm(op, text string, at time.Time) url.Values {
	return url.Values{"op": {op}, "parts": {"1"}, "text_0": {text}, "media_0": {"0"},
		"date": {at.UTC().Format("2006-01-02")}, "time": {at.UTC().Format("15:04")}}
}

func TestOwnerEditOfApprovedReturnsToPending(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	e.approveNow(id)
	r := c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm("save", "Example post, edited", at)))
	if r.status != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if d.Status != store.StatusPending || d.Parts[0].Text != "Example post, edited" {
		t.Fatalf("after edit: %s %q", d.Status, d.Parts[0].Text)
	}
	// The agent sees it back in pending.
	if got := decode(t, e.api("GET", "/api/v1/drafts/"+itoa(id), sec, nil)); got.Status != "pending" {
		t.Fatalf("agent sees %s", got.Status)
	}
}

func TestSaveAndApprove(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	r := c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm("approve", "Example post v2", at.Add(time.Hour))))
	if r.status != http.StatusSeeOther {
		t.Fatalf("save and approve: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	if d.Status != store.StatusApproved || d.Parts[0].Text != "Example post v2" || !d.ScheduledAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("after save and approve: %s %q %v", d.Status, d.Parts[0].Text, d.ScheduledAt)
	}
}

func TestOverLimitCannotSave(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	r := c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm("approve", strings.Repeat("例", 141), at)))
	if r.status == http.StatusSeeOther {
		t.Fatal("saved a 282-weight part")
	}
	if d := e.draft(id); d.Status != store.StatusPending || d.Parts[0].Text != "Example post" {
		t.Fatalf("over-limit edit stored: %s", d.Status)
	}
}

func TestRescheduleApprovedReturnsToPending(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	e.approveNow(id)
	f := url.Values{"op": {"move"}, "date": {"2026-06-02"}, "time": {"09:30"}}
	r := c.post(path(id, "/reschedule"), path(id, "/reschedule"), e.ownerForm(id, f))
	if r.status != http.StatusSeeOther {
		t.Fatalf("reschedule: %d %s", r.status, r.body)
	}
	d := e.draft(id)
	want := time.Date(2026, 6, 2, 9, 30, 0, 0, time.UTC)
	if d.Status != store.StatusPending || !d.ScheduledAt.Equal(want) {
		t.Fatalf("after reschedule: %s %v", d.Status, d.ScheduledAt)
	}
}

func TestRejectReasonVisibleToAgent(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	a := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example A")
	b := e.propose(sec, sm, testNow.Add(3*time.Hour), "Example B")
	c.post("/queue", path(a, "/reject"), e.ownerForm(a, url.Values{"reason": {"Example reason: tone"}}))
	c.post("/queue", path(b, "/reject"), e.ownerForm(b, nil))
	ga := decode(t, e.api("GET", "/api/v1/drafts/"+itoa(a), sec, nil))
	if ga.Status != "rejected" || ga.RejectReason == nil || *ga.RejectReason != "Example reason: tone" {
		t.Fatalf("agent sees %+v", ga)
	}
	if gb := decode(t, e.api("GET", "/api/v1/drafts/"+itoa(b), sec, nil)); gb.Status != "rejected" || (gb.RejectReason != nil && *gb.RejectReason != "") {
		t.Fatalf("no-reason reject: %+v", gb)
	}
	var list struct {
		Drafts []draftOut `json:"drafts"`
	}
	json.Unmarshal([]byte(e.api("GET", "/api/v1/drafts?status=rejected", sec, nil).body), &list)
	if len(list.Drafts) != 2 {
		t.Fatalf("rejected list: %+v", list)
	}
}

func TestApproveRefusesSpacingAndPast(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	ctx := context.Background()
	a := e.propose(sec, jg, testNow.Add(2*time.Hour), "Example A")
	e.approveNow(a)
	b := e.propose(sec, jg, testNow.Add(2*time.Hour+10*time.Minute), "Example B")
	err := e.st.Approve(ctx, e.owner, b, e.draft(b).Version)
	var ce *store.ConflictError
	if !asConflict(err, &ce) || !ce.NextFree.Equal(testNow.Add(2*time.Hour+15*time.Minute)) {
		t.Fatalf("approve within 15 min: %v", err)
	}
	other := e.propose(sec, sm, testNow.Add(2*time.Hour+5*time.Minute), "Example other account")
	if err := e.st.Approve(ctx, e.owner, other, e.draft(other).Version); err != nil {
		t.Fatalf("other account is not a conflict: %v", err)
	}
	p := e.propose(sec, sm, testNow.Add(30*time.Minute), "Example soon")
	e.advance(time.Hour)
	if err := e.st.Approve(ctx, e.owner, p, e.draft(p).Version); err == nil {
		t.Fatal("approved a time in the past")
	}
}

func TestStaleVersionRefused(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	old := e.ownerForm(id, nil)
	c.post(path(id, ""), path(id, ""), e.ownerForm(id, editForm("save", "Example newer", at)))
	if e.draft(id).Parts[0].Text != "Example newer" {
		t.Fatal("first save did not land")
	}
	r := c.post("/queue", path(id, "/approve"), old)
	if e.draft(id).Status == store.StatusApproved || r.status != http.StatusConflict {
		t.Fatalf("approved a version the owner never saw (status %d)", r.status)
	}
}

func TestLoginNeedsPasswordAndPasskey(t *testing.T) {
	e := newEnv(t, nil)
	// Before setup, a password cannot sign in or enroll.
	c := e.client()
	if r := c.post("/login", "/login", url.Values{"email": {testEmail}, "password": {testPW}}); r.status == http.StatusSeeOther {
		t.Fatal("password-only login before setup")
	}
	_, k := e.setupOwner()
	// Wrong password.
	if r := e.client().post("/login", "/login", url.Values{"email": {testEmail}, "password": {"wrong password 123"}}); r.status != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", r.status)
	}
	// A password-stage session cannot add a passkey.
	pc := e.passwordStep()
	if r := pc.postRaw("/passkeys/begin", url.Values{"csrf": {pc.csrf("/login/passkey")}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("password-stage register: %d", r.status)
	}
	// A different authenticator cannot finish the login.
	other := newSoftKey(t)
	other.userHandle = k.userHandle
	csrf := pc.csrf("/login/passkey")
	r := pc.postRaw("/login/passkey/begin", url.Values{"csrf": {csrf}}, nil)
	if r = pc.postRaw("/login/passkey/finish", url.Values{"csrf": {csrf}, "credential": {other.get(r.body)}}, nil); r.status == http.StatusOK {
		t.Fatal("unknown passkey signed in")
	}
	full := e.login(k)
	if g := full.get("/queue"); g.status != http.StatusOK {
		t.Fatalf("queue after login: %d", g.status)
	}
	// The setup link is single use.
	if g := e.client().get("/setup/" + e.setup); g.status != http.StatusNotFound {
		t.Fatalf("used setup link: %d", g.status)
	}
}

var styleAttr = regexp.MustCompile(`(?i)\sstyle=|<style|<script>`)

func TestPagesHaveCSPAndNoInlineStyleOrScript(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post https://example.com/x", "Example part two")
	for _, p := range []string{"/queue", "/queue?status=approved", "/queue?status=rejected", path(id, ""), path(id, "/reschedule"), "/schedule", "/log", "/settings/accounts", "/settings", "/security", "/nope", "/compose", "/compose?from=schedule&day=2026-06-02"} {
		r := c.get(p)
		if r.status >= 500 {
			t.Fatalf("%s: %d", p, r.status)
		}
		csp := r.h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP %q", p, csp)
		}
		if m := styleAttr.FindString(r.body); m != "" {
			t.Errorf("%s has inline %q", p, m)
		}
	}
	for _, p := range []string{"/login", "/setup/x"} {
		r := e.client().get(p)
		if m := styleAttr.FindString(r.body); m != "" {
			t.Errorf("%s has inline %q", p, m)
		}
	}
}

func TestTokenShownOnceAndHashedAtRest(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	r := c.post("/log", "/tokens", url.Values{"name": {"sample-agent"}, "drafts_write": {"1"}})
	m := regexp.MustCompile(`xpt_[A-Za-z0-9_-]{20,}`).FindString(r.body)
	if r.status != http.StatusOK || m == "" {
		t.Fatalf("create token page: %d, token shown %v", r.status, m != "")
	}
	if got := c.get("/log"); strings.Contains(got.body, m) {
		t.Fatal("token shown again")
	}
	rows, err := e.st.DB.Query(`SELECT name || '|' || token_hash || '|' || last4 || '|' || scopes FROM agent_tokens`)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for rows.Next() {
		var all string
		rows.Scan(&all)
		seen++
		if strings.Contains(all, m) || strings.Contains(all, m[4:]) {
			t.Fatal("plaintext token stored")
		}
	}
	rows.Close()
	if seen != 1 {
		t.Fatalf("%d token rows", seen)
	}
	if rr := e.api("GET", "/api/v1/drafts", m, nil); rr.status != http.StatusOK {
		t.Fatalf("new token works: %d", rr.status)
	}
	if rr := e.api("GET", "/api/v1/log", m, nil); rr.status != http.StatusForbidden {
		t.Fatalf("unrequested scope: %d", rr.status)
	}
	if strings.Contains(e.logs.String(), m) {
		t.Fatal("token in logs")
	}
}

func pngBytes(t *testing.T) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4)))
	return b.Bytes()
}

func upload(e *env, bearer string, data []byte) resp {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "example.png")
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/media", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+bearer)
	return e.do(http.DefaultClient, req)
}

func TestMediaUploadAndOwnership(t *testing.T) {
	e := newEnv(t, nil)
	_, a := e.token("sample-agent-1")
	_, b := e.token("sample-agent-2")
	r := upload(e, a, pngBytes(t))
	var out struct {
		Ref string `json:"media_ref"`
	}
	json.Unmarshal([]byte(r.body), &out)
	if r.status != http.StatusCreated || out.Ref == "" {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	if r := upload(e, a, []byte("not an image, example text")); r.status != http.StatusBadRequest {
		t.Fatalf("text upload: %d", r.status)
	}
	if r := upload(e, a, append(pngBytes(t), make([]byte, store.MaxMediaBytes)...)); r.status != http.StatusRequestEntityTooLarge && r.status != http.StatusBadRequest {
		t.Fatalf("oversize upload: %d", r.status)
	}
	body := map[string]any{"account": sm, "proposed_at": testNow.Add(2 * time.Hour).Format(time.RFC3339),
		"parts": []map[string]any{{"text": "Example with image", "media": []any{map[string]string{"ref": out.Ref, "alt": "Example alt text"}}}}}
	if r := e.api("POST", "/api/v1/drafts", b, body); r.status != http.StatusBadRequest {
		t.Fatalf("another token's media: %d %s", r.status, r.body)
	}
	if r := e.api("POST", "/api/v1/drafts", a, body); r.status != http.StatusCreated {
		t.Fatalf("own media: %d %s", r.status, r.body)
	}
}

func TestIntakeValidation(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	bad := []map[string]any{
		draftBody(sm, at, strings.Repeat("a", 281)),
		draftBody(sm, at, strings.Repeat("例", 141)),
		draftBody("not_an_account", at, "Example"),
		draftBody(sm, testNow.Add(-time.Minute), "Example"),
		draftBody(sm, at),
		draftBody(sm, at, "Example", ""),
	}
	for i, b := range bad {
		if r := e.api("POST", "/api/v1/drafts", sec, b); r.status != http.StatusBadRequest {
			t.Errorf("bad body %d: %d %s", i, r.status, r.body)
		}
	}
	if r := e.api("POST", "/api/v1/drafts", sec, "{not json"); r.status != http.StatusBadRequest {
		t.Errorf("bad json: %d", r.status)
	}
	if r := e.api("POST", "/api/v1/drafts", sec, draftBody(sm, at, strings.Repeat("a", 280))); r.status != http.StatusCreated {
		t.Errorf("280 exactly: %d %s", r.status, r.body)
	}
}

// Times render in the owner's chosen zone, never a hardcoded one.
func TestTimesFollowOwnerZone(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, time.Date(2026, 6, 1, 18, 0, 0, 0, time.UTC), "Example")
	for tz, want := range map[string]string{"UTC": "6:00 PM", "Pacific/Auckland": "6:00 AM", "Asia/Kolkata": "11:30 PM", "America/Denver": "12:00 PM"} {
		if r := c.post("/settings", "/settings", url.Values{"tz": {tz}, "cap": {"10.00"}}); r.status != http.StatusSeeOther {
			t.Fatalf("settings %s: %d %s", tz, r.status, r.body)
		}
		if body := c.get("/queue").body; !strings.Contains(body, want) {
			t.Errorf("%s: queue lacks %q", tz, want)
		}
		_ = id
	}
	if r := c.post("/settings", "/settings", url.Values{"tz": {"Mars/Base"}, "cap": {"10"}}); r.status == http.StatusSeeOther {
		t.Fatal("accepted an invalid zone")
	}
}

func asConflict(err error, ce **store.ConflictError) bool {
	if err == nil {
		return false
	}
	c, ok := err.(*store.ConflictError)
	if ok {
		*ce = c
	}
	return ok
}

// Every redirect back to the queue shows its note; unknown keys show none.
func TestQueueFlashShown(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	id := e.propose(sec, jg, at(2, 9, 0), "Example post to approve")
	e.propose(sec, jg, at(3, 9, 0), "Example post still pending")
	r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, nil))
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.loc, "/queue?done=approved#d") {
		t.Fatalf("approve: %d %s", r.status, r.loc)
	}
	mustHave(t, c.get(r.loc).body, `class="hint ok flash" role="status"`, "<span>Approved. 1 left to review.</span>")
	for k, msg := range queueFlash {
		if k == "approved" {
			msg = "Approved. 1 left to review."
		}
		mustHave(t, c.get("/queue?done="+k).body, "<span>"+html.EscapeString(msg)+"</span>")
	}
	for _, k := range []string{"", "nope", "<b>x</b>"} {
		if p := c.get("/queue?done=" + url.QueryEscape(k)).body; strings.Contains(p, "hint ok flash") {
			t.Errorf("flash shown for %q", k)
		}
	}
}

// The queue shows each post in full: no clamp, line breaks kept, and every
// thread part labelled n/N (not only the first part).
func TestQueueShowsFullText(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	long := "Example line one of a long sample post for the demo workspace.\n\nExample line two keeps going with invented filler text, lorem ipsum dolor sit amet.\nhttps://example.com/" + strings.Repeat("a", 60) + "\nEnd."
	long += strings.Repeat("z", 280-len(long))
	if n := len([]rune(long)); n != 280 {
		t.Fatalf("fixture is %d chars", n)
	}
	single := e.propose(sec, jg, at(2, 9, 0), long)
	parts := []string{"Example thread part one.\nSecond line of part one.", "Example thread part two, " + strings.Repeat("longword", 20), "Example thread part three, the last one."}
	thread := e.propose(sec, sm, at(2, 10, 0), parts...)
	e.approveNow(thread)
	check := func(p string) {
		t.Helper()
		mustHave(t, p, `<p class="pbody">`+html.EscapeString(long)+`</p>`)
		for i, x := range parts {
			mustHave(t, p, `<span class="pn">`+strconv.Itoa(i+1)+`/3</span><p class="pbody">`+html.EscapeString(x)+`</p>`)
		}
		if strings.Contains(p, `class="preview"`) {
			t.Error("queue still uses the clamped preview")
		}
	}
	all := c.get("/queue").body + c.get("/queue?status=approved").body
	check(all)
	// A single post has no 1/1 label.
	if strings.Contains(all, ">1/1<") {
		t.Error("single post labelled 1/1")
	}
	// Posted and rejected cards use the same full text.
	posted := e.propose(sec, sm, at(2, 12, 0), "Example posted part one.\nLine two.", "Example posted part two.")
	e.approveNow(posted)
	e.markPosted(posted, "1900000000000000002")
	mustHave(t, c.get("/queue?status=posted").body, `<span class="pn">1/2</span><p class="pbody">Example posted part one.`+"\n"+`Line two.</p>`, `<span class="pn">2/2</span><p class="pbody">Example posted part two.</p>`)
	rej := e.propose(sec, jg, at(2, 13, 0), "Example rejected post.\nSecond line.")
	if r := c.post("/queue", path(rej, "/reject"), e.ownerForm(rej, nil)); r.status != http.StatusSeeOther {
		t.Fatalf("reject: %d", r.status)
	}
	mustHave(t, c.get("/queue?status=rejected").body, `<p class="pbody">Example rejected post.`+"\n"+`Second line.</p>`)
	// Reschedule shows the full text too.
	r := c.get(path(thread, "/reschedule")).body
	for i, x := range parts {
		mustHave(t, r, `<span class="pn">`+strconv.Itoa(i+1)+`/3</span><p class="pbody">`+html.EscapeString(x)+`</p>`)
	}
	mustHave(t, c.get(path(single, "/reschedule")).body, `<p class="pbody">`+html.EscapeString(long)+`</p>`)
	// The CSS scopes the full text: no clamp on .pbody, pre-wrap and anywhere.
	css := c.get("/static/xpost.css").body
	mustHave(t, css, ".pbody{margin:0;white-space:pre-wrap;overflow-wrap:anywhere;")
	if m := regexp.MustCompile(`\.p(text|body|part)\{[^}]*line-clamp`).FindString(css); m != "" {
		t.Errorf("full text clamped: %s", m)
	}
}
