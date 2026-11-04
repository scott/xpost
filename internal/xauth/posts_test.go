package xauth_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
	"github.com/scott/xpost/internal/xauth/xfake"
)

func kind(t *testing.T, err error) poster.Kind {
	t.Helper()
	var pe *poster.Error
	if !errors.As(err, &pe) {
		t.Fatalf("not a poster.Error: %v", err)
	}
	return pe.Kind
}

func TestXPosterCreatesPostsAndReplies(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	p := &xauth.XPoster{M: f.m}
	ctx := context.Background()
	if err := p.Ready(ctx, "sample_writer"); err != nil {
		t.Fatal(err)
	}
	r1, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example one"})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example two", ReplyTo: r1.PostID})
	if err != nil {
		t.Fatal(err)
	}
	posts := f.fake.Posts()
	if len(posts) != 2 || posts[0].ID != r1.PostID || posts[1].ReplyTo != r1.PostID || posts[1].ID != r2.PostID || posts[1].Username != "sample_writer" {
		t.Fatalf("posts %+v", posts)
	}
	if r1.URL != "https://x.invalid/sample_writer/status/"+r1.PostID {
		t.Fatalf("url %q", r1.URL)
	}
	if strings.Contains(f.logs.String(), "fake-at-") || strings.Contains(f.logs.String(), "Example") {
		t.Fatalf("token or text logged: %s", f.logs.String())
	}
}

func TestXPosterNotConnected(t *testing.T) {
	f := setup(t)
	p := &xauth.XPoster{M: f.m}
	ctx := context.Background()
	if p.Ready(ctx, "sample_writer") == nil {
		t.Fatal("ready without a connection")
	}
	_, err := p.Post(ctx, poster.Request{Account: "sample_writer", Text: "Example"})
	if kind(t, err) != poster.NotSent || f.fake.CreateCalls != 0 {
		t.Fatalf("%v %d", err, f.fake.CreateCalls)
	}
}

// An expired access token gets one refresh and one retry; X created the
// post once.
func TestXPosterRefreshesOn401(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.fake.ExpireAccess("sample_writer")
	_, err := (&xauth.XPoster{M: f.m}).Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
	if err != nil || f.fake.CreateCalls != 2 || len(f.fake.Posts()) != 1 || f.fake.RefreshCalls != 1 {
		t.Fatalf("%v calls %d posts %d refreshes %d", err, f.fake.CreateCalls, len(f.fake.Posts()), f.fake.RefreshCalls)
	}
}

func TestXPosterClassifiesFailures(t *testing.T) {
	reset := t0.Add(9 * time.Minute)
	cases := []struct {
		name    string
		fault   xfake.Fault
		want    poster.Kind
		created int
	}{
		{"429", xfake.Fault{Kind: xfake.RateLimit, Reset: reset}, poster.RateLimited, 0},
		{"400", xfake.Fault{Kind: xfake.Status, Status: 400}, poster.Refused, 0},
		{"403", xfake.Fault{Kind: xfake.Status, Status: 403}, poster.Refused, 0},
		{"500", xfake.Fault{Kind: xfake.Status, Status: 500}, poster.Ambiguous, 0},
		{"503", xfake.Fault{Kind: xfake.Status, Status: 503}, poster.Ambiguous, 0},
		{"drop after create", xfake.Fault{Kind: xfake.DropAfterCreate}, poster.Ambiguous, 1},
		{"drop before create", xfake.Fault{Kind: xfake.DropBeforeCreate}, poster.Ambiguous, 0},
		{"garbled 201", xfake.Fault{Kind: xfake.Garbled}, poster.Ambiguous, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.connect("sample_writer")
			f.fake.Queue(c.fault)
			_, err := (&xauth.XPoster{M: f.m}).Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
			if k := kind(t, err); k != c.want {
				t.Fatalf("kind %v, want %v (%v)", k, c.want, err)
			}
			if n := len(f.fake.Posts()); n != c.created {
				t.Fatalf("created %d", n)
			}
			var pe *poster.Error
			errors.As(err, &pe)
			if c.want == poster.RateLimited && !pe.RetryAt.Equal(reset) {
				t.Fatalf("retry at %v, want %v", pe.RetryAt, reset)
			}
			if strings.Contains(err.Error(), "fake-at-") {
				t.Fatal("token in error")
			}
		})
	}
}

// X refuses duplicate text; that is a definite "not posted".
func TestXPosterDuplicateIsRefused(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	p := &xauth.XPoster{M: f.m}
	p.Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
	_, err := p.Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
	if kind(t, err) != poster.Refused || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("%v", err)
	}
}

