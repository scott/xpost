package web

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Invented rates for these tests (not X's): 1 cent a part, 25 cents with a
// link, 5 cents an image upload, 2 cents an alt text call.
func enableCost(e *env, capCents string) {
	e.st.Rates = store.Rates{PostMicros: 10_000, LinkPostMicros: 250_000, Known: true, UploadMicros: 50_000, UploadKnown: true, AltMicros: 20_000, AltKnown: true}
	if capCents != "" {
		e.st.SetSetting(context.Background(), store.SettingCapCents, capCents)
	}
}

// composeForm is a filled-in composer: one part on @sample_writer at a
// picked time (testNow is Mon Jun 1 15:00 UTC).
func composeForm(clock string, texts ...string) url.Values {
	f := url.Values{"acct": {sm}, "mode": {"pick"}, "date": {"2026-06-01"}, "time": {clock}, "from": {"queue"}, "parts": {itoa(int64(len(texts)))}}
	for i, s := range texts {
		f.Set("text_"+itoa(int64(i)), s)
	}
	return f
}

func (c *client) compose(f url.Values) resp {
	c.e.t.Helper()
	return c.post("/compose", "/compose", f)
}

// composeFiles posts the composer as multipart with image files.
func (c *client) composeFiles(f url.Values, files map[string][][]byte) resp {
	c.e.t.Helper()
	f.Set("csrf", c.csrf("/compose"))
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for k, vs := range f {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	for field, list := range files {
		for _, data := range list {
			fw, _ := mw.CreateFormFile(field, "example.png")
			fw.Write(data)
		}
	}
	mw.Close()
	req, _ := http.NewRequest("POST", c.e.ts.URL+"/compose", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return c.e.do(c.c, req)
}

// owned lists the drafts the owner wrote (no agent token).
func owned(e *env) []*store.Draft {
	e.t.Helper()
	ds, err := e.st.Between(context.Background(), testNow.Add(-24*time.Hour), testNow.Add(60*24*time.Hour))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []*store.Draft
	for _, d := range ds {
		if d.TokenID == 0 {
			out = append(out, d)
		}
	}
	return out
}

func TestComposeEntryPoints(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	q := c.get("/queue").body
	mustHave(t, q, `class="btn pri newpost" href="/compose"`, `<a class="btn pri" href="/compose">`, `<a class="ib plus" href="/compose" aria-label="New post">`)
	s := c.get("/schedule").body
	mustHave(t, s, `class="btn pri newpost" href="/compose?from=schedule"`, `<a class="ib plus" href="/compose?from=schedule" aria-label="New post">`)
	// A day picked on the schedule carries into Pick a time.
	s = c.get("/schedule?day=2026-06-03").body
	mustHave(t, s, `href="/compose?from=schedule&amp;day=2026-06-03"`)
	p := c.get("/compose?from=schedule&day=2026-06-03")
	if p.status != http.StatusOK {
		t.Fatalf("compose: %d", p.status)
	}
	mustHave(t, p.body, `name="date" value="2026-06-03"`, `name="time" step="60" value="09:00"`, `name="from" value="schedule"`, `<h1>New post</h1>`, `form="cf" name="op" value="cancel"`)
	// Other pages have no phone "+".
	if b := c.get("/log").body; strings.Contains(b, `class="ib plus"`) {
		t.Error("log page has the phone New post button")
	}
}

// The default is Next free slot: 10 minutes out on the 5-minute grid,
// stepped past neighbors with the same helper as Move to.
func TestComposeNextFreeSlot(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	p := c.get("/compose").body
	mustHave(t, p, `<span class="t">Today, Mon Jun 1 · 3:10 PM</span>`, `name="slot" value="`+itoa(testNow.Add(10*time.Minute).Unix())+`"`, "No other @sample_writer posts within a day. Spacing OK.")
	_, sec := e.token("sample-agent")
	id := e.propose(sec, sm, at(1, 15, 12), "Example neighbor")
	e.approveNow(id)
	// 3:12 + 15 = 3:27, rounded up to 3:30.
	p = c.get("/compose").body
	mustHave(t, p, "Today, Mon Jun 1 · 3:30 PM", "18 min after 3:12. Spacing OK.")
	// Schedule in Next free slot mode uses the slot the page showed.
	f := url.Values{"acct": {sm}, "mode": {"next"}, "slot": {itoa(at(1, 15, 30).Unix())}, "parts": {"1"}, "text_0": {"Example next slot post"}, "op": {"schedule"}}
	if r := c.compose(f); r.status != http.StatusSeeOther || !strings.HasPrefix(r.loc, "/compose/done/") {
		t.Fatalf("schedule next slot: %d %s %s", r.status, r.loc, r.body)
	}
	if ds := owned(e); len(ds) != 1 || !ds[0].ScheduledAt.Equal(at(1, 15, 30)) {
		t.Fatalf("owned %+v", ds)
	}
	// A stale slot is not used silently.
	f.Set("slot", itoa(at(1, 15, 10).Unix()))
	f.Set("text_0", "Example stale slot post")
	if r := c.compose(f); r.status != http.StatusConflict || !strings.Contains(r.body, "The next free slot changed to") {
		t.Fatalf("stale slot: %d", r.status)
	}
}

func TestComposeValidation(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	// Empty: both actions refused, the reason shown.
	p := c.get("/compose").body
	mustHave(t, p, `data-why>Write something to schedule</span>`, `value="draft" data-save data-gate="draft" data-other="" disabled`, `value="schedule" data-save data-gate="schedule" data-other="" data-tail="" disabled`)
	for _, op := range []string{"schedule", "draft"} {
		f := composeForm("16:00", "   ")
		f.Set("op", op)
		if r := c.compose(f); r.status != http.StatusBadRequest || !strings.Contains(r.body, "Write something to") {
			t.Fatalf("empty %s: %d", op, r.status)
		}
	}
	// Over 280 (weighted: CJK counts 2) blocks both, with the split offer.
	over := strings.Repeat("例", 141)
	for _, op := range []string{"schedule", "draft"} {
		f := composeForm("16:00", "Example first part", over)
		f.Set("op", op)
		r := c.compose(f)
		if r.status != http.StatusBadRequest {
			t.Fatalf("over %s: %d", op, r.status)
		}
		mustHave(t, r.body, "Part 2 is over 280", "2 over. CJK and emoji count 2 each.", `value="split:1"`, `class="card part over"`)
	}
	// An empty part in a thread is refused too.
	f := composeForm("16:00", "Example first part", "")
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusBadRequest || !strings.Contains(r.body, "Part 2 is empty") {
		t.Fatalf("empty part: %d", r.status)
	}
	// A past time is refused for both.
	f = composeForm("14:00", "Example past post")
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusBadRequest || !strings.Contains(r.body, "Pick a time in the future") {
		t.Fatalf("past: %d", r.status)
	}
	// Images: a fifth is refused; the four upload through the owner path.
	png := pngBytes(t)
	f = composeForm("16:00", "Example with images")
	f.Set("op", "check")
	f.Set("media_0", "0")
	r := c.composeFiles(f, map[string][][]byte{"add_0": {png, png, png, png, png}})
	if r.status != http.StatusOK {
		t.Fatalf("upload: %d %s", r.status, r.body)
	}
	mustHave(t, r.body, "Part 1 already has 4 images.", `name="media_0" value="4"`, `Image <span class="muted t">4/4</span>`, `placeholder="Describe this image"`)
	if strings.Contains(r.body, `name="add_0"`) {
		t.Error("add image offered at 4/4")
	}
	// Not an image: refused with the store's message, nothing attached.
	r = c.composeFiles(composeForm("16:00", "Example"), map[string][][]byte{"add_0": {[]byte("example text, not an image")}})
	if !strings.Contains(r.body, `name="media_0" value="0"`) {
		t.Error("a non-image was attached")
	}
	// The owner can't attach an agent's upload by id.
	_, sec := e.token("sample-agent")
	up := upload(e, sec, png)
	if up.status != http.StatusCreated {
		t.Fatal(up.body)
	}
	var agentMedia int64
	e.st.DB.QueryRow(`SELECT id FROM media WHERE token_id IS NOT NULL`).Scan(&agentMedia)
	f = composeForm("16:00", "Example with an agent image")
	f.Set("media_0", "1")
	f.Set("mid_0_0", itoa(agentMedia))
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusBadRequest || !strings.Contains(r.body, "part 1: unknown image") {
		t.Fatalf("agent media: %d", r.status)
	}
	if n := len(owned(e)); n != 0 {
		t.Fatalf("%d drafts created by refused submits", n)
	}
}

// Schedule creates an approved draft written by the owner; Save as draft a
// pending one at the chosen time. A resubmit does not duplicate.
func TestComposeApprovedVersusPending(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	f := composeForm("16:00", "Example scheduled post", "Example reply https://example.com/sample")
	f.Set("op", "schedule")
	f.Set("nonce", strings.Repeat("a", 32))
	r := c.compose(f)
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.loc, "/compose/done/") {
		t.Fatalf("schedule: %d %s %s", r.status, r.loc, r.body)
	}
	ds := owned(e)
	if len(ds) != 1 {
		t.Fatalf("%d drafts", len(ds))
	}
	d := ds[0]
	if d.Status != store.StatusApproved || d.ApprovedAt == nil || d.Source != store.OwnerSource || len(d.Parts) != 2 || !d.ScheduledAt.Equal(at(1, 16, 0)) {
		t.Fatalf("scheduled draft %+v", d)
	}
	evs, _ := e.st.Events(context.Background(), d.ID)
	if len(evs) != 1 || evs[0].Kind != "written and approved" || evs[0].Actor != "you" {
		t.Fatalf("events %+v", evs)
	}
	done := c.get(r.loc)
	mustHave(t, done.body, `role="status"`, "Scheduled for Mon 4:00 PM", "Mon, Jun 1, 2026 · 4:00 PM UTC", "Written by you", `<span class="st approved">approved</span>`, "View in Schedule", "Write another", "Back to Review queue", "Thread · 2 parts")
	// The same form again returns the first draft.
	if r := c.compose(f); r.status != http.StatusSeeOther || len(owned(e)) != 1 {
		t.Fatalf("resubmit: %d, %d drafts", r.status, len(owned(e)))
	}
	// Save as draft: pending, at the picked time, in the queue as yours.
	f = composeForm("17:00", "Example saved draft")
	f.Set("op", "draft")
	r = c.compose(f)
	if r.status != http.StatusSeeOther || r.loc != "/queue?done=drafted" {
		t.Fatalf("draft: %d %s", r.status, r.loc)
	}
	ds = owned(e)
	if len(ds) != 2 || ds[1].Status != store.StatusPending || ds[1].ApprovedAt != nil || !ds[1].ScheduledAt.Equal(at(1, 17, 0)) {
		t.Fatalf("drafts %+v", ds)
	}
	q := c.get("/queue?done=drafted").body
	mustHave(t, q, "Saved as draft. It is in Pending until you approve it.", "Written by you")
	// Agents never see owner drafts.
	_, sec := e.token("sample-agent")
	if l := e.api("GET", "/api/v1/drafts?status=pending", sec, nil); strings.Contains(l.body, "Example saved draft") {
		t.Fatal("agent sees an owner draft")
	}
}

// The cap blocks Schedule, not Save as draft. Room counts what the month
// spent and every approved post already scheduled in it.
func TestComposeCapBlocksScheduleNotDraft(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "30") // $0.30
	_, sec := e.token("sample-agent")
	id := e.propose(sec, jg, at(1, 18, 0), "Example approved https://example.com/a")
	e.approveNow(id) // $0.25 scheduled: $0.05 left
	f := composeForm("16:00", "Example post", "Example reply https://example.com/sample")
	f.Set("op", "check")
	p := c.compose(f).body
	mustHave(t, p, "Over the $0.30 monthly cap by <span class=\"t\">$0.21</span>", "This post needs $0.26 and only $0.05 is left this month.", "Remove the link in part 2 to save $0.24",
		"Part 2 has a link, so it costs $0.25 instead of $0.01.", "Scheduled $0.25", `data-why>Over the monthly cap</span>`)
	f.Set("op", "schedule")
	if r := c.compose(f); r.status != http.StatusBadRequest || len(owned(e)) != 0 {
		t.Fatalf("schedule over cap: %d, %d drafts", r.status, len(owned(e)))
	}
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusSeeOther || len(owned(e)) != 1 || owned(e)[0].Status != store.StatusPending {
		t.Fatalf("draft over cap: %d", r.status)
	}
	// Under the cap (no link: $0.02) schedules.
	f = composeForm("16:30", "Example post", "Example reply")
	f.Set("op", "schedule")
	if r := c.compose(f); r.status != http.StatusSeeOther {
		t.Fatalf("schedule under cap: %d %s", r.status, r.body)
	}
	// Over 280 and over the cap: one reason line names both.
	f = composeForm("17:30", "Example https://example.com/a", "Example https://example.com/b", strings.Repeat("x", 281))
	f.Set("op", "check")
	mustHave(t, c.compose(f).body, "Part 3 is over 280 and over the monthly cap")
}

// Fail closed: with no cap, or rates unset, Schedule is off; drafts save.
func TestComposeFailsClosedWithoutCapOrRates(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(e *env)
		want string
	}{
		{"no cap", func(e *env) { enableCost(e, "") }, "No monthly spend cap is set."},
		{"no rates", func(e *env) { e.st.SetSetting(context.Background(), store.SettingCapCents, "2000") }, "Cost rates are not configured on this server (XPOST_COST_POST_USD"},
		{"no image rate", func(e *env) {
			enableCost(e, "2000")
			e.st.Rates.UploadKnown = false
		}, "The image upload cost rate (XPOST_COST_MEDIA_UPLOAD_USD) is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			c, _ := e.setupOwner()
			tc.set(e)
			f := composeForm("16:00", "Example post")
			if tc.name == "no image rate" {
				_, mid, err := e.st.AddMedia(context.Background(), nil, pngBytes(t))
				if err != nil {
					t.Fatal(err)
				}
				f.Set("media_0", "1")
				f.Set("mid_0_0", itoa(mid))
			}
			f.Set("op", "check")
			mustHave(t, c.compose(f).body, tc.want, "Schedule is off until the cost check can run")
			f.Set("op", "schedule")
			if r := c.compose(f); r.status != http.StatusBadRequest || len(owned(e)) != 0 {
				t.Fatalf("schedule: %d", r.status)
			}
			f.Set("op", "draft")
			if r := c.compose(f); r.status != http.StatusSeeOther || len(owned(e)) != 1 {
				t.Fatalf("draft: %d %s", r.status, r.body)
			}
		})
	}
}

