package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// markPosted takes an approved draft through the store's posting steps
// (claim, one log row per part with an invented post id, finish), as the
// dispatcher would, with no poster and no X anywhere.
func (e *env) markPosted(id int64, ids ...string) {
	e.t.Helper()
	ctx := context.Background()
	d := e.draft(id)
	if ok, err := e.st.Claim(ctx, store.Due{ID: id, Version: d.Version, Status: d.Status, Account: d.Account}, e.clock().Add(time.Minute)); err != nil || !ok {
		e.t.Fatalf("claim %d: %v %v", id, ok, err)
	}
	for i, pid := range ids {
		key := fmt.Sprintf("test-%d-%d", id, i+1)
		if err := e.st.BeginPart(ctx, store.LogEntry{DraftID: id, Account: d.Account, Part: i + 1, Parts: len(ids), IdemKey: key, Preview: "Example"}); err != nil {
			e.t.Fatal(err)
		}
		if err := e.st.FinishPart(ctx, key, store.Outcome{Status: store.LogPosted, PostID: pid}); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.st.FinishDraft(ctx, id, store.StatusPosted, ""); err != nil {
		e.t.Fatal(err)
	}
}

// cardIDs lists the draft ids of the queue cards in page order.
func cardIDs(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`<article class="card qcard[^"]*" id="d(\d+)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func chipCount(t *testing.T, body, label string) string {
	t.Helper()
	m := regexp.MustCompile(`<nav class="seg segst" aria-label="Status">.*?>` + label + ` <span class="c t">(\d+)</span>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s chip", label)
	}
	return m[1]
}

// The Posted chip lists posted drafts, newest first by when they posted
// (not by schedule or id), and its count matches the list, per account too.
func TestPostedTabNewestFirstAndCounts(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	a := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example A")
	b := e.propose(sec, sm, testNow.Add(3*time.Hour), "Example B")
	cc := e.propose(sec, jg, testNow.Add(4*time.Hour), "Example C")
	pend := e.propose(sec, sm, testNow.Add(5*time.Hour), "Example pending")
	for _, id := range []int64{a, b, cc} {
		e.approveNow(id)
	}
	e.markPosted(b, "1001")
	e.advance(time.Minute)
	e.markPosted(cc, "1002")
	e.advance(time.Minute)
	e.markPosted(a, "1003")

	r := c.get("/queue?status=posted")
	if r.status != http.StatusOK {
		t.Fatalf("posted tab: %d", r.status)
	}
	want := []string{strconvI(a), strconvI(cc), strconvI(b)}
	if got := cardIDs(r.body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("posted order %v, want %v (newest posted first)", got, want)
	}
	if n := chipCount(t, r.body, "Posted"); n != "3" {
		t.Fatalf("Posted chip %s, want 3", n)
	}
	if !strings.Contains(r.body, `href="/queue?status=posted" class="on" aria-current="true">Posted`) {
		t.Fatal("Posted chip not marked current")
	}
	if strings.Contains(r.body, `id="d`+strconvI(pend)+`"`) {
		t.Fatal("a pending draft is on the Posted tab")
	}
	// Per account: count and list agree.
	r = c.get("/queue?status=posted&account=" + sm)
	if got := cardIDs(r.body); strings.Join(got, ",") != strconvI(a)+","+strconvI(b) || chipCount(t, r.body, "Posted") != "2" {
		t.Fatalf("@%s posted: %v, chip %s", sm, got, chipCount(t, r.body, "Posted"))
	}
	// Posted drafts are not on the other tabs; the pending tab is unchanged.
	r = c.get("/queue")
	if got := cardIDs(r.body); strings.Join(got, ",") != strconvI(pend) || chipCount(t, r.body, "Pending") != "1" || chipCount(t, r.body, "Approved") != "0" {
		t.Fatalf("pending tab: %v", got)
	}
	if strings.Contains(r.body, "Not in these tabs") {
		t.Fatal("pointer shown with nothing posting or failed")
	}
}

