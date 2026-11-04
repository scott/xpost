package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

const emDashW = "\u2014"

// blogRW is a rewrite env with one ready blog and one pending agent post.
func blogRW(t *testing.T) (*rwEnv, *store.Blog, int64) {
	e := newRewriteEnv(t)
	b := e.readyBlog("notes.example.test")
	o := decodeBlog(t, e.api("POST", "/api/v1/drafts", e.sec, blogBody("notes.example.test", "sample-week", "Example opening paragraph.\n\nA second paragraph with sample words.")))
	if o.ID == 0 || o.Status != store.StatusPending {
		t.Fatalf("propose: %+v", o)
	}
	return e, b, o.ID
}

func (e *rwEnv) reviseBlog(bearer string, id, reqID int64, post map[string]any) resp {
	e.t.Helper()
	return e.api("POST", "/api/v1"+path(id, "/revise"), bearer, map[string]any{"request_id": reqID, "post": post})
}

func samplePostFields() map[string]any {
	return map[string]any{"title": "Example post: a shorter sample week", "description": "Invented one-line summary, tightened.", "tags": []string{"sample", "demo"},
		"markdown": "A shorter example opening.\n\nA second paragraph with sample words."}
}

func TestBlogRewriteFlow(t *testing.T) {
	e, _, id := blogRW(t)
	ctx := context.Background()
	// The Rewrite sheet on a blog card names the blog and slug.
	q := e.c.get("/queue?kind=blog&rewrite=" + strconvI(id)).body
	for _, want := range []string{`class="btn rwb on"`, "Send back for a rewrite", "notes.example.test · /sample-week", "The slug and cover stay."} {
		if !strings.Contains(q, want) {
			t.Errorf("rewrite sheet lacks %q", want)
		}
	}
	if r := e.rewrite(id, "Tighten the opening, please"); r.status != http.StatusSeeOther {
		t.Fatalf("rewrite: %d %s", r.status, r.body)
	}
	hits := e.hook.hits()
	var p map[string]any
	json.Unmarshal(hits[len(hits)-1].body, &p)
	post, _ := p["post"].(map[string]any)
	if p["kind"] != "blog" || p["parts"] != nil || post == nil || post["slug"] != "sample-week" || post["site"] != "notes.example.test" || post["cover"] != false ||
		!strings.Contains(post["markdown"].(string), "Example opening paragraph.") || !strings.Contains(p["callback"].(map[string]any)["body"].(string), `"post"`) {
		t.Fatalf("payload %s", hits[len(hits)-1].body)
	}
	// While waiting: Approve, Publish now and Edit wait; the card says so.
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusConflict {
		t.Fatalf("approve while waiting: %d", r.status)
	}
	d := e.draft(id)
	if err := e.st.PublishNow(ctx, e.owner, id, d.Version); err == nil || !strings.Contains(err.Error(), "Waiting on sample-agent") {
		t.Fatalf("publish now while waiting: %v", err)
	}
	if err := e.st.SaveBlogPost(ctx, e.owner, id, d.Version, store.BlogEdit{Title: "x", Description: "y", Slug: "sample-week", Markdown: "z", When: d.ScheduledAt}); err == nil {
		t.Fatal("edit while waiting")
	}
	q = e.c.get("/queue?kind=blog").body
	if !strings.Contains(q, `<span class="st rewriting">rewriting</span>`) || !strings.Contains(q, "Waiting on sample-agent, sent") || strings.Contains(q, `href="/drafts/`+strconvI(id)+`/edit"`) {
		t.Error("waiting card is not shown as waiting")
	}
	rid := e.lastRequestID(id)
	// Wrong shapes are refused and leave the request open.
	if r := e.revise(e.sec, id, rid, "parts for a blog post"); r.status != http.StatusBadRequest {
		t.Fatalf("parts on a blog post: %d %s", r.status, r.body)
	}
	missing := samplePostFields()
	delete(missing, "tags")
	if r := e.reviseBlog(e.sec, id, rid, missing); r.status != http.StatusBadRequest {
		t.Fatalf("missing tags: %d", r.status)
	}
	slug := samplePostFields()
	slug["slug"] = "another-slug"
	if r := e.reviseBlog(e.sec, id, rid, slug); r.status != http.StatusBadRequest {
		t.Fatalf("slug change: %d", r.status)
	}
	bad := samplePostFields()
	bad["title"] = "Two\nlines"
	if r := e.reviseBlog(e.sec, id, rid, bad); r.status != http.StatusBadRequest {
		t.Fatalf("two-line title: %d", r.status)
	}
	// The revised post: lint re-runs (an em dash blocks), it stays Pending.
	rev := samplePostFields()
	rev["markdown"] = "A shorter example opening " + emDashW + " with a dash.\n\nA second paragraph with sample words."
	r := e.reviseBlog(e.sec, id, rid, rev)
	var out struct {
		Result string  `json:"result"`
		Draft  blogOut `json:"draft"`
	}
	json.Unmarshal([]byte(r.body), &out)
	if r.status != http.StatusOK || out.Result != store.RewriteRevised || out.Draft.Status != store.StatusPending {
		t.Fatalf("revise: %d %s", r.status, r.body)
	}
	blocked := false
	for _, f := range out.Draft.Blog.Lint {
		blocked = blocked || (f.Rule == "em_dash" && f.Level == "block")
	}
	if !blocked {
		t.Fatalf("lint did not re-run: %s", r.body)
	}
	d = e.draft(id)
	if d.Post.Title != "Example post: a shorter sample week" || d.Post.Slug != "sample-week" || strings.Join(d.Post.Tags, ",") != "sample,demo" || d.Status != store.StatusPending {
		t.Fatalf("draft %+v", d.Post)
	}
	if r := e.c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusConflict {
		t.Fatalf("approve with blocking lint: %d", r.status)
	}
	q = e.c.get("/queue?kind=blog").body
	for _, want := range []string{"revised by sample-agent", "Previous version", `<span class="pn">Title</span>`, `<span class="pn">Body</span>`, "<ins>", "<del>", "Your note to sample-agent"} {
		if !strings.Contains(q, want) {
			t.Errorf("revised card lacks %q", want)
		}
	}
	revs, _ := e.st.Revisions(ctx, id)
	if len(revs) != 1 || revs[0].Post == nil || revs[0].Post.Title != "Example post: a sample week" {
		t.Fatalf("revision %+v", revs)
	}
	// A second round with the same text is "unchanged".
	e.rewrite(id, "")
	rev["markdown"] = "A shorter example opening.\n\nA second paragraph with sample words."
	e.reviseBlog(e.sec, id, e.lastRequestID(id), rev)
	e.rewrite(id, "")
	r = e.reviseBlog(e.sec, id, e.lastRequestID(id), rev)
	if !strings.Contains(r.body, `"result":"unchanged"`) {
		t.Fatalf("unchanged: %s", r.body)
	}
	// A cancelled request refuses a late reply.
	e.rewrite(id, "")
	rid = e.lastRequestID(id)
	e.c.post("/queue", path(id, "/rewrite/cancel"), url.Values{"return": {"/queue"}})
	if r := e.reviseBlog(e.sec, id, rid, samplePostFields()); r.status != http.StatusConflict {
		t.Fatalf("late reply: %d", r.status)
	}
	// The agent's GET shows the rewrite state on a blog post too.
	if g := e.api("GET", "/api/v1/drafts/"+strconvI(id), e.sec, nil).body; !strings.Contains(g, `"rewrite":{"request_id":`+strconvI(rid)+`,"state":"cancelled"`) {
		t.Fatalf("draft GET: %s", g)
	}
}

