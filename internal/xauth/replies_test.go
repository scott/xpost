package xauth_test

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
)

// The rule that matters most for replies: the poster and the scheduler
// never send one. An approved reply, long past its time, with a live X
// connection and a spend cap, goes through the real dispatcher and the real
// X poster pointed at the fake X server, and the fake sees zero requests.
// A plain post afterwards does reach the fake, so the zero is not a wiring
// accident.
func TestDispatcherNeverSendsAReply(t *testing.T) {
	d := dsetup(t)
	ctx := context.Background()
	var hits atomic.Int64
	counted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		d.fake.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(counted.Close)
	cfg := *d.c.Cfg
	cfg.TokenURL, cfg.RevokeURL, cfg.MeURL = counted.URL+"/2/oauth2/token", counted.URL+"/2/oauth2/revoke", counted.URL+"/2/users/me"
	cfg.PostURL, cfg.MediaURL, cfg.MediaMetaURL = counted.URL+"/2/tweets", counted.URL+"/2/media/upload", counted.URL+"/2/media/metadata"
	c := xauth.NewClient(&cfg)
	c.Now = d.clock
	disp := &poster.Dispatcher{Store: d.st, Poster: &xauth.XPoster{M: &xauth.Manager{Store: d.st, Client: c, Log: log.New(d.logs, "", 0)}}}

	at := d.clock().Add(-2 * time.Hour)
	r, _, err := d.st.CreateDraft(ctx, d.tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example reply, never through the API"}},
		Reply: &store.NewReply{InReplyTo: "1900000000000000111", AuthorName: "Sample Builder", AuthorHandle: "sample_builder", Text: "Example post", CreatedAt: &at}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.Approve(ctx, d.owner, r.ID, r.Version); err != nil {
		t.Fatal(err)
	}
	r = d.get(t, r.ID)
	if err := d.st.OpenedOnX(ctx, d.owner, r.ID, r.Version); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		d.advance(48 * time.Hour)
		rep, err := disp.RunDue(ctx, d.clock())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Posted != 0 || rep.Block != "" {
			t.Fatalf("run %d: %+v", i, rep)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the fake X server saw %d requests for an approved reply", n)
	}
	if len(d.fake.Posts()) != 0 {
		t.Fatalf("fake has posts: %+v", d.fake.Posts())
	}
	if got := d.get(t, r.ID); got.Status != store.StatusApproved {
		t.Fatalf("reply is %s, want approved (waiting for the owner)", got.Status)
	}
	if rows, _ := d.st.DraftLog(ctx, r.ID); len(rows) != 0 {
		t.Fatalf("post log rows for a reply: %d", len(rows))
	}

	// Control: a plain approved post that is due does reach the fake.
	p, _, err := d.st.CreateDraft(ctx, d.tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example post"}}, ProposedAt: d.clock().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.Approve(ctx, d.owner, p.ID, p.Version); err != nil {
		t.Fatal(err)
	}
	d.advance(2 * time.Minute)
	if rep, err := disp.RunDue(ctx, d.clock()); err != nil || rep.Posted != 1 {
		t.Fatalf("control post: %+v %v", rep, err)
	}
	if hits.Load() == 0 || len(d.fake.Posts()) != 1 || d.fake.Posts()[0].ReplyTo != "" {
		t.Fatalf("control: %d hits, posts %+v", hits.Load(), d.fake.Posts())
	}
	if got := d.get(t, r.ID); got.Status != store.StatusApproved {
		t.Fatalf("reply is %s after the control run", got.Status)
	}
}

// Post links are format checked only: x.com hosts, /<handle>/status/<id>.
func TestParseStatusLink(t *testing.T) {
	good := map[string][2]string{
		"https://x.com/sample_writer/status/1900000000000000777": {"sample_writer", "1900000000000000777"},
		"x.com/sample_writer/status/123":                         {"sample_writer", "123"},
		"https://www.x.com/Sample_Builder/status/42/":            {"Sample_Builder", "42"},
		"https://mobile.x.com/sample/status/42?s=20#frag":        {"sample", "42"},
		"  https://x.com/sample/status/42  ":                     {"sample", "42"},
	}
	for in, want := range good {
		h, id, err := xauth.ParseStatusLink(in)
		if err != nil || h != want[0] || id != want[1] {
			t.Errorf("%q: %q %q %v", in, h, id, err)
		}
	}
	bad := map[string]error{
		"https://x.com/sample_writer":                 xauth.ErrProfileLink,
		"https://example.test/sample_writer/status/1": xauth.ErrNotXLink,
		"https://x.com.example.test/a/status/1":       xauth.ErrNotXLink,
		"ftp://x.com/a/status/1":                      xauth.ErrNotXLink,
		"https://user@x.com/a/status/1":               xauth.ErrNotXLink,
		"https://x.com:8443/a/status/1":               xauth.ErrNotXLink,
		"https://x.com/a/status/12ab":                 xauth.ErrNotPostLink,
		"https://x.com/a/status/1/photo/1":            xauth.ErrNotPostLink,
		"https://x.com/":                              xauth.ErrNotPostLink,
		"":                                            xauth.ErrNotXLink,
	}
	for in, want := range bad {
		if _, _, err := xauth.ParseStatusLink(in); err != want {
			t.Errorf("%q: %v, want %v", in, err, want)
		}
	}
	c := &xauth.Config{WebURL: "https://x.invalid"}
	if got := c.IntentURL("Example reply & more", "42"); got != "https://x.invalid/intent/post?text=Example%20reply%20%26%20more&in_reply_to=42" {
		t.Fatalf("intent %q", got)
	}
}
