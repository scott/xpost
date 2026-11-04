package web

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// at is a UTC instant on the test week (testNow is Mon 2026-06-01 15:00 UTC).
func at(day, hh, mm int) time.Time { return time.Date(2026, 6, day, hh, mm, 0, 0, time.UTC) }

// mockWeek seeds the day from Sol's mock on Tue 2026-06-02 (UTC): a pending
// thread at 9:00 and an approved thread at 10:30 on @sample_writer; on
// @sample_maker an approved 9:05, a pending 9:15 (10 min after it) and an
// approved 2:00 PM. All copy is invented.
func mockWeek(t *testing.T) (*env, *client, map[string]int64) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	ids := map[string]int64{}
	ids["sm900"] = e.propose(sec, sm, at(2, 9, 0), "Example thread", "Example part two", "Example part three")
	ids["jg905"] = e.propose(sec, jg, at(2, 9, 5), "Example single")
	e.approveNow(ids["jg905"])
	ids["jg915"] = e.propose(sec, jg, at(2, 9, 15), "Example too close")
	ids["sm1030"] = e.propose(sec, sm, at(2, 10, 30), "Example thread B", "Example part two", "Example part three")
	e.approveNow(ids["sm1030"])
	ids["jg1400"] = e.propose(sec, jg, at(2, 14, 0), "Example afternoon")
	e.approveNow(ids["jg1400"])
	return e, c, ids
}

func mustHave(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
}

// Each day is one row with a lane per account on an 8 AM to 7 PM axis; each
// post is a marker at its time, one 15-minute window wide, colored by status.
func TestScheduleLanesAndBlocks(t *testing.T) {
	_, c, ids := mockWeek(t)
	r := c.get("/schedule?from=2026-06-02")
	if r.status != 200 {
		t.Fatalf("schedule: %d", r.status)
	}
	b := r.body
	// axis: 8 AM at the left edge, 7 PM at the right, one line per hour
	mustHave(t, b, `<text x="0.000%" y="14" text-anchor="start">8 AM</text>`, `<text x="100.000%" y="14" text-anchor="end">7 PM</text>`,
		`<line class="sgl" x1="9.091%" x2="9.091%"`)
	// lane 1 (@sample_writer) markers sit at y=11, lane 2 (@sample_maker) at y=41
	for _, m := range []struct {
		id        int64
		cls, x, y string
	}{
		{ids["sm900"], "pe", "9.091%", "11"},   // 9:00 pending
		{ids["sm1030"], "ap", "22.727%", "11"}, // 10:30 approved
		{ids["jg905"], "ap", "9.848%", "41"},   // 9:05 approved
		{ids["jg915"], "co", "11.364%", "41"},  // 9:15 pending, too close: conflict
		{ids["jg1400"], "ap", "54.545%", "41"}, // 2:00 PM
	} {
		re := regexp.MustCompile(`<a href="/drafts/` + itoa(m.id) + `"><title>[^<]*</title><rect class="smk ` + m.cls + `" x="` + regexp.QuoteMeta(m.x) + `" y="` + m.y + `" width="2.273%" height="20"`)
		if !re.MatchString(b) {
			t.Errorf("no %s marker for draft %d at x=%s y=%s", m.cls, m.id, m.x, m.y)
		}
	}
	// labels: close posts merge, the conflict says its gap, threads say so
	mustHave(t, b, `>9:05, 9:15 AM<tspan class="sfx bad"> · 10 min gap</tspan>`, `>10:30 AM<tspan class="sfx"> · thread</tspan>`, `>2:00 PM</text>`)
	mustHave(t, b, "<b>Tue, Jun 2</b>", ">5 items<", ">0 items<", "No posts scheduled")
	if n := strings.Count(b, "No posts scheduled</text>"); n != 6 {
		t.Errorf("%d empty days, want 6", n)
	}
	// the stat cards and week buttons stay
	mustHave(t, b, "Spacing conflicts", "Tue 9:15 AM @sample_maker", "Next post", "‹ Prev", "Next ›", `href="/schedule?from=2026-05-26"`, `href="/schedule?from=2026-06-09"`)
	// legend, and no build notes
	mustHave(t, b, ">Approved<", ">Pending (posts only if approved)<", ">15-min window<", ">Conflict<", ">Posted<", "Two lanes per day:")
	for _, bad := range []string{"slice", "comes with the scheduler"} {
		if strings.Contains(strings.ToLower(b), bad) {
			t.Errorf("build note %q in the UI", bad)
		}
	}
	if m := styleAttr.FindString(b); m != "" {
		t.Errorf("inline style %q", m)
	}
}

