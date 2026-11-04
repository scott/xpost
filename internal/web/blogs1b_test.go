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

func TestBlogSettings(t *testing.T) {
	var tested []string
	e := newEnv(t, func(c *Config) {
		c.BlogTest = func(_ context.Context, b *store.Blog) (string, error) {
			tested = append(tested, b.Name)
			if !b.HasConnection() {
				return "", errors.New("Add the connection details first.")
			}
			return "Test passed. Nothing was published.", nil
		}
	})
	c, _ := e.setupOwner()
	ctx := context.Background()

	// Step 1 picks the method, step 2 is the form.
	if r := c.get("/settings/blogs/new"); r.status != 200 || !strings.Contains(r.body, "Publisher API") || !strings.Contains(r.body, "Continue") {
		t.Fatalf("step 1: %d", r.status)
	}
	if r := c.get("/settings/blogs/new?method=webhook"); r.status != 200 || !strings.Contains(r.body, "Publish webhook URL") || !strings.Contains(r.body, "Untested") {
		t.Fatalf("step 2: %d", r.status)
	}
	const hookURL, key = "https://hooks.example.test/sample-blog-path", "sample-signing-key-1111111111111111"
	form := url.Values{"name": {"notes.example.test"}, "site_url": {"https://notes.example.test"}, "method": {"webhook"}, "clock": {"07:00"}, "promo": {"sample_writer"},
		"topics": {"sample topic A\nsample topic B"}, "lint_en": {"warn"}, "lint_banned": {"block"}, "lint_h1": {"warn"}, "hook_url": {hookURL}, "hook_key": {key}}
	r := c.post("/settings/blogs/new?method=webhook", "/settings/blogs", form)
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=added") {
		t.Fatalf("create: %d %s %s", r.status, r.loc, r.body)
	}
	bs, _ := e.st.Blogs(ctx)
	if len(bs) != 1 || bs[0].TestState != store.TestUntested || !bs[0].HasHookURL || !bs[0].HasHookKey || len(bs[0].Rules.Topics) != 2 || bs[0].PromoAccount != "sample_writer" {
		t.Fatalf("created %+v", bs)
	}
	b := bs[0]
	page := c.get("/settings/blogs?open=" + strconvI(b.ID))
	for _, secret := range []string{hookURL, key, "sample-blog-path"} {
		if strings.Contains(page.body, secret) {
			t.Fatal("a sealed value is rendered in Settings")
		}
	}
	for _, want := range []string{"notes.example.test", "Untested", "cannot publish until its last test passes", "Remove blog", "Webhook to agent"} {
		if !strings.Contains(page.body, want) {
			t.Errorf("settings lack %q", want)
		}
	}

	// Test: a dry run that passes, then the blog is Ready.
	r = c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"test"}})
	if r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=tested") || len(tested) != 1 {
		t.Fatalf("test: %d %s %v", r.status, r.loc, tested)
	}
	if b, _ = e.st.BlogByID(ctx, b.ID); !b.Ready() {
		t.Fatal("not ready after a passed test")
	}
	// Saving the same connection keeps it Ready; a new Site URL resets it.
	form.Del("hook_url")
	form.Del("hook_key")
	r = c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), form)
	if b, _ = e.st.BlogByID(ctx, b.ID); r.status != http.StatusSeeOther || !b.Ready() || !b.HasHookURL {
		t.Fatalf("save unchanged: %d ready=%v", r.status, b.Ready())
	}
	form.Set("site_url", "https://notes2.example.test")
	c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), form)
	if b, _ = e.st.BlogByID(ctx, b.ID); b.Ready() {
		t.Fatal("a connection change should reset the test")
	}
	// A bad value is refused with the form kept.
	form.Set("site_url", "http://insecure.example.test")
	if r = c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), form); r.status != http.StatusBadRequest || !strings.Contains(r.body, "https://") {
		t.Fatalf("bad site url: %d", r.status)
	}
	// Clear a sealed value.
	c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"clear-hook_key"}})
	if b, _ = e.st.BlogByID(ctx, b.ID); b.HasHookKey {
		t.Fatal("clear")
	}

	// Remove is blocked while the blog has posts waiting.
	e.st.SetBlogSecret(ctx, e.owner, b.ID, store.BlogHookKey, key)
	e.st.RecordBlogTest(ctx, e.owner, b.ID, true, "ok", 1)
	_, sec := e.token("sample-agent")
	if o := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("notes.example.test", "sample-week", "Example opening."))); o.ID == 0 {
		t.Fatal("intake")
	}
	r = c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"remove"}})
	if r.status != http.StatusConflict || !strings.Contains(r.body, "remove notes.example.test yet") || !strings.Contains(r.body, "View them in the queue") || !strings.Contains(r.body, "1 post is pending") {
		t.Fatalf("remove blocked: %d %s", r.status, r.body[strings.Index(r.body, "<main"):])
	}
	// "View them in the queue" opens the queue filtered to this one blog.
	other := e.readyBlog("other.example.test")
	if o := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody(other.Name, "sample-other", "Example opening."))); o.ID == 0 {
		t.Fatal("intake on the other blog")
	}
	link := "/queue?kind=blog&amp;blog=" + strconvI(b.ID)
	if !strings.Contains(r.body, `href="`+link+`"`) {
		t.Fatalf("queue link not filtered to the blog: %s", r.body[strings.Index(r.body, "pact"):])
	}
	q := c.get(strings.ReplaceAll(link, "&amp;", "&"))
	if q.status != http.StatusOK || !strings.Contains(q.body, "/sample-week") || strings.Contains(q.body, "/sample-other") || !strings.Contains(q.body, "Showing only <b>notes.<wbr>example.<wbr>test</b>") {
		t.Fatalf("one-blog queue: %d", q.status)
	}
	if all := c.get("/queue?kind=blog"); !strings.Contains(all.body, "/sample-week") || !strings.Contains(all.body, "/sample-other") || strings.Contains(all.body, "Showing only") {
		t.Fatal("the Blog view should list both blogs")
	}
	// Status chips keep the one-blog filter.
	if !strings.Contains(q.body, "/queue?blog="+strconvI(b.ID)+"&amp;kind=blog&amp;status=approved") {
		t.Fatal("status chips drop the blog filter")
	}
	// Only approved posts left: the link opens the Approved tab.
	bd, _ := e.st.Queue(ctx, store.Filter{Status: store.StatusPending, Account: b.Account, Kind: store.KindBlog})
	if len(bd) != 1 {
		t.Fatalf("pending on the blog: %d", len(bd))
	}
	if err := e.st.Approve(ctx, e.owner, bd[0].ID, bd[0].Version); err != nil {
		t.Fatal(err)
	}
	r = c.post("/settings/blogs", "/settings/blogs/"+strconvI(b.ID), url.Values{"op": {"remove"}})
	if !strings.Contains(r.body, `href="`+link+`&amp;status=approved"`) {
		t.Fatalf("approved-only link: %s", r.body[strings.Index(r.body, "pact"):])
	}

	// Without a publisher the Test button is off.
	e2 := newEnv(t, nil)
	c2, _ := e2.setupOwner()
	b2 := e2.readyBlog("example.test")
	if p := c2.get("/settings/blogs"); !strings.Contains(p.body, "Testing is not available on this server yet") {
		t.Fatal("no publisher note")
	}
	if r := c2.post("/settings/blogs", "/settings/blogs/"+strconvI(b2.ID), url.Values{"op": {"test"}}); r.status != http.StatusConflict {
		t.Fatalf("test without a publisher: %d", r.status)
	}
	// Removing an idle blog works.
	if r := c2.post("/settings/blogs", "/settings/blogs/"+strconvI(b2.ID), url.Values{"op": {"remove"}}); r.status != http.StatusSeeOther {
		t.Fatalf("remove: %d %s", r.status, r.body)
	}
	if bs, _ := e2.st.Blogs(ctx); len(bs) != 0 {
		t.Fatal("removed blog still listed")
	}
}