// A slow reply past the client timeout may still have posted: ambiguous.
func TestXPosterSlowReplyIsAmbiguous(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.c.HTTP.Timeout = 200 * time.Millisecond
	f.fake.Queue(xfake.Fault{Kind: xfake.Hang, Create: true}) // released only by the client giving up
	_, err := (&xauth.XPoster{M: f.m}).Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
	if kind(t, err) != poster.Ambiguous || len(f.fake.Posts()) != 1 {
		t.Fatalf("%v %d", err, len(f.fake.Posts()))
	}
}

// A request that never connects was not sent.
func TestXPosterDialFailureIsNotSent(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	f.c.Cfg.PostURL = "http://" + addr + "/2/tweets"
	_, err = (&xauth.XPoster{M: f.m}).Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example"})
	if kind(t, err) != poster.NotSent {
		t.Fatalf("%v", err)
	}
}

func TestXPosterRefusesBadMediaIDsAndReplyTarget(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	p := &xauth.XPoster{M: f.m}
	if _, err := p.Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example", MediaIDs: []string{"med_x"}}); kind(t, err) != poster.Refused {
		t.Fatal(err)
	}
	if _, err := p.Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example", ReplyTo: "../1"}); kind(t, err) != poster.Refused {
		t.Fatal(err)
	}
	if f.fake.CreateCalls != 0 {
		t.Fatal("sent anyway")
	}
}