// 9:30 and 9:45 are exactly 15 minutes apart: one label, "at minimum".
func TestScheduleAtMinimumLabel(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	for _, m := range []int{30, 45} {
		e.approveNow(e.propose(sec, sm, at(3, 9, m), "Example"))
	}
	mustHave(t, c.get("/schedule?from=2026-06-02").body, `>9:30, 9:45 AM<tspan class="sfx"> · 15 min, at minimum</tspan>`)
}

// The axis widens to whole hours when a post falls outside 8 AM to 7 PM.
func TestScheduleRangeWidens(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	early := e.propose(sec, sm, at(2, 6, 30), "Example early")
	e.propose(sec, jg, at(3, 21, 10), "Example late")
	b := c.get("/schedule?from=2026-06-02").body
	// 6 AM to 10 PM is 960 minutes; 6:30 is 3.125% in
	mustHave(t, b, `text-anchor="start">6 AM</text>`, `text-anchor="end">10 PM</text>`)
	if !regexp.MustCompile(`/drafts/` + itoa(early) + `"><title>[^<]*</title><rect class="smk pe" x="3.125%"`).MatchString(b) {
		t.Error("6:30 marker not at 3.125%")
	}
	if strings.Contains(b, ">7 PM<") && !strings.Contains(b, ">10 PM<") {
		t.Error("axis did not widen")
	}
}

// The zoom panel shows the selected day's first conflict on a 15-minute
// grid with the hint, the Move-to link into the reschedule flow, and a
// ghost "Next free" block at the suggested time.
func TestScheduleZoomConflictHint(t *testing.T) {
	_, c, ids := mockWeek(t)
	b := c.get("/schedule?from=2026-06-02").body
	mustHave(t, b, `id="zoom"`, "<h3>Tue, Jun 2 · 8:30 to 11:00 AM</h3>", "15-minute grid. Each block is a post plus its 15-minute no-post window.")
	// axis every 15 minutes
	mustHave(t, b, `text-anchor="start">8:30</text>`, `<text x="40.000%" y="14" text-anchor="middle">9:30</text>`, `text-anchor="end">11:00</text>`)
	move := "/drafts/" + itoa(ids["jg915"]) + "/reschedule?date=2026-06-02&amp;time=09:20"
	mustHave(t, b, `>10 min after 9:05. Needs 15. <a href="`+move+`"><tspan class="smv">Move to 9:20 AM</tspan></a></text>`)
	// cards: 9:05 in row 1, the conflict in row 2 (y=60), the ghost in row 3 (y=112)
	mustHave(t, b, `<rect class="scd ap" x="23.333%" y="8" width="9.333%" height="44"`, `<rect class="scd co" x="30.000%" y="60"`, `<rect class="scd gh" x="33.333%" y="112"`, ">Next free</text>")
	mustHave(t, b, `<line class="sbr" x1="23.333%" x2="30.000%" y1="59" y2="59"/>`)
	// gap text between @sample_writer's 9:00 and 10:30
	mustHave(t, b, ">90 min to next post</text>")
	// phone: banner with the same Move-to, chips for all 7 days, later list
	mustHave(t, b, "<b>1 conflict.</b> @sample_maker 9:15 AM is 10 min after 9:05. Needs 15.", `<a href="`+move+`"><b>Move to 9:20</b></a>`, "Later Tuesday")
	if n := strings.Count(b, `class="pdc`); n != 7 {
		t.Errorf("%d day chips, want 7", n)
	}
	// the Move-to link opens the existing reschedule page with the time filled in
	r := c.get(html.UnescapeString(move))
	if r.status != 200 || !strings.Contains(r.body, `value="09:20"`) || !strings.Contains(r.body, `value="2026-06-02"`) {
		t.Fatalf("reschedule prefill: %d", r.status)
	}
}