// Every part of a thread links its live post, built from the stored id and
// the handle. "View in xstats" appears only with XPOST_XSTATS_URL.
func TestPostedLinksWithAndWithoutXstats(t *testing.T) {
	for _, base := range []string{"", "https://stats.example.com/"} {
		t.Run("xstats="+base, func(t *testing.T) {
			e := newEnv(t, func(c *Config) { c.XstatsURL = base })
			c, _ := e.setupOwner()
			_, sec := e.token("sample-agent")
			id := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example one", "Example two", "Example three")
			e.approveNow(id)
			e.markPosted(id, "1900000000000000001", "1900000000000000002", "1900000000000000003")
			body := c.get("/queue?status=posted").body
			for i := 1; i <= 3; i++ {
				x := fmt.Sprintf(`<a href="https://x.com/%s/status/190000000000000000%d" target="_blank" rel="noopener noreferrer">View on X`, sm, i)
				if !strings.Contains(body, x) {
					t.Fatalf("part %d: no X link %s", i, x)
				}
				if !strings.Contains(body, fmt.Sprintf(`<span class="xl">Part %d</span>`, i)) {
					t.Fatalf("part %d: no label", i)
				}
				s := fmt.Sprintf(`<a href="https://stats.example.com/#post/190000000000000000%d" target="_blank" rel="noopener noreferrer">View in xstats`, i)
				if base == "" && strings.Contains(body, "xstats") {
					t.Fatal("xstats link shown with XPOST_XSTATS_URL unset")
				}
				if base != "" && !strings.Contains(body, s) {
					t.Fatalf("part %d: no xstats link %s", i, s)
				}
			}
			// Read-only: View only, no Edit, Approve, Reject or Delete.
			for _, bad := range []string{"/approve", "/reject", "/delete", "/reschedule", ">Edit<"} {
				if strings.Contains(body, bad) {
					t.Fatalf("posted card offers %s", bad)
				}
			}
			if !strings.Contains(body, `<a class="btn" href="/drafts/`+strconvI(id)+`">View</a>`) || !strings.Contains(body, `<div class="sect">Posted</div>`) {
				t.Fatal("posted card has no View link or Posted time")
			}
		})
	}
}

func TestParseXstatsURL(t *testing.T) {
	ok := map[string]string{
		"":                                 "",
		"  ":                               "",
		"https://stats.example.com":        "https://stats.example.com",
		"https://stats.example.com/":       "https://stats.example.com",
		" https://stats.example.com// ":    "https://stats.example.com",
		"https://stats.example.com:8443/x": "https://stats.example.com:8443/x",
		"http://localhost:8080/":           "http://localhost:8080",
		"http://127.0.0.1:8080":            "http://127.0.0.1:8080",
	}
	for in, want := range ok {
		got, err := ParseXstatsURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"stats.example.com", "http://stats.example.com", "ftp://stats.example.com", "javascript:alert(1)", "data:text/html,x",
		"https://", "https:///x", "https://user:pw@stats.example.com", "https://user@stats.example.com",
		"https://stats.example.com/?x=1", "https://stats.example.com/?", "https://stats.example.com/#post/1", "https://stats.example.com/#",
		"https://stats.example.com/a b", "https://stats.example.com/\"onmouseover=\"x", "https://stats.example.com/<script>", "https://stats.example.com/a\nb", "https://stats.example.com/\x7f",
		"//stats.example.com", "/relative", "https://stats.example.com:port",
	} {
		if got, err := ParseXstatsURL(in); err == nil || got != "" {
			t.Errorf("%q accepted as %q", in, got)
		} else if !strings.Contains(err.Error(), "XPOST_XSTATS_URL") || strings.Contains(err.Error(), "pw") {
			t.Errorf("%q: error %q", in, err)
		}
	}
}

func TestNewRefusesBadXstatsURL(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := New(e.st, Config{BaseURL: "https://xpost.example.test", XstatsURL: "http://stats.example.com"}); err == nil || !strings.Contains(err.Error(), "XPOST_XSTATS_URL") {
		t.Fatalf("New accepted an http xstats URL: %v", err)
	}
}