// A spacing conflict blocks Schedule and offers Use 9:20 style: the
// earliest clear minute from moveTarget, not the next quarter hour.
func TestComposeSpacingAndUseTarget(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	_, sec := e.token("sample-agent")
	a := e.propose(sec, jg, at(1, 16, 5), "Example approved neighbor")
	e.approveNow(a)
	b := e.propose(sec, jg, at(1, 17, 30), "Example later neighbor")
	e.approveNow(b)
	f := composeForm("16:15", "Example conflicting post")
	f.Set("acct", jg)
	f.Set("op", "check")
	p := c.compose(f).body
	mustHave(t, p, "10 min after 4:05. Needs 15. Use 4:20.", `name="op" value="use:2026-06-01T16:20">Use 4:20</button>`, `data-why>Fix spacing to schedule</span>`,
		`class="cl-win co"`, `class="cl-mk gh"`, "Conflict</span>")
	if strings.Contains(p, "data-ok") {
		t.Error("conflicting When panel marked OK")
	}
	f.Set("op", "schedule")
	if r := c.compose(f); r.status != http.StatusBadRequest || len(owned(e)) != 0 {
		t.Fatalf("schedule with conflict: %d", r.status)
	}
	// Use 4:20 moves only this post.
	f.Set("op", "use:2026-06-01T16:20")
	p = c.compose(f).body
	mustHave(t, p, `name="time" step="60" value="16:20"`, "15 min after 4:05. 70 min before 5:30. Spacing OK.", "data-ok")
	if d := e.draft(a); !d.ScheduledAt.Equal(at(1, 16, 5)) || d.Status != store.StatusApproved {
		t.Fatal("the neighbor moved")
	}
	f.Set("time", "16:20")
	f.Set("op", "schedule")
	if r := c.compose(f); r.status != http.StatusSeeOther {
		t.Fatalf("schedule at 4:20: %d %s", r.status, r.body)
	}
	// Save as draft keeps a conflicting time.
	f = composeForm("16:25", "Example conflicting draft")
	f.Set("acct", jg)
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusSeeOther {
		t.Fatalf("draft with conflict: %d", r.status)
	}
	ds := owned(e)
	if len(ds) != 2 || !ds[1].ScheduledAt.Equal(at(1, 16, 25)) || ds[1].Status != store.StatusPending {
		t.Fatalf("drafts %+v", ds)
	}
	if _, _, err := e.st.CreateOwnerDraft(context.Background(), nil, store.OwnerDraft{Account: jg, Parts: []store.PartIn{{Text: "Example"}}, When: at(1, 20, 0)}, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("nil owner: %v", err)
	}
}