// The next free time steps past every neighbour, never earlier than the post.
func TestScheduleNextFree(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	e.approveNow(e.propose(sec, jg, at(2, 9, 5), "Example A"))
	e.approveNow(e.propose(sec, jg, at(2, 9, 20), "Example B"))
	mid := e.propose(sec, jg, at(2, 9, 15), "Example between") // 10 after A, 5 before B
	early := e.propose(sec, sm, at(2, 9, 0), "Example early")  // 10 before C
	e.approveNow(e.propose(sec, sm, at(2, 9, 10), "Example C"))
	b := c.get("/schedule?from=2026-06-02").body
	mustHave(t, b, "/drafts/"+itoa(mid)+"/reschedule?date=2026-06-02&amp;time=09:35", "Move to 9:35 AM")
	mustHave(t, b, "/drafts/"+itoa(early)+"/reschedule?date=2026-06-02&amp;time=09:25", "10 min before 9:10. Needs 15.")
	// a post sitting on its neighbour moves 15 past it
	e2 := newEnv(t, nil)
	c2, _ := e2.setupOwner()
	_, sec2 := e2.token("sample-agent")
	e2.approveNow(e2.propose(sec2, sm, at(2, 23, 50), "Example late"))
	same := e2.propose(sec2, sm, at(2, 23, 55), "Example next day")
	b2 := c2.get("/schedule?from=2026-06-02").body
	mustHave(t, b2, "/drafts/"+itoa(same)+"/reschedule?date=2026-06-03&amp;time=00:05", "Move to Wed 12:05 AM")
}

// ?day= picks the zoomed day; without it, the first day with a conflict;
// an unknown day falls back the same way; an empty day says so.
func TestScheduleDaySelection(t *testing.T) {
	e, c, _ := mockWeek(t)
	_, sec := e.token("sample-agent-2")
	e.approveNow(e.propose(sec, sm, at(4, 13, 0), "Example Thu A"))
	e.approveNow(e.propose(sec, sm, at(4, 13, 30), "Example Thu B"))
	e.approveNow(e.propose(sec, jg, at(4, 14, 0), "Example Thu C"))
	e.approveNow(e.propose(sec, jg, at(4, 8, 0), "Example Thu early"))
	for q, want := range map[string]string{
		"":                "<h3>Tue, Jun 2 · 8:30 to 11:00 AM</h3>",
		"&day=2026-07-01": "<h3>Tue, Jun 2 · 8:30 to 11:00 AM</h3>",
		"&day=2026-06-04": "<h3>Thu, Jun 4 · 12:30 to 3:00 PM</h3>", // busiest window
		"&day=2026-06-05": "<h3>Fri, Jun 5</h3>",
	} {
		b := c.get("/schedule?from=2026-06-02" + q).body
		mustHave(t, b, want)
		if q == "&day=2026-06-05" {
			mustHave(t, b, "No posts scheduled. Pick another day above.", `class="pdc on" href="/schedule?from=2026-06-02&amp;day=2026-06-05" aria-current="date"`)
		}
	}
	// days link to themselves for the zoom, keeping the week
	mustHave(t, c.get("/schedule?from=2026-06-02").body, `<a class="sdn" href="/schedule?from=2026-06-02&amp;day=2026-06-04#zoom">`)
	// Thursday's phone grid starts at its busiest post and lists the rest
	b := c.get("/schedule?from=2026-06-02&day=2026-06-04").body
	mustHave(t, b, "Earlier Thursday", "8:00 AM", "Later Thursday", "2:00 PM")
}

