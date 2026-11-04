package poster

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

var t0 = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

type fx struct {
	st    *store.Store
	owner *store.User
	tok   *store.AgentToken
	now   time.Time
}

func setup(t *testing.T) *fx {
	t.Helper()
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	k, _ := secret.NewKey()
	f := &fx{now: t0}
	f.st = store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := f.st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	f.st.Now = func() time.Time { return f.now }
	if f.owner, _, err = f.st.CreateOwner(ctx, "owner@example.com", "Sample Owner"); err != nil {
		t.Fatal(err)
	}
	if f.tok, _, err = f.st.CreateAgentToken(ctx, f.owner, "sample-agent", []string{store.ScopeDraftsWrite}); err != nil {
		t.Fatal(err)
	}
	return f
}

// enable configures what posting needs: invented rates ($1 a post, $2 with
// a link) and a $20 cap.
func (f *fx) enable(t *testing.T) {
	t.Helper()
	f.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	if err := f.st.SetCapCents(context.Background(), f.owner, 2000); err != nil {
		t.Fatal(err)
	}
}

// enableImages adds invented media rates: $0.50 per upload, $0.10 per alt
// text call.
func (f *fx) enableImages() {
	f.st.Rates.UploadMicros, f.st.Rates.UploadKnown = 500_000, true
	f.st.Rates.AltMicros, f.st.Rates.AltKnown = 100_000, true
}

func (f *fx) disp(p Poster) *Dispatcher {
	return &Dispatcher{Store: f.st, Poster: p}
}

