package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// A failing Test whose error echoes the webhook URL and secret-like values
// shows and keeps only the safe part.
func TestBlogTestDetailHidesURLsAndSecrets(t *testing.T) {
	const echoed = `Post "https://hooks.example.test/blog?key=sample-signing-key-0000000000000000": 401 Unauthorized; ` +
		`sent Authorization: Bearer example-secret-value-abc123, token=zzz-unrelated-secret-999, sk_example_abcdefghijklmnopqrstuvwxyz, via hooks.example.test/blog/test`
	e := newEnv(t, func(c *Config) {
		c.BlogTest = func(context.Context, *store.Blog) (string, error) { return "", errors.New(echoed) }
	})
	c, _ := e.setupOwner()
	b := e.readyBlog("notes.example.test")
	ctx := context.Background()
	if err := e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogHookAuth, "Bearer example-secret-value-abc123"); err != nil {
		t.Fatal(err)
	}
	c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}})
	b, _ = e.st.BlogByID(ctx, b.ID)
	page := c.get("/settings/blogs?open=" + strconvI(b.ID))
	if page.status != http.StatusOK {
		t.Fatalf("settings %d", page.status)
	}
	for _, where := range []struct{ name, text string }{{"stored detail", b.TestDetail}, {"settings page", page.body}, {"logs", e.logs.String()}} {
		for _, bad := range []string{"hooks.example.test", "sample-signing-key", "example-secret-value", "zzz-unrelated-secret", "sk_example_abcdef", "?key="} {
			if strings.Contains(where.text, bad) {
				t.Errorf("%s shows %q", where.name, bad)
			}
		}
	}
	if b.TestState != store.TestFailed || !strings.Contains(b.TestDetail, "401 Unauthorized") {
		t.Fatalf("state %s detail %q", b.TestState, b.TestDetail)
	}
}

func TestScrubDetail(t *testing.T) {
	cases := map[string]string{
		"Listed posts, 200 OK in 0.6 s.":                         "Listed posts, 200 OK in 0.6 s.",
		"The guide.example.org Publisher API answered 503.":      "The guide.example.org Publisher API answered 503.",
		"GET https://api.example.test/v1/posts?x=1 returned 401": "GET [link hidden] returned 401",
		"Authorization: Bearer abc.def-ghi rejected":             "Authorization: [hidden] rejected",
		"password=hunter2 api_key: 'abc def'":                    "password=[hidden] api_key: [hidden]",
		"Check the API token.":                                   "Check the API token.",
		"example-value-0123456789abcdefghij leaked":              "[hidden] leaked",
		"Agent replied 401 Unauthorized to blog.test (example).": "Agent replied 401 Unauthorized to blog.test (example).",
	}
	for in, want := range cases {
		if got := store.ScrubDetail(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if got := store.ScrubDetail("echo s3cr3t-val here", "s3cr3t-val"); got != "echo [hidden] here" {
		t.Errorf("own secret: %q", got)
	}
}

// Banned topics are chips: a checked chip keeps its topic, Add topic adds
// (commas split, repeats dropped), and an unchecked chip is removed.
func TestBlogTopicChips(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	ctx := context.Background()
	base := url.Values{"name": {"notes.example.test"}, "site_url": {"https://notes.example.test"}, "method": {"webhook"}, "clock": {"07:00"},
		"lint_en": {"warn"}, "lint_banned": {"block"}, "lint_h1": {"warn"}}
	with := func(kv ...string) url.Values {
		v := url.Values{}
		for k, x := range base {
			v[k] = x
		}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return v
	}
	// Add topic on the new-blog form shows the form again with the chip; nothing is created.
	r := c.post("/settings/blogs/new?method=webhook", "/settings/blogs", with("topic_new", "sample topic A", "op", "addtopic"))
	if bs, _ := e.st.Blogs(ctx); r.status != http.StatusOK || len(bs) != 0 || !strings.Contains(r.body, `name="topic" value="sample topic A" checked`) {
		t.Fatalf("add topic on a new blog: %d, %d blogs", r.status, len(bs))
	}
	r = c.post("/settings/blogs/new?method=webhook", "/settings/blogs", with("topic", "sample topic A", "topic_new", "Sample Topic B, sample topic a"))
	bs, _ := e.st.Blogs(ctx)
	if r.status != http.StatusSeeOther || len(bs) != 1 || strings.Join(bs[0].Rules.Topics, "|") != "sample topic A|Sample Topic B" {
		t.Fatalf("create with chips: %d %+v", r.status, bs)
	}
	b := bs[0]
	page := c.get("/settings/blogs?open=" + strconvI(b.ID))
	for _, want := range []string{`class="tchip"`, `value="Sample Topic B" checked`, "Add topic", "data-tchip"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("settings lack %q", want)
		}
	}
	if strings.Contains(page.body, "one per line") {
		t.Error("the old textarea is still shown")
	}
	// Uncheck A (not sent), keep B, add C with the no-script Add topic button.
	r = c.post("/settings/blogs?open="+strconvI(b.ID), "/settings/blogs/"+strconvI(b.ID), with("topic", "Sample Topic B", "topic_new", "sample topic C", "op", "addtopic"))
	b, _ = e.st.BlogByID(ctx, b.ID)
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "#topics"+strconvI(b.ID)) || strings.Join(b.Rules.Topics, "|") != "Sample Topic B|sample topic C" {
		t.Fatalf("edit chips: %d %s %v", r.status, r.loc, b.Rules.Topics)
	}
}