// Owner session and CSRF only; an agent token gets nowhere.
func TestComposeNeedsOwnerAndCSRF(t *testing.T) {
	e := newEnv(t, nil)
	c, k := e.setupOwner()
	enableCost(e, "2000")
	f := composeForm("16:00", "Example post")
	f.Set("op", "schedule")
	if r := e.client().get("/compose"); r.status != http.StatusSeeOther || r.loc != "/login" {
		t.Fatalf("anonymous get: %d %s", r.status, r.loc)
	}
	if r := c.postRaw("/compose", f, nil); r.status != http.StatusForbidden {
		t.Fatalf("no csrf: %d", r.status)
	}
	g := url.Values{}
	for key, v := range f {
		g[key] = v
	}
	g.Set("csrf", "forged")
	if r := c.postRaw("/compose", g, nil); r.status != http.StatusForbidden {
		t.Fatalf("forged csrf: %d", r.status)
	}
	g.Set("csrf", c.csrf("/compose"))
	if r := c.postRaw("/compose", g, map[string]string{"Sec-Fetch-Site": "cross-site"}); r.status != http.StatusForbidden {
		t.Fatalf("cross-site: %d", r.status)
	}
	// A password-only session is not an owner session.
	half := e.passwordStep()
	if r := half.get("/compose"); r.status != http.StatusSeeOther || r.loc != "/login" {
		t.Fatalf("half session get: %d %s", r.status, r.loc)
	}
	if r := half.post("/login/passkey", "/compose", f); r.status != http.StatusUnauthorized {
		t.Fatalf("half session post: %d", r.status)
	}
	// Agent bearer tokens: no session, so no composer and no CSRF pass.
	_, sec := e.token("sample-agent")
	for _, m := range []string{"GET", "POST"} {
		req, _ := http.NewRequest(m, e.ts.URL+"/compose", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+sec)
		r := e.do(e.client().c, req)
		if r.status == http.StatusOK || (m == "POST" && r.status != http.StatusForbidden && r.status != http.StatusUnauthorized) {
			t.Fatalf("agent %s /compose: %d", m, r.status)
		}
	}
	// The agent API still has no way to ask for an approved draft.
	body := draftBody(sm, at(1, 19, 0), "Example agent")
	body["status"] = "approved"
	if r := e.api("POST", "/api/v1/drafts", sec, body); r.status != http.StatusBadRequest {
		t.Fatalf("agent approved: %d %s", r.status, r.body)
	}
	if n := len(owned(e)); n != 0 {
		t.Fatalf("%d owner drafts after refused posts", n)
	}
	// With a full session and CSRF it works.
	c = e.login(k)
	if r := c.compose(f); r.status != http.StatusSeeOther {
		t.Fatalf("owner: %d %s", r.status, r.body)
	}
}

