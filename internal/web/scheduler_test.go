package web

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

// The cap has no default: a fresh install shows none and says nothing posts.
func TestSpendCapStartsUnsetAndFailsClosed(t *testing.T) {
	e := newEnv(t, nil)
	if _, ok := e.st.CapCents(context.Background()); ok {
		t.Fatal("a fresh install has a cap")
	}
	c, _ := e.setupOwner()
	r := c.get("/settings")
	if !strings.Contains(r.body, `id="cap" name="cap" value=""`) || !strings.Contains(r.body, "No cap is set, so nothing posts.") {
		t.Fatalf("settings: %s", r.body)
	}
	r = c.get("/log")
	for _, want := range []string{"No monthly spend cap is set", "no monthly cap set", "Scheduler off on this server", "Rates unverified", "unverified"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("log page lacks %q", want)
		}
	}
	// Saving the time zone with the cap left blank keeps it unset.
	if r := c.post("/settings", "/settings", url.Values{"tz": {"America/Denver"}, "cap": {""}}); r.status != http.StatusSeeOther {
		t.Fatalf("blank cap on first save: %d", r.status)
	}
	if _, ok := e.st.CapCents(context.Background()); ok {
		t.Fatal("blank created a cap")
	}
}

func TestOwnerSetsSpendCap(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	for _, bad := range []string{"0", "0.00", "-5", "abc", "10000.01", "1.234", "1e3", "20 dollars", "99999999"} {
		if r := c.post("/settings", "/settings", url.Values{"tz": {"UTC"}, "cap": {bad}}); r.status != http.StatusBadRequest {
			t.Errorf("cap %q: status %d", bad, r.status)
		}
	}
	if _, ok := e.st.CapCents(context.Background()); ok {
		t.Fatal("an invalid cap was stored")
	}
	if r := c.post("/settings", "/settings", url.Values{"tz": {"UTC"}, "cap": {"$20"}}); r.status != http.StatusSeeOther {
		t.Fatalf("set $20: %d %s", r.status, r.body)
	}
	if v, ok := e.st.CapCents(context.Background()); !ok || v != 2000 {
		t.Fatalf("cap %d %v", v, ok)
	}
	if !strings.Contains(e.logs.String(), "monthly spend cap changed from unset to $20.00 by the owner") {
		t.Fatalf("change not logged: %s", e.logs.String())
	}
	if r := c.get("/settings"); !strings.Contains(r.body, `value="20.00"`) {
		t.Fatal("cap not shown")
	}
	// Once set it can be changed but not blanked.
	if r := c.post("/settings", "/settings", url.Values{"tz": {"UTC"}, "cap": {""}}); r.status != http.StatusBadRequest {
		t.Fatalf("blanking a set cap: %d", r.status)
	}
	if r := c.post("/settings", "/settings", url.Values{"tz": {"UTC"}, "cap": {"10000"}}); r.status != http.StatusSeeOther {
		t.Fatalf("max cap: %d", r.status)
	}
	if !strings.Contains(e.logs.String(), "from $20.00 to $10000.00") {
		t.Fatalf("second change not logged: %s", e.logs.String())
	}
	if r := c.get("/log"); !strings.Contains(r.body, "of <b class=\"t\">$10000.00</b> monthly cap") || strings.Contains(r.body, "No monthly spend cap is set") {
		t.Fatalf("log page after cap: %s", r.body)
	}
}

// Agent tokens can't change the cap: not through the web route, and the
// API has no settings route at all.
func TestAgentCannotChangeSpendCap(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead)
	anon := e.client()
	csrf := anon.csrf("/login")
	for _, hdr := range []map[string]string{
		{"Authorization": "Bearer " + sec},
		{"Authorization": "Bearer " + sec, "Cookie": SessionCookie + "=" + sec},
	} {
		r := anon.postRaw("/settings", url.Values{"csrf": {csrf}, "tz": {"UTC"}, "cap": {"20"}}, hdr)
		if r.status != http.StatusUnauthorized && r.status != http.StatusForbidden {
			t.Fatalf("POST /settings with bearer: %d", r.status)
		}
	}
	for _, m := range []string{"GET", "POST", "PUT", "PATCH"} {
		for _, p := range []string{"/api/v1/settings", "/api/v1/settings/cap", "/api/v1/cap", "/api/v1/spend"} {
			if r := e.api(m, p, sec, `{"cap":"20","cap_cents":2000}`); r.status < 400 {
				t.Fatalf("%s %s: %d", m, p, r.status)
			}
		}
	}
	if _, ok := e.st.CapCents(context.Background()); ok {
		t.Fatal("agent set the cap")
	}
}

func TestLogShowsSchedulerStateAndRates(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Scheduler = "on" })
	e.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	e.st.SetSetting(context.Background(), store.SettingCapCents, "2000")
	c, _ := e.setupOwner()
	r := c.get("/log")
	for _, want := range []string{"Scheduler on: approved posts publish to X at their time.", "of <b class=\"t\">$20.00</b> monthly cap", "Rates come from the server config"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("log page lacks %q", want)
		}
	}
	if strings.Contains(r.body, "Rates unverified") {
		t.Error("rates shown as unverified")
	}
}