// A blog card nests its promos as full cards with their own inline actions;
// Rewrite follows the X rules. A nested promo is not repeated standalone
// under All, and stands alone under the X filter.
func TestNestedPromoCards(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("notes.example.test")
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("notes.example.test", "sample-week", "Example opening.")))
	body := map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "Example promo: new sample post " + store.BlogURLToken}}, "promotes": bo.ID}
	pid := decode(t, e.api("POST", "/api/v1/drafts", sec, body)).ID
	ps := strconvI(pid)
	q := c.get("/queue")
	if n := strings.Count(q.body, `id="d`+ps+`"`); n != 1 {
		t.Fatalf("promo card shows %d times under All", n)
	}
	nest := q.body[strings.Index(q.body, `class="promos pnest"`):]
	nest = nest[:strings.Index(nest, "</section>")]
	for _, want := range []string{"qnest", `id="d` + ps + `"`, `action="/drafts/` + ps + `/approve"`, `action="/drafts/` + ps + `/reject"`,
		`href="/drafts/` + ps + `/reschedule"`, `href="/drafts/` + ps + `"`, "Promo · ", "2h after the blog", "lacks drafts:revise"} {
		if !strings.Contains(nest, want) {
			t.Errorf("nested promo lacks %q", want)
		}
	}
	if strings.Contains(nest, "Promotes: ") {
		t.Error("a nested promo repeats its Promotes chip")
	}
	// The blog card itself has no Rewrite (blog rewrite is a later slice).
	blogCard := q.body[strings.Index(q.body, `id="d`+strconvI(bo.ID)+`"`):strings.Index(q.body, `class="promos pnest"`)]
	if strings.Contains(blogCard, "/rewrite") || strings.Contains(blogCard, ">Rewrite<") {
		t.Error("blog card offers Rewrite")
	}
	x := c.get("/queue?kind=x")
	if strings.Count(x.body, `id="d`+ps+`"`) != 1 || strings.Contains(x.body, "qnest") || !strings.Contains(x.body, "Promotes: ") {
		t.Fatal("promo is not standalone under the X filter")
	}
	// A nested promo approves from its inline form and lands back on the queue.
	if r := c.post("/queue", "/drafts/"+ps+"/approve", e.ownerForm(pid, nil)); r.status != http.StatusSeeOther {
		t.Fatalf("nested approve: %d", r.status)
	}
	if d := e.draft(pid); d.Status != store.StatusApproved {
		t.Fatalf("promo %s after approve", d.Status)
	}
}