// The fake only lets an account reply to its own posts.
func TestFakeRefusesRepliesToOthers(t *testing.T) {
	f := setup(t)
	f.connect("sample_writer")
	f.connect("sample_maker")
	p := &xauth.XPoster{M: f.m}
	r, err := p.Post(context.Background(), poster.Request{Account: "sample_maker", Text: "Example"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Post(context.Background(), poster.Request{Account: "sample_writer", Text: "Example reply", ReplyTo: r.PostID})
	if kind(t, err) != poster.Refused {
		t.Fatalf("%v", err)
	}
}

// ---- Dispatcher + XPoster + fake X ----

type dfx struct {
	*fx
	owner *store.User
	tok   *store.AgentToken
	disp  *poster.Dispatcher
}

func dsetup(t *testing.T) *dfx {
	f := setup(t)
	ctx := context.Background()
	owner, _, err := f.st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.st.CreateAgentToken(ctx, owner, "sample-agent", []string{store.ScopeDraftsWrite})
	if err != nil {
		t.Fatal(err)
	}
	f.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	if err := f.st.SetCapCents(ctx, owner, 2000); err != nil {
		t.Fatal(err)
	}
	f.connect("sample_writer")
	f.connect("sample_maker")
	return &dfx{fx: f, owner: owner, tok: tok, disp: &poster.Dispatcher{Store: f.st, Poster: &xauth.XPoster{M: f.m}}}
}

func (d *dfx) draft(t *testing.T, account string, at time.Time, texts ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var parts []store.PartIn
	for _, s := range texts {
		parts = append(parts, store.PartIn{Text: s})
	}
	dr, _, err := d.st.CreateDraft(ctx, d.tok, store.NewDraft{Account: account, Parts: parts, ProposedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.st.Approve(ctx, d.owner, dr.ID, dr.Version); err != nil {
		t.Fatal(err)
	}
	return dr.ID
}

func (d *dfx) run(t *testing.T) poster.Report {
	t.Helper()
	rep, err := d.disp.RunDue(context.Background(), d.clock())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (d *dfx) get(t *testing.T, id int64) *store.Draft {
	t.Helper()
	dr, err := d.st.Draft(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return dr
}

// chain checks the fake's posts form one reply chain with these texts.
func chain(t *testing.T, posts []xfake.Post, texts ...string) {
	t.Helper()
	if len(posts) != len(texts) {
		t.Fatalf("%d posts, want %d: %+v", len(posts), len(texts), posts)
	}
	for i, p := range posts {
		if p.Text != texts[i] {
			t.Fatalf("post %d text %q", i, p.Text)
		}
		want := ""
		if i > 0 {
			want = posts[i-1].ID
		}
		if p.ReplyTo != want {
			t.Fatalf("post %d replies to %q, want %q", i, p.ReplyTo, want)
		}
	}
}

func TestDispatchThreadToFakeX(t *testing.T) {
	d := dsetup(t)
	id := d.draft(t, "sample_writer", t0.Add(20*time.Minute), "Example one", "Example two", "Example three")
	d.advance(20 * time.Minute)
	if rep := d.run(t); rep.Posted != 1 {
		t.Fatalf("%+v", rep)
	}
	chain(t, d.fake.Posts(), "Example one", "Example two", "Example three")
	rows, _ := d.st.DraftLog(context.Background(), id)
	for i, r := range rows {
		if r.PostID != d.fake.Posts()[i].ID || r.URL != "https://x.invalid/sample_writer/status/"+r.PostID {
			t.Fatalf("row %+v", r)
		}
	}
	// Restart: a new dispatcher reconciles and posts nothing again.
	d.disp = &poster.Dispatcher{Store: d.st, Poster: &xauth.XPoster{M: d.m}}
	d.disp.Reconcile(context.Background())
	d.advance(time.Hour)
	d.run(t)
	if len(d.fake.Posts()) != 3 || d.fake.CreateCalls != 3 {
		t.Fatal("posted again after restart")
	}
}

// 429 mid-thread: wait for X's reset, then post the rest as replies.
func TestDispatchRateLimitMidThread(t *testing.T) {
	d := dsetup(t)
	id := d.draft(t, "sample_writer", t0.Add(20*time.Minute), "Example one", "Example two", "Example three")
	d.fake.Queue(xfake.Fault{}, xfake.Fault{Kind: xfake.RateLimit, Reset: t0.Add(31 * time.Minute)})
	d.advance(20 * time.Minute)
	if rep := d.run(t); rep.Waiting != 1 {
		t.Fatalf("%+v", rep)
	}
	d.advance(10 * time.Minute)
	d.run(t)
	if len(d.fake.Posts()) != 1 {
		t.Fatal("did not wait for the reset")
	}
	d.advance(time.Minute)
	d.run(t)
	chain(t, d.fake.Posts(), "Example one", "Example two", "Example three")
	if d.get(t, id).Status != store.StatusPosted {
		t.Fatal("not posted")
	}
}

// A refusal mid-thread fails the draft; the owner's Retry posts only the
// remaining parts, replying to the last posted id.
func TestDispatchRefusalMidThreadThenRetry(t *testing.T) {
	d := dsetup(t)
	ctx := context.Background()
	id := d.draft(t, "sample_writer", t0.Add(20*time.Minute), "Example one", "Example two", "Example three")
	d.fake.Queue(xfake.Fault{}, xfake.Fault{Kind: xfake.Status, Status: 400})
	d.advance(20 * time.Minute)
	d.run(t)
	dr := d.get(t, id)
	if dr.Status != store.StatusFailed || !strings.Contains(dr.Error, "Part 2 of 3 failed") {
		t.Fatalf("%s %q", dr.Status, dr.Error)
	}
	if err := d.st.RetryDraft(ctx, d.owner, id, dr.Version); err != nil {
		t.Fatal(err)
	}
	d.run(t)
	chain(t, d.fake.Posts(), "Example one", "Example two", "Example three")
}

// A dropped connection after X created part 2: never re-sent blindly. The
// owner says it posted (pasting the link), and Retry posts part 3 as a
// reply to it.
func TestDispatchAmbiguousMidThreadReview(t *testing.T) {
	d := dsetup(t)
	ctx := context.Background()
	id := d.draft(t, "sample_writer", t0.Add(20*time.Minute), "Example one", "Example two", "Example three")
	d.fake.Queue(xfake.Fault{}, xfake.Fault{Kind: xfake.DropAfterCreate})
	d.advance(20 * time.Minute)
	if rep := d.run(t); rep.Review != 1 {
		t.Fatalf("%+v", rep)
	}
	d.advance(time.Hour)
	d.run(t)
	if len(d.fake.Posts()) != 2 || d.fake.CreateCalls != 2 {
		t.Fatal("re-sent an ambiguous part")
	}
	dr := d.get(t, id)
	onX := d.fake.Posts()[1].ID
	if err := d.st.ResolveUnknown(ctx, d.owner, id, dr.Version, 2, true, onX, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.st.RetryDraft(ctx, d.owner, id, dr.Version); err != nil {
		t.Fatal(err)
	}
	d.run(t)
	chain(t, d.fake.Posts(), "Example one", "Example two", "Example three")
}

// Shutdown during an X call does not abandon it: the reply is recorded.
func TestDispatchShutdownMidCallStillRecords(t *testing.T) {
	d := dsetup(t)
	id := d.draft(t, "sample_writer", t0.Add(20*time.Minute), "Example one", "Example two")
	release, arrived := make(chan struct{}), make(chan struct{})
	d.fake.Queue(xfake.Fault{Kind: xfake.Hang, Release: release, Arrived: arrived})
	d.advance(20 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.disp.RunDue(ctx, d.clock())
	}()
	<-arrived // the request is at X
	cancel()
	close(release)
	<-done
	rows, _ := d.st.DraftLog(context.Background(), id)
	if len(rows) != 1 || rows[0].Status != store.LogPosted {
		t.Fatalf("rows %+v", rows)
	}
	// Part 2 was not started after the cancel; the draft resumes later.
	if dr := d.get(t, id); dr.Status != store.StatusPosting {
		t.Fatalf("status %s", dr.Status)
	}
	d.advance(poster.DefaultLease)
	d.run(t)
	chain(t, d.fake.Posts(), "Example one", "Example two")
}