// Rewrite is for Pending posts only, so a scheduled or publishing post is
// never touched; a publish still in flight refuses the request and the
// reply, and the publish stays as it was.
func TestBlogRewriteNeverTouchesAPublish(t *testing.T) {
	e, b, id := blogRW(t)
	ctx := context.Background()
	e.st.Approve(ctx, e.owner, id, e.draft(id).Version)
	if r := e.rewrite(id, ""); r.status != http.StatusConflict {
		t.Fatalf("rewrite an approved post: %d", r.status)
	}
	if q := e.c.get("/queue?kind=blog&status=approved").body; strings.Contains(q, ">Rewrite</a>") {
		t.Error("approved card offers Rewrite")
	}
	// A pending post whose publish is still in flight (defense in depth).
	o := decodeBlog(t, e.api("POST", "/api/v1/drafts", e.sec, blogBody("notes.example.test", "sample-two", "Example opening paragraph.")))
	pub, err := e.st.StartBlogPublish(ctx, store.PublishStart{DraftID: o.ID, BlogID: b.ID, Version: 1, ContentKey: "k", ContentSHA: strings.Repeat("a", 64), Payload: "{}",
		LiveURL: "https://notes.example.test/blog/sample-two/", DeliveryID: "d_sample"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(e.hook.hits())
	if r := e.rewrite(o.ID, ""); r.status != http.StatusConflict || !strings.Contains(r.body, "A publish is underway") {
		t.Fatalf("rewrite with a publish in flight: %d", r.status)
	}
	if len(e.hook.hits()) != n {
		t.Fatal("a rewrite request went out")
	}
	after, _ := e.st.BlogPublishByID(ctx, pub.ID)
	if after.State != pub.State || after.ContentSHA != pub.ContentSHA || after.Version != pub.Version {
		t.Fatal("the in-flight publish changed")
	}
}

// A reply that arrives after the post left Pending (or once a publish is in
// flight) is refused and changes nothing.
func TestBlogRewriteReplyAfterPublishStarts(t *testing.T) {
	e, b, id := blogRW(t)
	ctx := context.Background()
	e.rewrite(id, "")
	rid := e.lastRequestID(id)
	before := e.draft(id)
	if _, err := e.st.StartBlogPublish(ctx, store.PublishStart{DraftID: id, BlogID: b.ID, Version: before.Version, ContentKey: "k", ContentSHA: strings.Repeat("b", 64), Payload: "{}",
		LiveURL: "https://notes.example.test/blog/sample-week/", DeliveryID: "d_sample"}); err != nil {
		t.Fatal(err)
	}
	if r := e.reviseBlog(e.sec, id, rid, samplePostFields()); r.status != http.StatusConflict || !strings.Contains(r.body, "a publish is underway") {
		t.Fatalf("reply with a publish in flight: %d %s", r.status, r.body)
	}
	if d := e.draft(id); d.Version != before.Version || d.Post.Title != before.Post.Title {
		t.Fatal("the post changed")
	}
	_ = time.Second
}

// X drafts keep their payload: no kind, no post.
func TestXRewritePayloadUnchanged(t *testing.T) {
	e := newRewriteEnv(t)
	o := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample X draft for the payload check.")
	e.rewrite(o, "")
	hits := e.hook.hits()
	var p map[string]any
	json.Unmarshal(hits[len(hits)-1].body, &p)
	if _, ok := p["kind"]; ok {
		t.Fatalf("X payload has kind: %s", hits[len(hits)-1].body)
	}
	if _, ok := p["post"]; ok || p["parts"] == nil {
		t.Fatalf("X payload: %s", hits[len(hits)-1].body)
	}
	rid := e.lastRequestID(o)
	if r := e.reviseBlog(e.sec, o, rid, samplePostFields()); r.status != http.StatusBadRequest {
		t.Fatalf("post on an X draft: %d", r.status)
	}
}
