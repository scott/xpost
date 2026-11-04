package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

const emDash = "\u2014"

func (f *fx) blog(t *testing.T, name string, method string) *store.Blog {
	t.Helper()
	ctx := context.Background()
	b, err := f.st.CreateBlog(ctx, f.owner, store.BlogIn{Name: name, SiteURL: "https://" + name, Method: method, APIBase: "https://api.example.test", Topics: []string{"sample banned topic"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fx) ready(t *testing.T, b *store.Blog) {
	t.Helper()
	ctx := context.Background()
	if b.Method == store.MethodWebhook {
		f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookURL, "https://hooks.example.test/blog")
		f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	} else {
		f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogAPIToken, "sample_api_token_000000000000")
	}
	if err := f.st.RecordBlogTest(ctx, f.owner, b.ID, true, "Test passed. Nothing was published.", 12); err != nil {
		t.Fatal(err)
	}
}

func post(site, slug string) store.NewBlogDraft {
	return store.NewBlogDraft{Site: site, Slug: slug, Title: "Example post: a sample week", Description: "Invented one-line summary.", Tags: []string{"sample", "demo"},
		Markdown: "Example opening paragraph.\n\nA second paragraph with sample words."}
}

func (f *fx) propose(t *testing.T, nb store.NewBlogDraft) *store.Draft {
	t.Helper()
	d, _, err := f.st.CreateBlogDraft(context.Background(), f.a, nb, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBlogSecretsSealedAndTestResets(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "example.test", store.MethodWebhook)
	if b.TestState != store.TestUntested || b.Ready() {
		t.Fatalf("new blog: %s", b.TestState)
	}
	const url = "https://hooks.example.test/sample-path"
	if err := f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookURL, url); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	f.st.DB.QueryRow(`SELECT hook_url_sealed FROM blogs WHERE id = ?`, b.ID).Scan(&raw)
	if len(raw) == 0 || strings.Contains(string(raw), "hooks.example") {
		t.Fatal("hook URL is not sealed at rest")
	}
	if got, err := f.st.BlogSecret(ctx, b.ID, store.BlogHookURL); err != nil || got != url {
		t.Fatalf("open: %q %v", got, err)
	}
	// The AAD binds the value to its field and blog: moved, it won't open.
	b2 := f.blog(t, "second.example.test", store.MethodWebhook)
	f.st.DB.Exec(`UPDATE blogs SET hook_key_sealed = ? WHERE id = ?`, raw, b2.ID)
	if _, err := f.st.BlogSecret(ctx, b2.ID, store.BlogHookKey); err == nil {
		t.Fatal("a sealed value opened under another field and blog")
	}

	f.ready(t, b)
	reload := func() *store.Blog { x, _ := f.st.BlogByID(ctx, b.ID); return x }
	if !reload().Ready() {
		t.Fatal("test did not pass")
	}
	// Any connection edit resets the test.
	f.st.SetBlogSecret(ctx, f.owner, b.ID, store.BlogHookKey, "another-sample-signing-key-000000000")
	if reload().TestState != store.TestUntested {
		t.Fatal("secret change kept the test")
	}
	f.ready(t, b)
	in := store.BlogIn{Name: b.Name, SiteURL: b.SiteURL, Method: store.MethodAPI, APIBase: "https://api.example.test"}
	if _, err := f.st.UpdateBlog(ctx, f.owner, b.ID, in); err != nil {
		t.Fatal(err)
	}
	if reload().TestState != store.TestUntested {
		t.Fatal("method change kept the test")
	}
	f.ready(t, reload())
	in.SiteURL = "https://www.example.test"
	f.st.UpdateBlog(ctx, f.owner, b.ID, in)
	if reload().TestState != store.TestUntested {
		t.Fatal("site URL change kept the test")
	}
	f.ready(t, reload())
	in.PublishClock = "08:30"
	f.st.UpdateBlog(ctx, f.owner, b.ID, in)
	if !reload().Ready() {
		t.Fatal("a non-connection edit reset the test")
	}
	// Blogs are not X accounts.
	accts, _ := f.st.Accounts(ctx)
	for _, a := range accts {
		if strings.HasPrefix(a.Handle, "blog:") {
			t.Fatalf("blog listed as an X account: %s", a.Handle)
		}
	}
}

func TestBlogIntake(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "example.test", store.MethodWebhook)
	// Default time: the next 7:00 AM in the zone given (here Denver, then Tokyo).
	den, _ := time.LoadLocation("America/Denver")
	d, created, err := f.st.CreateBlogDraft(ctx, f.a, post("Example.Test", "sample-week"), den)
	if err != nil || !created {
		t.Fatal(err)
	}
	if got := d.ScheduledAt.In(den); got.Hour() != 7 || got.Minute() != 0 || !got.After(t0) {
		t.Fatalf("default time %v", got)
	}
	if !d.IsBlog() || d.Kind != store.KindBlog || d.Account != b.Account || d.Status != store.StatusPending || d.Post.Slug != "sample-week" {
		t.Fatalf("draft %+v", d)
	}
	tok, _ := time.LoadLocation("Asia/Tokyo")
	d2, _, _ := f.st.CreateBlogDraft(ctx, f.a, post("example.test", "sample-week-2"), tok)
	if got := d2.ScheduledAt.In(tok); got.Hour() != 7 {
		t.Fatalf("tokyo default %v", got)
	}
	// Same key, same draft.
	nb := post("example.test", "keyed")
	nb.IdempotencyKey = "k1"
	a, _, _ := f.st.CreateBlogDraft(ctx, f.a, nb, time.UTC)
	again, created, err := f.st.CreateBlogDraft(ctx, f.a, nb, time.UTC)
	if err != nil || created || again.ID != a.ID {
		t.Fatalf("idempotent: %v %v", created, err)
	}

	bad := func(name string, nb store.NewBlogDraft, want string) {
		t.Helper()
		_, _, err := f.st.CreateBlogDraft(ctx, f.a, nb, time.UTC)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	bad("dup slug", post("example.test", "sample-week"), "already used")
	bad("site", post("nope.test", "x"), "unknown site")
	x := post("example.test", "Bad Slug")
	bad("slug", x, "slug")
	x = post("example.test", "ok-slug")
	x.Title = ""
	bad("title", x, "title")
	x = post("example.test", "ok-slug")
	x.Description = "two\nlines"
	bad("desc", x, "description")
	x = post("example.test", "ok-slug")
	x.Markdown = "Intro.\n\nA clause" + emDash + "and another."
	_, _, err = f.st.CreateBlogDraft(ctx, f.a, x, time.UTC)
	var le *store.LintError
	if !errors.As(err, &le) || len(le.Findings) != 1 || le.Findings[0].Rule != blogtext.RuleEmDash || le.Findings[0].Where != "paragraph 2" {
		t.Fatalf("em dash: %v", err)
	}
	if !errors.Is(err, store.ErrConflict) || le.Error() != "Fix 1 lint error to approve or publish." {
		t.Fatalf("lint error: %v", err)
	}
	x.Markdown = "We will not discuss Sample Banned Topic here."
	bad("banned", x, "lint error")
	// A warning is stored and reported, not refused.
	x.Markdown = "Sample years 2019\u20132024."
	if _, _, err := f.st.CreateBlogDraft(ctx, f.a, x, time.UTC); err != nil {
		t.Fatalf("en dash refused: %v", err)
	}
	// X drafts are unaffected: blog accounts can't take X drafts.
	if _, _, err := f.st.CreateDraft(ctx, f.a, store.NewDraft{Account: b.Account, Parts: []store.PartIn{{Text: "x"}}, ProposedAt: t0.Add(time.Hour)}); err == nil {
		t.Fatal("X draft accepted for a blog destination")
	}
}

func TestBlogApproveAndPublishNow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "example.test", store.MethodWebhook)
	d := f.propose(t, post("example.test", "sample-week"))
	err := f.st.Approve(ctx, f.owner, d.ID, d.Version)
	if err == nil || !strings.Contains(err.Error(), "cannot publish until its last test passes. Drafts can still be reviewed.") {
		t.Fatalf("untested approve: %v", err)
	}
	if err := f.st.PublishNow(ctx, f.owner, d.ID, d.Version); err == nil {
		t.Fatal("publish now on an untested blog")
	}
	f.ready(t, b)
	// Rules can change after intake: Approve lints again.
	in := store.BlogIn{Name: b.Name, SiteURL: b.SiteURL, Method: b.Method, Topics: []string{"opening"}}
	f.st.UpdateBlog(ctx, f.owner, b.ID, in)
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err == nil || !strings.Contains(err.Error(), "Fix 1 lint error") {
		t.Fatalf("lint on approve: %v", err)
	}
	in.Topics = nil
	f.st.UpdateBlog(ctx, f.owner, b.ID, in)
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusApproved {
		t.Fatal(d.Status)
	}
	// Publish now: approved for now.
	if err := f.st.PublishNow(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusApproved || !d.ScheduledAt.Equal(f.now) {
		t.Fatalf("publish now: %s %v", d.Status, d.ScheduledAt)
	}
	// Blogs are never X work.
	if due, _ := f.st.DueDrafts(ctx, f.now); len(due) != 0 {
		t.Fatalf("blog post in DueDrafts: %v", due)
	}
	due, _ := f.st.DueBlogs(ctx, f.now)
	if len(due) != 1 || due[0].ID != d.ID {
		t.Fatalf("DueBlogs: %v", due)
	}
	ok, _ := f.st.Claim(ctx, due[0], f.now.Add(time.Minute))
	if !ok {
		t.Fatal("claim")
	}
	if err := f.st.FinishBlog(ctx, d.ID, false, "", "", "Sample failure."); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusFailed || d.Error != "Sample failure." {
		t.Fatalf("failed: %s %q", d.Status, d.Error)
	}
	// Failed blog posts wait in Pending; retry is Publish now.
	q, _ := f.st.Queue(ctx, store.Filter{Status: store.StatusPending, Kind: store.KindBlog})
	if len(q) != 1 || q[0].ID != d.ID {
		t.Fatalf("pending tab: %d", len(q))
	}
	if err := f.st.PublishNow(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	due, _ = f.st.DueBlogs(ctx, f.now)
	f.st.Claim(ctx, due[0], f.now.Add(time.Minute))
	if err := f.st.FinishBlog(ctx, d.ID, true, b.LiveURL("sample-week"), "ref_1", ""); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPosted || d.Post.PublishedURL != "https://example.test/blog/sample-week/" || !d.Post.Staged() {
		t.Fatalf("published: %s %q", d.Status, d.Post.PublishedURL)
	}
	if xq, _ := f.st.Queue(ctx, store.Filter{Status: store.StatusPosted, Kind: store.KindX}); len(xq) != 0 {
		t.Fatal("blog post under the X filter")
	}
}

func TestBlogSlugLocksAndEdits(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "example.test", store.MethodWebhook)
	f.ready(t, b)
	d := f.propose(t, post("example.test", "sample-week"))
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	d = f.get(t, d.ID)
	e := store.BlogEdit{Title: "New sample title", Description: "New line.", Slug: "renamed-week", Tags: []string{"a", "A", "b"}, Markdown: "Body.", When: t0.Add(48 * time.Hour)}
	if err := f.st.SaveBlogPost(ctx, f.owner, d.ID, d.Version, e); err != nil {
		t.Fatal(err)
	}
	d = f.get(t, d.ID)
	if d.Status != store.StatusPending || d.Post.Slug != "renamed-week" || len(d.Post.Tags) != 2 || d.Post.Title != "New sample title" {
		t.Fatalf("edit: %s %+v", d.Status, d.Post)
	}
	f.st.MarkStaged(ctx, d.ID, "ref_1")
	e.Slug = "third-name"
	if err := f.st.SaveBlogPost(ctx, f.owner, d.ID, d.Version, e); err == nil || !strings.Contains(err.Error(), "slug is locked") {
		t.Fatalf("staged slug change: %v", err)
	}
	e.Slug = "renamed-week"
	if err := f.st.SaveBlogPost(ctx, f.owner, d.ID, d.Version, e); err != nil {
		t.Fatal(err)
	}
	// The X editor refuses blog posts; so does Rewrite.
	d = f.get(t, d.ID)
	if err := f.st.Save(ctx, f.owner, d.ID, d.Version, store.Edit{Parts: []store.PartIn{{Text: "x"}}, When: t0.Add(time.Hour)}, false); err == nil {
		t.Fatal("X save on a blog post")
	}
	if _, err := f.st.StartRewrite(ctx, f.owner, d.ID, d.Version, ""); err == nil {
		t.Fatal("rewrite on a blog post")
	}
}

