package blog

import (
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

func hookBlog() *store.Blog {
	return &store.Blog{ID: 1, Name: "notes.example.test", Method: store.MethodWebhook, SiteURL: "https://notes.example.test"}
}

func samplePost() Post {
	return Post{DraftID: 7, Slug: "sample-week", Title: "Example post: a sample week", Description: "Invented one-line summary.", Tags: []string{"build", "sample"},
		Markdown: "Example opening paragraph.\n\nA [sample link](https://example.test/a)."}
}

// The canonical form is exact: a golden vector the agent can check.
func TestCanonicalForm(t *testing.T) {
	den, _ := time.LoadLocation("America/Denver")
	at := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	pl, js, key, err := BuildHookPayload(hookBlog(), samplePost(), at, den)
	if err != nil {
		t.Fatal(err)
	}
	if pl.HTML != string(blogtext.Render(samplePost().Markdown)) {
		t.Fatal("html is not the preview renderer's output")
	}
	want := "xpost-blog-publish-v1\n" +
		"slug=sample-week\n" +
		"live_url=https://notes.example.test/blog/sample-week/\n" +
		"date_utc=2026-11-05\n" +
		"date_local=2026-11-05\n" +
		"zone=America/Denver\n" +
		"title=" + hexsum("Example post: a sample week") + "\n" +
		"description=" + hexsum("Invented one-line summary.") + "\n" +
		"tags=" + hexsum("build\nsample") + "\n" +
		"markdown=" + hexsum(samplePost().Markdown) + "\n" +
		"html=" + hexsum(pl.HTML) + "\n" +
		"cover=none\ncover_type=none\ncover_alt=none\n"
	if pl.Canonical != want || pl.ContentSHA != hexsum(want) || pl.Marker != `<meta name="xpost:content" content="sha256:`+pl.ContentSHA+`">` {
		t.Fatalf("canonical:\n%s", pl.Canonical)
	}
	if !strings.Contains(js, `"content_sha256":"`+pl.ContentSHA+`"`) || strings.Contains(js, "base64") {
		t.Fatalf("payload %s", js)
	}
	// A later local day is a new key and hash; the same local day is not.
	pl2, _, key2, _ := BuildHookPayload(hookBlog(), samplePost(), at.Add(30*time.Hour), den)
	if key2 == key || pl2.ContentSHA == pl.ContentSHA {
		t.Fatal("a later day kept the content key or hash")
	}
	if _, _, key3, _ := BuildHookPayload(hookBlog(), samplePost(), at.Add(5*time.Hour), den); key3 != key {
		t.Fatal("the same local day changed the content key")
	}
	p := samplePost()
	p.Title += " (edited)"
	if _, _, key4, _ := BuildHookPayload(hookBlog(), p, at, den); key4 == key {
		t.Fatal("an edit kept the content key")
	}
}

// 11:30 PM in Denver is the next day in UTC: both dates are sent and the
// local one is the blog's day.
func TestHookDatesAtDayBoundary(t *testing.T) {
	den, _ := time.LoadLocation("America/Denver")
	at := time.Date(2026, 11, 5, 23, 30, 0, 0, den)
	pl, _, _, _ := BuildHookPayload(hookBlog(), samplePost(), at, den)
	if pl.DateUTC != "2026-11-06" || pl.DateLocal != "2026-11-05" || pl.PublishedAt != "2026-11-06T06:30:00Z" {
		t.Fatalf("dates %s %s %s", pl.DateUTC, pl.DateLocal, pl.PublishedAt)
	}
}

func TestHookCoverChecks(t *testing.T) {
	at := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	p := samplePost()
	p.HasCover, p.CoverMIME, p.Cover, p.CoverAlt = true, "image/svg+xml", []byte("<svg/>"), "Example alt."
	if _, _, _, err := BuildHookPayload(hookBlog(), p, at, time.UTC); err != ErrHookCover {
		t.Fatalf("svg: %v", err)
	}
	p.CoverMIME, p.Cover = "image/png", make([]byte, MaxHookCover+1)
	if _, _, _, err := BuildHookPayload(hookBlog(), p, at, time.UTC); err != ErrHookCover {
		t.Fatalf("big: %v", err)
	}
	p.Cover = []byte("\x89PNG example")
	pl, _, _, err := BuildHookPayload(hookBlog(), p, at, time.UTC)
	if err != nil || pl.Cover == nil || pl.Cover.SHA256 != hexsum(string(p.Cover)) || !strings.Contains(pl.Canonical, "cover_type=image/png\ncover_alt="+hexsum("Example alt.")+"\n") {
		t.Fatalf("cover %+v %v", pl.Cover, err)
	}
}

// The Retry key follows the blog-local day: 4 PM and 6 PM in Denver are the
// same local day across UTC midnight (same key); 11:59 PM and 12:01 AM are
// different local days (new key, both dates move).
func TestHookKeyAtMidnightInBlogZone(t *testing.T) {
	den, _ := time.LoadLocation("America/Denver")
	key := func(at time.Time) (string, HookPayload) {
		pl, _, k, err := BuildHookPayload(hookBlog(), samplePost(), at, den)
		if err != nil {
			t.Fatal(err)
		}
		return k, pl
	}
	k1, p1 := key(time.Date(2026, 11, 5, 16, 0, 0, 0, den))
	k2, p2 := key(time.Date(2026, 11, 5, 18, 0, 0, 0, den))
	if p1.DateUTC == p2.DateUTC || k1 != k2 {
		t.Fatalf("same local day across UTC midnight: %s %s, keys equal %v", p1.DateUTC, p2.DateUTC, k1 == k2)
	}
	k3, p3 := key(time.Date(2026, 11, 5, 23, 59, 0, 0, den))
	k4, p4 := key(time.Date(2026, 11, 6, 0, 1, 0, 0, den))
	if k3 == k4 || p3.DateLocal != "2026-11-05" || p4.DateLocal != "2026-11-06" || p3.ContentSHA == p4.ContentSHA {
		t.Fatalf("local midnight: %s %s", p3.DateLocal, p4.DateLocal)
	}
	if k1 != k3 {
		t.Fatal("same local day, different keys")
	}
}