func TestComposeStructuralOps(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	// Thread adds part 2; Single refuses to drop written parts.
	f := composeForm("16:00", "Example first")
	f.Set("op", "fmt:thread")
	mustHave(t, c.compose(f).body, `name="parts" value="2"`, "Part 2 of 2", `aria-label="Remove part 2"`)
	f = composeForm("16:00", "Example first", "Example second")
	f.Set("op", "fmt:single")
	mustHave(t, c.compose(f).body, "Remove part 2 first to make this a single post.", `name="parts" value="2"`)
	f = composeForm("16:00", "Example first", "")
	f.Set("op", "fmt:single")
	mustHave(t, c.compose(f).body, `name="parts" value="1"`, "<b>Post</b>")
	// Split moves the overflow into a new part at a word boundary.
	long := strings.TrimSpace(strings.Repeat("example ", 40)) // 319
	f = composeForm("16:00", long)
	f.Set("op", "split:0")
	p := c.compose(f).body
	mustHave(t, p, `name="parts" value="2"`)
	if strings.Contains(p, "is over 280") {
		t.Error("still over after split")
	}
	head, rest := splitPart(long)
	if head+" "+rest != long || strings.HasSuffix(head, " ") || len(head) > 280 {
		t.Errorf("split %q | %q", head, rest)
	}
	// Cancel with text asks first; empty goes straight back.
	f = composeForm("16:00", "Example unsaved")
	f.Set("op", "cancel")
	mustHave(t, c.compose(f).body, "Discard this post? What you wrote is not saved.", `<a class="btn danger" href="/queue">Discard</a>`)
	f = composeForm("16:00", "")
	f.Set("from", "schedule")
	f.Set("op", "cancel")
	if r := c.compose(f); r.status != http.StatusSeeOther || r.loc != "/schedule" {
		t.Fatalf("empty cancel: %d %s", r.status, r.loc)
	}
	if len(owned(e)) != 0 {
		t.Fatal("a structural op saved a draft")
	}
}