func TestBlogRemoveBlocked(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	b := f.blog(t, "example.test", store.MethodWebhook)
	f.ready(t, b)
	d := f.propose(t, post("example.test", "sample-week"))
	err := f.st.RemoveBlog(ctx, f.owner, b.ID)
	var busy *store.BlogBusyError
	if !errors.As(err, &busy) || busy.Pending != 1 {
		t.Fatalf("remove with a pending post: %v", err)
	}
	f.st.Approve(ctx, f.owner, d.ID, d.Version)
	if err := f.st.RemoveBlog(ctx, f.owner, b.ID); !errors.As(err, &busy) || busy.Scheduled != 1 {
		t.Fatalf("remove with a scheduled post: %v", err)
	}
	d = f.get(t, d.ID)
	f.st.Reject(ctx, f.owner, d.ID, d.Version, "")
	if err := f.st.RemoveBlog(ctx, f.owner, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.BlogByID(ctx, b.ID); err == nil {
		t.Fatal("removed blog still listed")
	}
	var n int
	f.st.DB.QueryRow(`SELECT COUNT(*) FROM blogs WHERE id = ? AND (hook_url_sealed IS NOT NULL OR hook_key_sealed IS NOT NULL)`, b.ID).Scan(&n)
	if n != 0 {
		t.Fatal("secrets kept after remove")
	}
	if _, _, err := f.st.CreateBlogDraft(ctx, f.a, post("example.test", "after"), time.UTC); err == nil {
		t.Fatal("intake for a removed blog")
	}
}