// failedThread makes a 3-part draft whose part 2 failed (or is unknown).
func failedThread(t *testing.T, e *env, sec string, fail error) int64 {
	t.Helper()
	e.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	e.st.SetSetting(context.Background(), store.SettingCapCents, "2000")
	id := e.propose(sec, sm, testNow.Add(20*time.Minute), "Example one", "Example two", "Example three")
	e.approveNow(id)
	e.advance(time.Hour)
	fake := &poster.Fake{Fail: func(r poster.Request) error {
		if r.Text == "Example two" {
			return fail
		}
		return nil
	}}
	if _, err := (&poster.Dispatcher{Store: e.st, Poster: fake}).RunDue(context.Background(), e.clock()); err != nil {
		t.Fatal(err)
	}
	if d := e.draft(id); d.Status != store.StatusFailed {
		t.Fatalf("status %s", d.Status)
	}
	return id
}

func TestOwnerRetryFromLog(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead)
	id := failedThread(t, e, sec, &poster.Error{Kind: poster.Refused, Status: 400, Msg: "X refused it: example"})
	c, _ := e.setupOwner()
	r := c.get("/log")
	if !strings.Contains(r.body, "Retry parts 2 to 3") || !strings.Contains(r.body, "Part 2 of 3 failed: X refused it: example") {
		t.Fatalf("log: %s", r.body)
	}
	// An agent can't retry.
	anon := e.client()
	csrf := anon.csrf("/login")
	if r := anon.postRaw(path(id, "/retry"), e.ownerForm(id, url.Values{"csrf": {csrf}}), map[string]string{"Authorization": "Bearer " + sec}); r.status != http.StatusUnauthorized && r.status != http.StatusForbidden {
		t.Fatalf("agent retry: %d", r.status)
	}
	if r := e.api("POST", "/api/v1/drafts/"+itoa(id)+"/retry", sec, `{}`); r.status < 400 {
		t.Fatalf("api retry: %d", r.status)
	}
	// The owner needs CSRF.
	if r := c.postRaw(path(id, "/retry"), e.ownerForm(id, nil), nil); r.status != http.StatusForbidden {
		t.Fatalf("retry without csrf: %d", r.status)
	}
	if e.draft(id).Status != store.StatusFailed {
		t.Fatal("changed without the owner")
	}
	if r := c.post("/log", path(id, "/retry"), e.ownerForm(id, nil)); r.status != http.StatusSeeOther || r.loc != "/log?done=retry" {
		t.Fatalf("owner retry: %d %s", r.status, r.loc)
	}
	if d := e.draft(id); d.Status != store.StatusPosting {
		t.Fatalf("after retry: %s", d.Status)
	}
}

func TestOwnerResolvesUnknownFromLog(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead)
	id := failedThread(t, e, sec, &poster.Error{Kind: poster.Ambiguous, Msg: "timed out waiting for X; the post may or may not exist"})
	c, _ := e.setupOwner()
	r := c.get("/log")
	if !strings.Contains(r.body, "needs review") || !strings.Contains(r.body, "It posted") || strings.Contains(r.body, "Retry parts") {
		t.Fatalf("log: %s", r.body)
	}
	// Retry is refused while the outcome is unknown.
	if r := c.post("/log", path(id, "/retry"), e.ownerForm(id, nil)); r.status != http.StatusConflict {
		t.Fatalf("retry before review: %d", r.status)
	}
	// Agents can't resolve.
	anon := e.client()
	csrf := anon.csrf("/login")
	f := e.ownerForm(id, url.Values{"csrf": {csrf}, "part": {"2"}, "outcome": {"posted"}, "post": {"1234"}})
	if r := anon.postRaw(path(id, "/resolve"), f, map[string]string{"Authorization": "Bearer " + sec}); r.status != http.StatusUnauthorized && r.status != http.StatusForbidden {
		t.Fatalf("agent resolve: %d", r.status)
	}
	if r := c.post("/log", path(id, "/resolve"), e.ownerForm(id, url.Values{"part": {"2"}, "outcome": {"posted"}, "post": {"not a link"}})); r.status != http.StatusBadRequest {
		t.Fatalf("bad link: %d", r.status)
	}
	link := "https://x.invalid/sample_writer/status/1990000000000000123?s=20"
	if r := c.post("/log", path(id, "/resolve"), e.ownerForm(id, url.Values{"part": {"2"}, "outcome": {"posted"}, "post": {link}, "account": {sm}})); r.status != http.StatusSeeOther {
		t.Fatalf("resolve: %d %s", r.status, r.body)
	}
	rows, _ := e.st.DraftLog(context.Background(), id)
	if rows[1].Status != store.LogPosted || rows[1].PostID != "1990000000000000123" {
		t.Fatalf("row %+v", rows[1])
	}
	if r := c.post("/log", path(id, "/retry"), e.ownerForm(id, nil)); r.status != http.StatusSeeOther {
		t.Fatalf("retry after review: %d", r.status)
	}
}

// The agent log API reports whether rates are configured and leaves cost
// empty when they are not.
func TestAPILogRates(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeLogRead)
	e.st.RecordPart(context.Background(), store.LogEntry{DraftID: 1, Account: sm, Part: 1, Parts: 1, At: e.clock(), Status: store.LogPosted, PostID: "1", IdemKey: "k1"})
	r := e.api("GET", "/api/v1/log?since=2026-01-01T00:00:00Z", sec, nil)
	if !strings.Contains(r.body, `"rates_configured":false`) || !strings.Contains(r.body, `"cost_estimate_usd":""`) {
		t.Fatalf("%s", r.body)
	}
	e.st.Rates = store.Rates{PostMicros: 15_000, LinkPostMicros: 200_000, Known: true}
	r = e.api("GET", "/api/v1/log?since=2026-01-01T00:00:00Z", sec, nil)
	if !strings.Contains(r.body, `"rates_configured":true`) || !strings.Contains(r.body, `"cost_estimate_usd":"0.015"`) {
		t.Fatalf("%s", r.body)
	}
}