func TestPromoIntakeAndCards(t *testing.T) {
	e := newEnv(t, nil)
	_, sec := e.token("sample-agent")
	c, _ := e.setupOwner()
	e.readyBlog("example.test")
	bo := decodeBlog(t, e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "sample-week", "Example opening.")))
	body := map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "Example promo: new sample post " + store.BlogURLToken}}, "promotes": bo.ID}
	r := e.api("POST", "/api/v1/drafts", sec, body)
	if r.status != http.StatusCreated || !strings.Contains(r.body, `"promotes":`) || !strings.Contains(r.body, `"promo_offset_minutes":120`) {
		t.Fatalf("promo intake: %d %s", r.status, r.body)
	}
	pid := decode(t, r).ID
	// Without promotes, {{blog_url}} is refused; so is an offset with a fixed time.
	bad := map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "x " + store.BlogURLToken}}, "proposed_at": testNow.Add(time.Hour).Format(time.RFC3339)}
	if r := e.api("POST", "/api/v1/drafts", sec, bad); r.status != http.StatusBadRequest {
		t.Fatalf("blog_url without promotes: %d", r.status)
	}
	bad = map[string]any{"account": "sample_writer", "parts": []map[string]any{{"text": "x"}}, "promotes": bo.ID, "promo_offset_minutes": 30, "proposed_at": testNow.Add(time.Hour).Format(time.RFC3339)}
	if r := e.api("POST", "/api/v1/drafts", sec, bad); r.status != http.StatusBadRequest {
		t.Fatalf("offset and fixed time: %d", r.status)
	}
	q := c.get("/queue?kind=x")
	for _, want := range []string{"Promotes: Example post: a sample week", `class="blink"`, "blog link", "2h after the blog"} {
		if !strings.Contains(q.body, want) {
			t.Errorf("promo card lacks %q", want)
		}
	}
	if strings.Contains(q.body, store.BlogURLToken) {
		t.Fatal("the raw placeholder shows on the card")
	}
	bq := c.get("/queue?kind=blog")
	if !strings.Contains(bq.body, "1 promo post") || !strings.Contains(bq.body, `id="d`+strconvI(pid)+`"`) {
		t.Fatal("blog card lacks its promo cards")
	}
	// Approved early, then held when the publish fails.
	e.approveNow(pid)
	if q = c.get("/queue?kind=x&status=approved"); !strings.Contains(q.body, "Posts after the blog is live.") {
		t.Fatal("approved early note")
	}
	ctx := context.Background()
	bd := e.draft(bo.ID)
	if err := e.st.PublishNow(ctx, e.owner, bd.ID, bd.Version); err != nil {
		t.Fatal(err)
	}
	due, _ := e.st.DueBlogs(ctx, e.clock())
	e.st.Claim(ctx, due[0], e.clock().Add(time.Minute))
	e.st.FinishBlog(ctx, bo.ID, false, "", "", "example error")
	q = c.get("/queue?kind=x&status=approved")
	for _, want := range []string{">held<", "Held until blog is live. Stays approved."} {
		if !strings.Contains(q.body, want) {
			t.Errorf("held promo lacks %q", want)
		}
	}
	if bq = c.get("/queue"); !strings.Contains(bq.body, "Linked promos are held and stay approved") {
		t.Fatal("failed blog card lacks the held line")
	}
	// Schedule: the blog post is a day banner.
	if s := c.get("/schedule"); !strings.Contains(s.body, `class="bban`) || !strings.Contains(s.body, "Blog · example.<wbr>test") {
		t.Fatal("schedule day banner")
	}
}