func TestComposeGapText(t *testing.T) {
	for d, want := range map[time.Duration]string{25 * time.Minute: "25 min", 119 * time.Minute: "119 min", 3 * time.Hour: "3 h", 18*time.Hour + 15*time.Minute: "18 h 15 min"} {
		if got := gapText(d); got != want {
			t.Errorf("gapText(%v) = %q, want %q", d, got, want)
		}
	}
}

// Times are in the configured zone, never a fixed one.
func TestComposeUsesConfiguredZone(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	e := newEnv(t, func(c *Config) { c.Location = tokyo })
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	p := c.get("/compose").body
	// testNow is 00:00 Jun 2 in Tokyo: the slot is 00:10.
	mustHave(t, p, "Today, Tue Jun 2 · 12:10 AM", "Asia/Tokyo (JST)")
	for _, bad := range []string{"MST", "MDT", "America/Denver", "Mountain"} {
		if strings.Contains(p, bad) {
			t.Errorf("compose page mentions %s", bad)
		}
	}
	f := composeForm("09:00", "Example Tokyo post")
	f.Set("date", "2026-06-02")
	f.Set("op", "schedule")
	r := c.compose(f)
	if r.status != http.StatusSeeOther {
		t.Fatalf("schedule: %d %s", r.status, r.body)
	}
	if d := owned(e)[0]; !d.ScheduledAt.Equal(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("scheduled at %v", d.ScheduledAt)
	}
	mustHave(t, c.get(r.loc).body, "Scheduled for Tue 9:00 AM", "9:00 AM JST")
}