// The calendar marks a promo waiting on its blog, filters All/X/Blog (kept
// on Prev/Next), shows blog and promo stats, and ticks the banner at the
// publish time in the configured zone.
func TestCalendarBlogs1c(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	if r := c.post("/settings", "/settings", url.Values{"tz": {"Pacific/Auckland"}, "cap": {"10.00"}}); r.status != http.StatusSeeOther {
		t.Fatalf("tz: %d", r.status)
	}
	e.readyBlog("notes.example.test")
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("notes.example.test", "sample-week", "Example opening.")))
	body := map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "Example promo " + store.BlogURLToken}}, "promotes": bo.ID}
	pid := decode(t, e.api("POST", "/api/v1/drafts", sec, body)).ID
	xid := e.propose(sec, sm, e.clock().Add(30*time.Hour), "Example plain post")
	e.approveNow(pid)
	bd := e.draft(bo.ID)
	auck, _ := time.LoadLocation("Pacific/Auckland")
	at := bd.ProposedAt.In(auck)
	if at.Hour() != 7 || at.Minute() != 0 {
		t.Fatalf("blog proposed %s, want 7:00 in the configured zone", at)
	}
	day := "?day=" + at.Format("2006-01-02")
	s := c.get("/schedule" + day)
	for _, want := range []string{`class="bbtk"`, `class="bban pending btk bl`, "Waiting on blog", `fill="url(#xh-wb)"`, ">Blog posts<", ">Promos<",
		`class="card stat warn"`, `aria-label="Show"`, "kind=blog", "kind=x"} {
		if !strings.Contains(s.body, want) {
			t.Errorf("schedule lacks %q", want)
		}
	}
	// The axis starts an hour before the 7:00 tick, so the tick is not at the edge.
	if !strings.Contains(s.body, ">6 AM<") || strings.Contains(s.body, `<rect x="0.000%"`) {
		t.Errorf("axis does not start an hour before the tick")
	}
	xs := c.get("/schedule" + day + "&kind=x")
	if strings.Contains(xs.body, `class="bban`) || !strings.Contains(xs.body, "/drafts/"+strconvI(xid)) {
		t.Error("X filter shows blog banners")
	}
	bs := c.get("/schedule" + day + "&kind=blog")
	if !strings.Contains(bs.body, `class="bban`) || strings.Contains(bs.body, "/drafts/"+strconvI(xid)+`"`) {
		t.Error("Blog filter should keep banners and promos only")
	}
	if !strings.Contains(bs.body, "/drafts/"+strconvI(pid)) {
		t.Error("Blog filter dropped the promo")
	}
	for _, h := range []string{"Previous week", "Next week"} {
		i := strings.Index(bs.body, `aria-label="`+h+`"`)
		if i < 0 {
			t.Fatalf("no %s link", h)
		}
		a := bs.body[strings.LastIndex(bs.body[:i], "<a "):i]
		if !strings.Contains(a, "kind=blog") {
			t.Errorf("%s drops the filter: %s", h, a)
		}
	}
}

func TestBlogTickPad(t *testing.T) {
	for _, tc := range []struct {
		min, lo, span int
		pad           string
	}{{420, 360, 900, "bl6"}, {1200, 360, 900, "br7"}, {360, 360, 900, "bl0"}, {900, 360, 900, "bl60"}, {910, 360, 900, "br39"}} {
		b := schedBlog{Min: tc.min}
		blogTick(&b, tc.lo, tc.span)
		if b.Pad != tc.pad || b.TickX != pctAt(tc.min, tc.lo, tc.span) {
			t.Errorf("%d on %d+%d: pad %s x %s, want %s", tc.min, tc.lo, tc.span, b.Pad, b.TickX, tc.pad)
		}
	}
}