// Ids and handles come from the database; hostile ones are escaped, never
// linked as given, and never reach an href or the xstats hash.
func TestPostedEscapesHostileIDsAndHandles(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.XstatsURL = "https://stats.example.com" })
	ctx := context.Background()
	handle := `ev"il<script>alert(1)</script>`
	if _, err := e.st.DB.ExecContext(ctx, `INSERT INTO accounts(handle, initials, color, position) VALUES (?, 'EV', 'sm', 9)`, handle); err != nil {
		t.Fatal(err)
	}
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	hostileIDs := []string{`1" onmouseover="alert(1)`, `javascript:alert(1)`, `123/../../evil`, `<img src=x onerror=alert(1)>`, `12#post/34`, `１２３`}
	parts := make([]string, len(hostileIDs)+1)
	for i := range parts {
		parts[i] = fmt.Sprintf("Example part %d", i+1)
	}
	id := e.propose(sec, handle, testNow.Add(2*time.Hour), parts...)
	e.approveNow(id)
	e.markPosted(id, append(hostileIDs, "1900000000000000009")...)
	body := c.get("/queue?status=posted").body
	for _, raw := range []string{"<script>alert(1)", `onmouseover="alert`, "<img src=x", "javascript:alert", "../../evil", "#post/12#"} {
		if strings.Contains(body, raw) {
			t.Fatalf("hostile value rendered raw: %s", raw)
		}
	}
	if !strings.Contains(body, "@ev&#34;il&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("hostile handle not shown escaped")
	}
	if n := strings.Count(body, "No post id recorded"); n != len(hostileIDs) {
		t.Fatalf("%d parts without a usable id, want %d", n, len(hostileIDs))
	}
	// The one good id still links: the handle X would never issue is left
	// out of the path (handle-free /i/status/ form), and xstats gets digits.
	if !strings.Contains(body, `href="https://x.com/i/status/1900000000000000009"`) || !strings.Contains(body, `href="https://stats.example.com/#post/1900000000000000009"`) {
		t.Fatal("good id not linked")
	}
	for _, m := range regexp.MustCompile(`href="([^"]*)"`).FindAllStringSubmatch(body, -1) {
		if u, err := url.Parse(strings.ReplaceAll(m[1], "&amp;", "&")); err != nil || (u.Scheme != "" && u.Scheme != "https") {
			t.Fatalf("unexpected href %q", m[1])
		}
	}
}

func TestXURLAndStatsURLShapes(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.XstatsURL = "https://stats.example.com/sub/" })
	s := e.srv
	cases := []struct{ handle, id, x, stats string }{
		{"sample_writer", "123", "https://x.com/sample_writer/status/123", "https://stats.example.com/sub/#post/123"},
		{"a_b9", "0042", "https://x.com/a_b9/status/0042", "https://stats.example.com/sub/#post/0042"},
		{"toolonghandle_1234", "5", "https://x.com/i/status/5", "https://stats.example.com/sub/#post/5"},
		{"a/b", "5", "https://x.com/i/status/5", "https://stats.example.com/sub/#post/5"},
		{"ok", "", "", ""},
		{"ok", "12a", "", ""},
		{"ok", strings.Repeat("9", 31), "", ""},
	}
	for _, c := range cases {
		if got := s.xURL(c.handle, c.id, false); got != c.x {
			t.Errorf("xURL(%q,%q) = %q, want %q", c.handle, c.id, got, c.x)
		}
		if got := s.statsURL(c.id); got != c.stats {
			t.Errorf("statsURL(%q) = %q, want %q", c.id, got, c.stats)
		}
	}
	if got := s.xURL("sample_writer", "7", true); got != "https://x.invalid/sample_writer/status/7" {
		t.Errorf("demo row link %q", got)
	}
}