// Posted keeps its color; times follow the owner's zone, not a fixed one.
func TestSchedulePostedAndZone(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	if r := c.post("/settings", "/settings", url.Values{"tz": {"America/Denver"}, "cap": {"10.00"}}); r.status != 303 {
		t.Fatalf("settings: %d", r.status)
	}
	// 15:05 UTC on Jun 2 is 9:05 AM in Denver (MDT)
	id := e.propose(sec, sm, at(2, 15, 5), "Example posted")
	e.approveNow(id)
	e.markPosted(id, "1900000000000000001")
	b := c.get("/schedule?from=2026-06-02").body
	if !regexp.MustCompile(`/drafts/` + itoa(id) + `"><title>[^<]*</title><rect class="smk po" x="9.848%"`).MatchString(b) {
		t.Error("posted marker missing or not at 9:05 local")
	}
	mustHave(t, b, ">9:05 AM</text>", "<h3>Tue, Jun 2 · 8:30 to 11:00 AM</h3>", "Jun 2 to Jun 8, Mountain Time.")
	if strings.Contains(b, ">3:05 PM<") {
		t.Error("UTC time shown")
	}
}

// The queue, draft and reschedule conflict hints use the same
// earliest-clear-minute helper as the schedule: 9:20 (15 after 9:05), not
// the 15-minute grid's 9:30. The queue's Move to button moves it there.
func TestQueueHintUsesEarliestClearMinute(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	e.approveNow(e.propose(sec, jg, at(2, 9, 5), "Example approved"))
	id := e.propose(sec, jg, at(2, 9, 15), "Example too close")
	q := c.get("/queue").body
	mustHave(t, q, "Next free slot: 9:20 AM.", `<input type="hidden" name="date" value="2026-06-02"><input type="hidden" name="time" value="09:20"><button class="linkbtn">Move to 9:20 AM</button>`)
	for _, p := range []string{path(id, ""), path(id, "/reschedule")} {
		mustHave(t, c.get(p).body, "Next free: 9:20 AM.")
	}
	for _, b := range []string{q, c.get(path(id, "")).body} {
		if strings.Contains(b, "9:30 AM") {
			t.Error("hint still snaps to the 15-minute grid (9:30)")
		}
	}
	// the schedule suggests the same time
	mustHave(t, c.get("/schedule?from=2026-06-02").body, "/drafts/"+itoa(id)+"/reschedule?date=2026-06-02&amp;time=09:20")
	// pressing the queue's Move to puts it at 9:20, clear of the 9:05 post
	f := e.ownerForm(id, url.Values{"date": {"2026-06-02"}, "time": {"09:20"}, "return": {"/queue"}})
	if r := c.post("/queue", path(id, "/reschedule"), f); r.status != 303 {
		t.Fatalf("move: %d %s", r.status, r.body)
	}
	if d := e.draft(id); !d.ScheduledAt.Equal(at(2, 9, 20)) {
		t.Fatalf("moved to %v, want 09:20 UTC", d.ScheduledAt)
	}
	if strings.Contains(c.get("/queue").body, "Next free slot") {
		t.Error("still a conflict after the move")
	}
}

// The subtitle names the configured zone (friendly when known, else the
// abbreviation) and never assumes one.
func TestScheduleSubtitleZone(t *testing.T) {
	denver, _ := time.LoadLocation("America/Denver")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	for _, tc := range []struct {
		loc  *time.Location
		path string
		want string
	}{
		{time.UTC, "/schedule", "Next 7 days, UTC. 15 min minimum between posts per account."},
		{denver, "/schedule", "Next 7 days, Mountain Time. 15 min minimum between posts per account."},
		{tokyo, "/schedule", "Next 7 days, JST. 15 min minimum between posts per account."},
		{kolkata, "/schedule?from=2026-06-09", "Jun 9 to Jun 15, IST. 15 min minimum between posts per account."},
	} {
		e := newEnv(t, func(c *Config) { c.Location = tc.loc })
		c, _ := e.setupOwner()
		p := c.get(tc.path).body
		mustHave(t, p, `<div class="sub">`+tc.want+`</div>`)
		if tc.loc != denver && strings.Contains(p, "Mountain") {
			t.Errorf("%s: mentions Mountain", tc.loc)
		}
	}
	if got := zoneLabel(time.FixedZone("+0545", 20700), testNow); got != "+0545" {
		t.Errorf("fixed zone label %q", got)
	}
}