// Nested promos follow the status tab: a blog card nests only the promos on
// the tab being shown. A promo on another tab is not nested; it shows on
// its own tab (nested there when its blog is on that tab too, standalone
// otherwise), and the blog card links to it.
func TestNestedPromosFollowTab(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("notes.example.test")
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("notes.example.test", "sample-week", "Example opening.")))
	promo := func(text string) int64 {
		body := map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": text + " " + store.BlogURLToken}}, "promotes": bo.ID}
		return decode(t, e.api("POST", "/api/v1/drafts", sec, body)).ID
	}
	apID := promo("Example approved promo")
	nestOf := func(body string) string {
		i := strings.Index(body, `class="promos pnest"`)
		if i < 0 {
			return ""
		}
		n := body[i:]
		return n[:strings.Index(n, "</section>")]
	}

	// No promo on the tab: the card links to it and never falls back to the
	// compact list of every promo.
	e.approveNow(apID)
	q := c.get("/queue")
	n := nestOf(q.body)
	if !strings.Contains(n, "<b>Promo posts") || !strings.Contains(n, "On other tabs: ") || !strings.Contains(n, `>1 approved</a>`) ||
		strings.Contains(n, "qnest") || strings.Contains(q.body, `id="d`+strconvI(apID)+`"`) || strings.Contains(q.body, "Example approved promo") {
		t.Fatalf("pending tab, no promo on it: %s", n)
	}

	// Blog pending, one promo approved, one pending.
	peID := promo("Example pending promo")
	ap, pe, bs := `id="d`+strconvI(apID)+`"`, `id="d`+strconvI(peID)+`"`, `id="d`+strconvI(bo.ID)+`"`
	q = c.get("/queue")
	n = nestOf(q.body)
	if !strings.Contains(q.body, bs) || !strings.Contains(n, pe) || strings.Contains(q.body, ap) {
		t.Fatalf("pending tab: blog %v, pending nested %v, approved shown %v", strings.Contains(q.body, bs), strings.Contains(n, pe), strings.Contains(q.body, ap))
	}
	if !strings.Contains(n, "1 promo post<") || !strings.Contains(n, "Also on other tabs: ") || !strings.Contains(n, `<a href="/queue?status=approved#d`+strconvI(apID)+`">1 approved</a>`) {
		t.Errorf("pending tab other-tab link missing: %s", n)
	}
	// The approved promo stands alone on Approved (its blog is not there).
	a := c.get("/queue?status=approved")
	if strings.Count(a.body, ap) != 1 || strings.Contains(a.body, "qnest") || strings.Contains(a.body, bs) || strings.Contains(a.body, pe) || !strings.Contains(a.body, "Promotes: ") {
		t.Fatal("approved tab: the approved promo is not standalone, or the pending one shows")
	}

	// Blog approved: on Approved the blog nests only the approved promo.
	e.approveNow(bo.ID)
	a = c.get("/queue?status=approved")
	n = nestOf(a.body)
	if !strings.Contains(a.body, bs) || !strings.Contains(n, ap) || strings.Contains(a.body, pe) {
		t.Fatalf("approved tab: blog %v, approved nested %v, pending shown %v", strings.Contains(a.body, bs), strings.Contains(n, ap), strings.Contains(a.body, pe))
	}
	if strings.Count(a.body, ap) != 1 || !strings.Contains(n, `<a href="/queue#d`+strconvI(peID)+`">1 pending</a>`) {
		t.Errorf("approved tab: approved promo count %d, or no link to the pending one", strings.Count(a.body, ap))
	}
	// The pending promo stands alone on Pending; the blog is not there.
	q = c.get("/queue")
	if strings.Count(q.body, pe) != 1 || strings.Contains(q.body, "qnest") || strings.Contains(q.body, bs) || strings.Contains(q.body, ap) || !strings.Contains(q.body, "Promotes: ") {
		t.Fatal("pending tab: the pending promo is not standalone")
	}
	// Same under the Blog filter: nested promos follow the tab, and the link
	// goes to All, where the other promo shows.
	b := c.get("/queue?kind=blog&status=approved")
	if n := nestOf(b.body); !strings.Contains(n, ap) || strings.Contains(b.body, pe) || !strings.Contains(n, `href="/queue#d`+strconvI(peID)+`"`) {
		t.Fatal("blog filter, approved tab: nesting does not follow the tab")
	}
	if b := c.get("/queue?kind=blog"); strings.Contains(b.body, ap) || strings.Contains(b.body, pe) {
		t.Fatal("blog filter, pending tab: a promo shows without its blog")
	}

	// Reject the pending promo: Approved links to it on Rejected, where it stands alone.
	if r := c.post("/queue", "/drafts/"+strconvI(peID)+"/reject", e.ownerForm(peID, url.Values{"reason": {"Example reason"}})); r.status != http.StatusSeeOther {
		t.Fatalf("reject: %d", r.status)
	}
	if n := nestOf(c.get("/queue?status=approved").body); !strings.Contains(n, `<a href="/queue?status=rejected#d`+strconvI(peID)+`">1 rejected</a>`) {
		t.Errorf("approved tab: no link to the rejected promo: %s", n)
	}
	if r := c.get("/queue?status=rejected"); strings.Count(r.body, pe) != 1 || strings.Contains(r.body, "qnest") {
		t.Fatal("rejected tab: the rejected promo is not standalone")
	}

}