// Posted drafts are read-only for the owner, and agents keep exactly the
// rights they had (they could never touch a posted draft).
func TestPostedIsReadOnly(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	at := testNow.Add(2 * time.Hour)
	id := e.propose(sec, sm, at, "Example post")
	e.approveNow(id)
	e.markPosted(id, "1900000000000000001")
	before := e.draft(id)
	page := "/drafts/" + strconvI(id)
	for _, p := range []struct {
		path string
		form url.Values
	}{
		{page, editForm("save", "Changed", at)},
		{page + "/approve", nil},
		{page + "/reject", url.Values{"reason": {"x"}}},
		{page + "/reschedule", url.Values{"op": {"move"}, "date": {at.Format("2006-01-02")}, "time": {"18:00"}}},
		{page + "/delete", nil},
	} {
		r := c.post("/queue?status=posted", p.path, e.ownerForm(id, p.form))
		if r.status == http.StatusSeeOther || r.status == http.StatusOK && !strings.Contains(r.body, "can't") {
			t.Fatalf("%s on a posted draft: %d %s", p.path, r.status, r.loc)
		}
	}
	after := e.draft(id)
	if after.Status != store.StatusPosted || after.Version != before.Version || after.Parts[0].Text != "Example post" {
		t.Fatalf("posted draft changed: %+v", after)
	}
	if r := e.api("DELETE", "/api/v1/drafts/"+strconvI(id), sec, nil); r.status < 400 {
		t.Fatalf("agent deleted a posted draft: %d", r.status)
	}
	if r := e.api("PATCH", "/api/v1/drafts/"+strconvI(id), sec, map[string]any{"parts": []map[string]string{{"text": "x"}}}); r.status < 400 {
		t.Fatalf("agent patched a posted draft: %d", r.status)
	}
	if r := e.api("GET", "/queue?status=posted", sec, nil); r.status == http.StatusOK && strings.Contains(r.body, "View on X") {
		t.Fatal("an agent token read the review queue")
	}
	if e.draft(id).Status != store.StatusPosted {
		t.Fatal("agent changed a posted draft")
	}
}

// Posting and failed (including needs-review) drafts have no queue tab:
// the queue points to the Log page, and the Log page lists them.
func TestQueuePointsToLogForPostingAndFailed(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	ctx := context.Background()
	_, sec := e.token("sample-agent")
	claim := func(id int64) {
		d := e.draft(id)
		if ok, err := e.st.Claim(ctx, store.Due{ID: id, Version: d.Version, Status: d.Status, Account: d.Account}, e.clock().Add(time.Minute)); err != nil || !ok {
			t.Fatal(err)
		}
	}
	begin := func(id int64, key string) {
		if err := e.st.BeginPart(ctx, store.LogEntry{DraftID: id, Account: sm, Part: 1, Parts: 1, IdemKey: key, Preview: "Example in flight " + key}); err != nil {
			t.Fatal(err)
		}
	}
	posting := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example posting")
	failed := e.propose(sec, sm, testNow.Add(3*time.Hour), "Example failed")
	unknown := e.propose(sec, sm, testNow.Add(4*time.Hour), "Example unknown")
	for _, id := range []int64{posting, failed, unknown} {
		e.approveNow(id)
		claim(id)
	}
	begin(posting, "k-posting")
	begin(failed, "k-failed")
	if err := e.st.FinishPart(ctx, "k-failed", store.Outcome{Status: store.LogFailed, Error: "example refusal"}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishDraft(ctx, failed, store.StatusFailed, "Part 1 of 1 failed: example refusal"); err != nil {
		t.Fatal(err)
	}
	begin(unknown, "k-unknown")
	if _, err := e.st.Interrupted(ctx, unknown); err != nil {
		t.Fatal(err)
	}
	if err := e.st.FinishDraft(ctx, unknown, store.StatusFailed, store.ReviewMessage([]int{1})); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"/queue", "/queue?status=approved", "/queue?status=posted"} {
		body := c.get(q).body
		if !strings.Contains(body, `Not in these tabs: <a href="/log">1 posting</a> and <a href="/log?status=failed">2 failed or needing review</a>. Find them on the Log page.`) {
			t.Fatalf("%s: no pointer to the Log page", q)
		}
		for _, id := range []int64{posting, failed, unknown} {
			if strings.Contains(body, `id="d`+strconvI(id)+`"`) {
				t.Fatalf("%s lists draft %d", q, id)
			}
		}
	}
	log := c.get("/log").body
	for _, want := range []string{"Example in flight k-posting", `<span class="st posting">posting</span>`, "Example in flight k-failed", `<span class="st failed">failed</span>`, "Example in flight k-unknown", `<span class="st unknown">needs review</span>`} {
		if !strings.Contains(log, want) {
			t.Fatalf("Log page is missing %q", want)
		}
	}
	failedLog := c.get("/log?status=failed").body
	if !strings.Contains(failedLog, "k-failed") || !strings.Contains(failedLog, "k-unknown") || strings.Contains(failedLog, "k-posting") {
		t.Fatal("Log failed filter does not list the failed and needs-review drafts")
	}
}