func (f *fx) run(t *testing.T, d *Dispatcher) Report {
	t.Helper()
	rep, err := d.RunDue(context.Background(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (f *fx) get(t *testing.T, id int64) *store.Draft {
	t.Helper()
	d, err := f.st.Draft(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fx) rows(t *testing.T, id int64) []*store.LogEntry {
	t.Helper()
	r, err := f.st.DraftLog(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *fx) draft(t *testing.T, account string, at time.Time, approve bool, texts ...string) int64 {
	t.Helper()
	var parts []store.PartIn
	for _, s := range texts {
		parts = append(parts, store.PartIn{Text: s})
	}
	d, _, err := f.st.CreateDraft(context.Background(), f.tok, store.NewDraft{Account: account, Parts: parts, ProposedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if err := f.st.Approve(context.Background(), f.owner, d.ID, d.Version); err != nil {
			t.Fatal(err)
		}
	}
	return d.ID
}

func status(t *testing.T, f *fx, id int64) string {
	d, err := f.st.Draft(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return d.Status
}

func TestDispatcherPostsOnlyApprovedDue(t *testing.T) {
	f := setup(t)
	f.enable(t)
	due := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two", "Example three")
	later := f.draft(t, "sample_maker", t0.Add(3*time.Hour), true, "Example later")
	pend := f.draft(t, "sample_maker", t0.Add(20*time.Minute), false, "Example pending")
	fake := &Fake{}
	disp := f.disp(fake)
	f.now = t0.Add(19 * time.Minute)
	if rep := f.run(t, disp); rep.Posted != 0 || len(fake.Calls()) != 0 {
		t.Fatal("posted before its time")
	}
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Posted != 1 || rep.Failed != 0 {
		t.Fatalf("RunDue: %+v", rep)
	}
	calls := fake.Calls()
	// A thread posts back to back as a reply chain.
	if len(calls) != 3 || calls[0].Text != "Example one" || calls[0].ReplyTo != "" || calls[1].ReplyTo != "fake-1" || calls[2].ReplyTo != "fake-2" {
		t.Fatalf("calls %+v", calls)
	}
	if status(t, f, due) != store.StatusPosted || status(t, f, later) != store.StatusApproved || status(t, f, pend) != store.StatusPending {
		t.Fatal("wrong statuses after run")
	}
	rows := f.rows(t, due)
	for i, e := range rows {
		if e.Status != store.LogPosted || e.PostID != fmt.Sprintf("fake-%d", i+1) || !strings.HasPrefix(e.URL, "https://x.invalid/") || e.CostMicros != 1_000_000 {
			t.Fatalf("row %d: %+v", i, e)
		}
		if i > 0 && e.ReplyTo != rows[i-1].PostID {
			t.Fatalf("row %d replies to %q", i, e.ReplyTo)
		}
	}
	// Running again posts nothing new.
	if rep := f.run(t, disp); rep.Posted != 0 || len(fake.Calls()) != 3 {
		t.Fatalf("second run posted again: %+v, %d calls", rep, len(fake.Calls()))
	}
}

// Fail closed: no cap set means nothing posts, and the reason is given.
func TestNoCapMeansNoPosting(t *testing.T) {
	f := setup(t)
	f.st.Rates = store.Rates{PostMicros: 1_000_000, LinkPostMicros: 2_000_000, Known: true}
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one")
	fake := &Fake{}
	f.now = t0.Add(time.Hour)
	rep := f.run(t, f.disp(fake))
	if len(fake.Calls()) != 0 || !strings.Contains(rep.Block, "No monthly spend cap") || status(t, f, id) != store.StatusApproved {
		t.Fatalf("%+v %d", rep, len(fake.Calls()))
	}
}

// Unset rates mean the cap can't be enforced: nothing posts.
func TestUnverifiedRatesMeanNoPosting(t *testing.T) {
	f := setup(t)
	if err := f.st.SetCapCents(context.Background(), f.owner, 2000); err != nil {
		t.Fatal(err)
	}
	f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one")
	fake := &Fake{}
	f.now = t0.Add(time.Hour)
	rep := f.run(t, f.disp(fake))
	if len(fake.Calls()) != 0 || !strings.Contains(rep.Block, "Cost rates are not configured") {
		t.Fatalf("%+v", rep)
	}
	if f.st.PartCost(false) != 0 {
		t.Fatal("unverified rates priced a part")
	}
}

func TestParseRates(t *testing.T) {
	if r, err := store.ParseRates("", ""); err != nil || r.Known {
		t.Fatalf("empty: %+v %v", r, err)
	}
	if r, err := store.ParseRates("0.015", "$0.2"); err != nil || !r.Known || r.PostMicros != 15_000 || r.LinkPostMicros != 200_000 {
		t.Fatalf("set: %+v %v", r, err)
	}
	for _, c := range [][2]string{{"1", ""}, {"", "1"}, {"x", "1"}, {"-1", "1"}, {"1", "101"}, {"NaN", "1"}} {
		if _, err := store.ParseRates(c[0], c[1]); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
}

// The cap pauses posting: a draft that would pass it waits (approved),
// and once spend reaches the cap the run reports it.
func TestCapPausesPosting(t *testing.T) {
	f := setup(t)
	f.enable(t) // $20 cap, $1 per post
	ctx := context.Background()
	var ids []int64
	for i := 0; i < 4; i++ {
		var texts []string
		for j := 0; j < 5; j++ {
			texts = append(texts, fmt.Sprintf("Example %d.%d", i, j))
		}
		ids = append(ids, f.draft(t, []string{"sample_writer", "sample_maker"}[i%2], t0.Add(time.Duration(20+40*i)*time.Minute), true, texts...))
	}
	big := f.draft(t, "sample_writer", t0.Add(200*time.Minute), true, "Example big 1", "Example big 2")
	fake := &Fake{}
	disp := f.disp(fake)
	for i := 0; i < 4; i++ { // spacing lets one item per account per 15 min
		f.now = t0.Add(5*time.Hour + time.Duration(i)*store.MinSpacing)
		f.run(t, disp)
	}
	// 4 threads x 5 parts = $20: the cap. The fifth draft waits.
	if len(fake.Calls()) != 20 || status(t, f, big) != store.StatusApproved {
		t.Fatalf("%d calls, big %s", len(fake.Calls()), status(t, f, big))
	}
	rep := f.run(t, disp)
	if !strings.Contains(rep.Block, "cap reached") || len(fake.Calls()) != 20 {
		t.Fatalf("%+v", rep)
	}
	// Raising the cap lets it through.
	f.st.SetCapCents(ctx, f.owner, 2500)
	f.run(t, disp)
	if status(t, f, big) != store.StatusPosted || len(fake.Calls()) != 22 {
		t.Fatalf("after raise: %s %d", status(t, f, big), len(fake.Calls()))
	}
	_ = ids
}

// A thread that would cross the cap does not start (no half threads).
func TestCapNeverStartsAThreadItCannotFinish(t *testing.T) {
	f := setup(t)
	f.enable(t)
	f.st.SetCapCents(context.Background(), f.owner, 250) // $2.50
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two", "Example three")
	fake := &Fake{}
	f.now = t0.Add(time.Hour)
	f.run(t, f.disp(fake))
	if len(fake.Calls()) != 0 || status(t, f, id) != store.StatusApproved {
		t.Fatalf("%d calls, %s", len(fake.Calls()), status(t, f, id))
	}
}

// Spacing holds at post time: two overdue drafts on one account post at
// least MinSpacing apart; other accounts are not held up.
func TestSpacingAtPostTime(t *testing.T) {
	f := setup(t)
	f.enable(t)
	a := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example A")
	b := f.draft(t, "sample_writer", t0.Add(35*time.Minute), true, "Example B")
	c := f.draft(t, "sample_maker", t0.Add(36*time.Minute), true, "Example C")
	fake := &Fake{}
	disp := f.disp(fake)
	f.now = t0.Add(50 * time.Minute) // the server was down; both are overdue
	f.run(t, disp)
	if status(t, f, a) != store.StatusPosted || status(t, f, b) != store.StatusApproved || status(t, f, c) != store.StatusPosted {
		t.Fatalf("a %s b %s c %s", status(t, f, a), status(t, f, b), status(t, f, c))
	}
	f.now = t0.Add(64 * time.Minute)
	f.run(t, disp)
	if status(t, f, b) != store.StatusApproved {
		t.Fatal("b posted within 15 min of a")
	}
	f.now = t0.Add(65 * time.Minute)
	f.run(t, disp)
	if status(t, f, b) != store.StatusPosted {
		t.Fatal("b not posted after the gap")
	}
}

func TestPausedAccountAndImagesWait(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one")
	f.st.DB.ExecContext(ctx, `UPDATE accounts SET paused = 1 WHERE handle = 'sample_writer'`)
	fake := &Fake{}
	f.now = t0.Add(time.Hour)
	f.run(t, f.disp(fake))
	if len(fake.Calls()) != 0 || status(t, f, id) != store.StatusApproved {
		t.Fatal("posted on a paused account")
	}
	f.st.DB.ExecContext(ctx, `UPDATE accounts SET paused = 0 WHERE handle = 'sample_writer'`)
	f.now = t0
	ref, _, err := f.st.AddMedia(ctx, f.tok, demo.Image(0))
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := f.st.CreateDraft(ctx, f.tok, store.NewDraft{Account: "sample_maker", Parts: []store.PartIn{{Text: "Example image", Media: []store.MediaIn{{Ref: ref, Alt: "Example"}}}}, ProposedAt: t0.Add(20 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Approve(ctx, f.owner, d.ID, d.Version); err != nil {
		t.Fatal(err)
	}
	f.now = t0.Add(time.Hour)
	disp := &Dispatcher{Store: f.st, Poster: textOnly{fake}} // no MediaPoster
	f.enableImages()
	f.run(t, disp)
	if status(t, f, d.ID) != store.StatusApproved {
		t.Fatal("a draft with images was sent to a poster without media support")
	}
}

// A refused part fails the draft; Retry posts only the remaining parts,
// replying to the last posted id.
func TestPartialThreadRetryPostsOnlyTheRest(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two", "Example three")
	refuse := true
	fake := &Fake{Fail: func(r Request) error {
		if r.Text == "Example two" && refuse {
			return &Error{Kind: Refused, Status: 400, Msg: "X refused it: example"}
		}
		return nil
	}}
	disp := f.disp(fake)
	f.now = t0.Add(time.Hour)
	if rep := f.run(t, disp); rep.Failed != 1 {
		t.Fatalf("%+v", rep)
	}
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "Part 2 of 3 failed") || len(fake.Calls()) != 2 {
		t.Fatalf("%s %q %d", d.Status, d.Error, len(fake.Calls()))
	}
	// A failed draft is not picked up again automatically.
	f.now = f.now.Add(time.Hour)
	if f.run(t, disp); len(fake.Calls()) != 2 {
		t.Fatal("failed draft retried on its own")
	}
	// Agents can't retry: RetryDraft needs the owner.
	if err := f.st.RetryDraft(ctx, nil, id, d.Version); err == nil {
		t.Fatal("retry without the owner")
	}
	refuse = false
	if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
		t.Fatal(err)
	}
	f.run(t, disp)
	calls := fake.Calls()
	if len(calls) != 4 || calls[2].Text != "Example two" || calls[2].ReplyTo != "fake-1" || calls[3].ReplyTo != "fake-2" {
		t.Fatalf("calls %+v", calls)
	}
	if status(t, f, id) != store.StatusPosted {
		t.Fatal("not posted after retry")
	}
	if rows := f.rows(t, id); rows[1].Attempts != 2 || rows[0].Attempts != 1 {
		t.Fatalf("attempts %d %d", rows[0].Attempts, rows[1].Attempts)
	}
}

// 429 backs off until X's reset, then resumes the thread from the part that
// was refused, replying to the last posted id.
func TestRateLimitWaitsForReset(t *testing.T) {
	f := setup(t)
	f.enable(t)
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two")
	other := f.draft(t, "sample_writer", t0.Add(40*time.Minute), true, "Example other")
	reset := t0.Add(27 * time.Minute)
	limited := 1
	fake := &Fake{Fail: func(r Request) error {
		if r.Text == "Example two" && limited > 0 {
			limited--
			return &Error{Kind: RateLimited, Status: 429, Msg: "rate limited by X", RetryAt: reset}
		}
		return nil
	}}
	disp := f.disp(fake)
	f.now = t0.Add(20 * time.Minute)
	if rep := f.run(t, disp); rep.Waiting != 1 {
		t.Fatalf("%+v", rep)
	}
	d := f.get(t, id)
	if d.Status != store.StatusPosting || !d.LeaseUntil.Equal(reset) || d.Attempts != 1 || !strings.Contains(d.Error, "retrying automatically") {
		t.Fatalf("%s %v %d %q", d.Status, d.LeaseUntil, d.Attempts, d.Error)
	}
	if r := f.rows(t, id); r[1].Status != store.LogRetry || !r[1].RetryAt.Equal(reset) {
		t.Fatalf("row %+v", r[1])
	}
	f.now = t0.Add(26 * time.Minute)
	f.run(t, disp)
	if len(fake.Calls()) != 2 {
		t.Fatal("retried before the reset")
	}
	f.now = reset
	f.run(t, disp)
	calls := fake.Calls()
	if len(calls) != 3 || calls[2].Text != "Example two" || calls[2].ReplyTo != "fake-1" || status(t, f, id) != store.StatusPosted {
		t.Fatalf("calls %+v, %s", calls, status(t, f, id))
	}
	f.now = t0.Add(40 * time.Minute)
	f.run(t, disp)
	if status(t, f, other) != store.StatusPosted {
		t.Fatal("other not posted")
	}
}

// Without a reset header the wait backs off exponentially, and after
// MaxAttempts the draft fails instead of retrying forever.
func TestRateLimitBackoffGivesUp(t *testing.T) {
	f := setup(t)
	f.enable(t)
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one")
	fake := &Fake{Fail: func(Request) error { return &Error{Kind: RateLimited, Status: 429, Msg: "rate limited by X"} }}
	disp := f.disp(fake)
	disp.MaxAttempts = 3
	f.now = t0.Add(20 * time.Minute)
	f.run(t, disp)
	if d := f.get(t, id); !d.LeaseUntil.Equal(f.now.Add(time.Minute)) {
		t.Fatalf("first backoff until %v", d.LeaseUntil)
	}
	f.now = f.now.Add(time.Minute)
	f.run(t, disp)
	if d := f.get(t, id); !d.LeaseUntil.Equal(f.now.Add(2 * time.Minute)) {
		t.Fatalf("second backoff until %v", d.LeaseUntil)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.run(t, disp)
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "gave up after 3 attempts") || len(fake.Calls()) != 3 {
		t.Fatalf("%s %q %d", d.Status, d.Error, len(fake.Calls()))
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

// An ambiguous outcome is never retried automatically. The owner checks X
// and says whether it posted; Retry is refused until then.
func TestAmbiguousOutcomeNeedsReview(t *testing.T) {
	for _, posted := range []bool{true, false} {
		t.Run(fmt.Sprint("posted=", posted), func(t *testing.T) {
			f := setup(t)
			f.enable(t)
			ctx := context.Background()
			id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two", "Example three")
			ambiguous := true
			fake := &Fake{Fail: func(r Request) error {
				if r.Text == "Example two" && ambiguous {
					return errors.New("read: connection reset") // unclassified: treated as ambiguous
				}
				return nil
			}}
			disp := f.disp(fake)
			f.now = t0.Add(time.Hour)
			if rep := f.run(t, disp); rep.Review != 1 {
				t.Fatalf("%+v", rep)
			}
			d := f.get(t, id)
			if d.Status != store.StatusFailed || !strings.Contains(d.Error, "Needs review: part 2") {
				t.Fatalf("%s %q", d.Status, d.Error)
			}
			if r := f.rows(t, id); r[1].Status != store.LogUnknown {
				t.Fatalf("row %+v", r[1])
			}
			f.now = f.now.Add(24 * time.Hour)
			ambiguous = false
			f.run(t, disp)
			if len(fake.Calls()) != 2 {
				t.Fatal("ambiguous part retried automatically")
			}
			var ce *store.ConflictError
			if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); !errors.As(err, &ce) {
				t.Fatalf("retry before review: %v", err)
			}
			if err := f.st.ResolveUnknown(ctx, nil, id, d.Version, 2, posted, "1234", ""); err == nil {
				t.Fatal("resolved without the owner")
			}
			if err := f.st.ResolveUnknown(ctx, f.owner, id, d.Version, 2, true, "not-digits", ""); err == nil {
				t.Fatal("accepted a non-numeric post id")
			}
			if err := f.st.ResolveUnknown(ctx, f.owner, id, d.Version, 2, posted, "1234", "https://x.invalid/sample_writer/status/1234"); err != nil {
				t.Fatal(err)
			}
			if err := f.st.RetryDraft(ctx, f.owner, id, d.Version); err != nil {
				t.Fatal(err)
			}
			f.run(t, disp)
			calls := fake.Calls()
			if posted {
				// Part 2 is on X as 1234: only part 3 posts, replying to it.
				if len(calls) != 3 || calls[2].Text != "Example three" || calls[2].ReplyTo != "1234" {
					t.Fatalf("calls %+v", calls)
				}
			} else {
				if len(calls) != 4 || calls[2].Text != "Example two" || calls[2].ReplyTo != "fake-1" || calls[3].ReplyTo != "fake-2" {
					t.Fatalf("calls %+v", calls)
				}
			}
			if status(t, f, id) != store.StatusPosted {
				t.Fatal("not posted")
			}
		})
	}
}

// crashPoster simulates the process dying mid-call: the goroutine exits
// after the intent is committed and before any outcome is recorded.
type crashPoster struct {
	Fake
	crashOn string
}

func (c *crashPoster) Post(ctx context.Context, r Request) (Result, error) {
	if r.Text == c.crashOn {
		c.Fake.Post(ctx, r) // X got it (or not: nobody knows)
		runtime.Goexit()
	}
	return c.Fake.Post(ctx, r)
}

func runCrashing(t *testing.T, f *fx, d *Dispatcher) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.RunDue(context.Background(), f.now)
	}()
	<-done
}

// Restart after a crash mid-call: the in-flight part becomes "unknown" and
// the draft goes to review. It is never sent again automatically.
func TestRestartAfterCrashMidCallNeedsReview(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two")
	cp := &crashPoster{crashOn: "Example two"}
	f.now = t0.Add(20 * time.Minute)
	runCrashing(t, f, f.disp(cp))
	if r := f.rows(t, id); len(r) != 2 || r[1].Status != store.LogPosting {
		t.Fatalf("rows after crash: %+v", r)
	}
	// Restart while the old lease still runs: reconcile leaves it alone (the
	// old process might still be finishing).
	fake := &Fake{}
	disp := f.disp(fake)
	if err := disp.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	f.run(t, disp)
	if status(t, f, id) != store.StatusPosting || len(fake.Calls()) != 0 {
		t.Fatal("touched a draft under a live lease")
	}
	// After the lease lapses, a restart reconciles it to review.
	f.now = f.now.Add(DefaultLease)
	disp = f.disp(fake)
	if err := disp.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	d := f.get(t, id)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "Needs review: part 2") {
		t.Fatalf("%s %q", d.Status, d.Error)
	}
	if r := f.rows(t, id); r[1].Status != store.LogUnknown {
		t.Fatalf("row %+v", r[1])
	}
	f.run(t, disp)
	if len(fake.Calls()) != 0 {
		t.Fatal("re-posted after a crash")
	}
}

// The same crash found by a running dispatcher (lease lapsed, no restart)
// is treated the same way.
func TestLapsedLeaseFoundByRunNeedsReview(t *testing.T) {
	f := setup(t)
	f.enable(t)
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one")
	f.now = t0.Add(20 * time.Minute)
	runCrashing(t, f, f.disp(&crashPoster{crashOn: "Example one"}))
	fake := &Fake{}
	f.now = f.now.Add(DefaultLease)
	if rep := f.run(t, f.disp(fake)); rep.Review != 1 || len(fake.Calls()) != 0 || status(t, f, id) != store.StatusFailed {
		t.Fatalf("%+v %d %s", rep, len(fake.Calls()), status(t, f, id))
	}
}

// A crash between parts is safe to resume: every part is either posted
// with an id or was never sent.
func TestRestartBetweenPartsResumes(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	id := f.draft(t, "sample_writer", t0.Add(20*time.Minute), true, "Example one", "Example two", "Example three")
	f.now = t0.Add(20 * time.Minute)
	// What a run does up to the moment it dies after part 1: claim, record
	// the intent, post, record the outcome. Part 2's intent never happened.
	due, _ := f.st.DueDrafts(ctx, f.now)
	if ok, err := f.st.Claim(ctx, due[0], f.now.Add(DefaultLease)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	d := f.get(t, id)
	if err := f.st.BeginPart(ctx, store.LogEntry{DraftID: id, Account: d.Account, Part: 1, Parts: 3, IdemKey: Key(d, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.FinishPart(ctx, Key(d, 1), store.Outcome{Status: store.LogPosted, PostID: "stop-1", URL: "https://x.invalid/x"}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(DefaultLease)
	fake := &Fake{}
	disp := f.disp(fake)
	if err := disp.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if status(t, f, id) != store.StatusPosting {
		t.Fatal("resumable draft sent to review")
	}
	f.run(t, disp)
	calls := fake.Calls()
	if len(calls) != 2 || calls[0].Text != "Example two" || calls[0].ReplyTo != "stop-1" || calls[1].ReplyTo != "fake-1" {
		t.Fatalf("calls %+v", calls)
	}
	if status(t, f, id) != store.StatusPosted {
		t.Fatal("not posted")
	}
}

// Two dispatchers racing on one database never post a part twice.
func TestConcurrentDispatchersPostOnce(t *testing.T) {
	f := setup(t)
	f.enable(t)
	for i := 0; i < 4; i++ {
		f.draft(t, []string{"sample_writer", "sample_maker"}[i%2], t0.Add(time.Duration(20+20*i)*time.Minute), true, fmt.Sprintf("Example %d a", i), fmt.Sprintf("Example %d b", i))
	}
	fake := &Fake{}
	f.now = t0.Add(3 * time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := f.disp(fake)
			for j := 0; j < 3; j++ {
				d.RunDue(context.Background(), f.now)
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, c := range fake.Calls() {
		seen[c.IdempotencyKey]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s posted %d times", k, n)
		}
	}
}

// The cap month follows the owner's zone, not the server's.
func TestCapMonthUsesOwnerZone(t *testing.T) {
	f := setup(t)
	f.enable(t)
	ctx := context.Background()
	f.st.SetCapCents(ctx, f.owner, 100) // $1: one post
	f.st.SetSetting(ctx, store.SettingTZ, "Pacific/Auckland")
	// Auckland is UTC+12 in winter: July starts at 2026-06-30 12:00 UTC.
	f.now = time.Date(2026, 6, 30, 11, 0, 0, 0, time.UTC)
	a := f.draft(t, "sample_writer", f.now.Add(20*time.Minute), true, "Example June")
	b := f.draft(t, "sample_maker", f.now.Add(70*time.Minute), true, "Example July")
	fake := &Fake{}
	disp := f.disp(fake)
	f.now = f.now.Add(20 * time.Minute) // 11:20 UTC = June 30 23:20 Auckland
	f.run(t, disp)
	if status(t, f, a) != store.StatusPosted {
		t.Fatal("a not posted")
	}
	f.now = f.now.Add(40 * time.Minute) // 12:00 UTC: still June in UTC, July in Auckland
	f.run(t, disp)
	if status(t, f, b) != store.StatusApproved {
		t.Fatal("b posted over the cap") // b is due at 12:10
	}
	f.now = f.now.Add(10 * time.Minute)
	f.run(t, disp)
	if status(t, f, a) != store.StatusPosted || status(t, f, b) != store.StatusPosted {
		t.Fatalf("a %s b %s", status(t, f, a), status(t, f, b))
	}
}

func TestFakeURLsNeverResolve(t *testing.T) {
	r, _ := (&Fake{}).Post(context.Background(), Request{Account: "example", Text: "x"})
	if !strings.HasPrefix(r.URL, "https://x.invalid/") {
		t.Fatalf("fake URL %q", r.URL)
	}
}

func TestKindOf(t *testing.T) {
	if KindOf(errors.New("x")) != Ambiguous || KindOf(&Error{Kind: Refused}) != Refused || KindOf(fmt.Errorf("w: %w", &Error{Kind: NotSent})) != NotSent {
		t.Fatal("KindOf")
	}
}

// textOnly hides a poster's media methods.
type textOnly struct{ Poster }