// Only approved neighbours block Schedule (page and store). A pending one
// warns, and approving it afterwards still runs the usual spacing check.
func TestComposePendingNeighbourWarnsApprovedBlocks(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	enableCost(e, "2000")
	_, sec := e.token("sample-agent")
	pend := e.propose(sec, jg, at(2, 9, 5), "Example pending neighbour")
	appr := e.propose(sec, jg, at(2, 11, 0), "Example approved neighbour")
	e.approveNow(appr)

	// Pending neighbour 10 min before: warning, no block, no Use button.
	f := composeForm("09:15", "Example post near a pending draft")
	f.Set("acct", jg)
	f.Set("date", "2026-06-02")
	f.Set("op", "check")
	p := c.compose(f).body
	mustHave(t, p, "A pending draft at 9:05 would conflict; it&#39;ll need moving to be approved.")
	for _, bad := range []string{"Fix spacing to schedule", `value="use:`, "Needs 15.", "data-ok", "\u2014"} {
		if strings.Contains(p, bad) {
			t.Errorf("pending neighbour page has %q", bad)
		}
	}
	f.Set("op", "schedule")
	if r := c.compose(f); r.status != http.StatusSeeOther {
		t.Fatalf("schedule next to a pending draft: %d %s", r.status, r.body)
	}
	ds := owned(e)
	if len(ds) != 1 || ds[0].Status != store.StatusApproved || !ds[0].ScheduledAt.Equal(at(2, 9, 15)) {
		t.Fatalf("owned %+v", ds)
	}
	// Approving the pending draft now runs the existing spacing check: the
	// new approved post 10 min later blocks it, and it stays pending.
	r := c.post("/queue", path(pend, "/approve"), e.ownerForm(pend, nil))
	if r.status != http.StatusConflict || !strings.Contains(r.body, "Only 10 min from an approved @sample_maker post.") {
		t.Fatalf("approve pending neighbour: %d", r.status)
	}
	if d := e.draft(pend); d.Status != store.StatusPending {
		t.Fatalf("pending neighbour is %s", d.Status)
	}
	err := e.st.Approve(context.Background(), e.owner, pend, e.draft(pend).Version)
	var ce *store.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("store approve: %v", err)
	}

	// Store, directly: a pending neighbour does not block an owner Schedule.
	if _, _, err := e.st.CreateOwnerDraft(context.Background(), e.owner, store.OwnerDraft{Account: jg, Parts: []store.PartIn{{Text: "Example"}}, When: at(2, 8, 55)}, true); err != nil {
		t.Fatalf("store, pending neighbour: %v", err)
	}

	// Approved neighbour 5 min after: blocks on the page and in the store.
	f = composeForm("10:55", "Example post near an approved post")
	f.Set("acct", jg)
	f.Set("date", "2026-06-02")
	f.Set("op", "check")
	p = c.compose(f).body
	mustHave(t, p, "5 min before 11:00. Needs 15.", "Fix spacing to schedule", `value="use:2026-06-02T11:15"`)
	if strings.Contains(p, "would conflict; it") {
		t.Error("approved neighbour shown as a pending warning")
	}
	f.Set("op", "schedule")
	n := len(owned(e))
	if r := c.compose(f); r.status != http.StatusBadRequest || len(owned(e)) != n {
		t.Fatalf("schedule next to an approved post: %d", r.status)
	}
	_, _, err = e.st.CreateOwnerDraft(context.Background(), e.owner, store.OwnerDraft{Account: jg, Parts: []store.PartIn{{Text: "Example"}}, When: at(2, 10, 50)}, true)
	if !errors.As(err, &ce) || !strings.Contains(ce.Msg, "approved @sample_maker") {
		t.Fatalf("store, approved neighbour: %v", err)
	}
	// Save as draft is still allowed next to the approved post.
	f.Set("op", "draft")
	if r := c.compose(f); r.status != http.StatusSeeOther || len(owned(e)) != n+1 {
		t.Fatalf("draft next to an approved post: %d", r.status)
	}
}

func TestComposePendingWarnPlural(t *testing.T) {
	ns := []store.Neighbor{{At: at(2, 9, 5)}, {At: at(2, 9, 20)}}
	got := pendingWarn(ns, at(2, 9, 12), time.UTC)
	if want := "Pending drafts at 9:05 and 9:20 would conflict; they'll need moving to be approved."; got != want {
		t.Fatalf("got %q", got)
	}
}
