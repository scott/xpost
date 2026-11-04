package sweep

import (
	"bytes"
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// With no poster or dispatcher running at all (posting off), the sweep
// still sends a promo whose blog is not live 24h past its time back to
// pending, and only then.
func TestPromoSweepWithPosterOff(t *testing.T) {
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	k, _ := secret.NewKey()
	st := store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC).Unix())
	st.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	owner, _, _ := st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	tok, _, _ := st.CreateAgentToken(ctx, owner, "sample-agent-1", []string{store.ScopeDraftsWrite})
	b, err := st.CreateBlog(ctx, owner, store.BlogIn{Name: "notes.example.test", SiteURL: "https://notes.example.test", Method: store.MethodWebhook})
	if err != nil {
		t.Fatal(err)
	}
	st.SetBlogSecret(ctx, owner, b.ID, store.BlogHookURL, "https://hooks.example.test/blog")
	st.SetBlogSecret(ctx, owner, b.ID, store.BlogHookKey, "sample-signing-key-0000000000000000")
	st.RecordBlogTest(ctx, owner, b.ID, true, "ok", 1)
	bp, _, err := st.CreateBlogDraft(ctx, tok, store.NewBlogDraft{Site: b.Name, Slug: "sample-week", Title: "Example post", Description: "Invented.", Markdown: "Example opening."}, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := st.CreateDraft(ctx, tok, store.NewDraft{Account: "sample_writer", Parts: []store.PartIn{{Text: "Example promo " + store.BlogURLToken}}, Promotes: bp.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Approve(ctx, owner, p.ID, p.Version); err != nil {
		t.Fatal(err)
	}
	status := func() string {
		got, err := st.Draft(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}

	var out syncBuf
	sctx, cancel := context.WithCancel(ctx)
	done := Promos(sctx, st, 5*time.Millisecond, log.New(&out, "", 0))
	defer func() { cancel(); <-done }()

	clock.Store(p.ScheduledAt.Add(23 * time.Hour).Unix())
	time.Sleep(50 * time.Millisecond)
	if s := status(); s != store.StatusApproved {
		t.Fatalf("returned before 24h: %s", s)
	}
	clock.Store(p.ScheduledAt.Add(24*time.Hour + time.Minute).Unix())
	deadline := time.Now().Add(5 * time.Second)
	for status() != store.StatusPending {
		if time.Now().After(deadline) {
			t.Fatal("the sweep did not return the promo with posting off")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := st.Draft(ctx, p.ID)
	if !got.PromoReturned {
		t.Fatal("promo_returned not set")
	}
	cancel()
	<-done
	if !strings.Contains(out.String(), "1 promo(s) back to pending") {
		t.Fatalf("log: %q", out.String())
	}
}

func TestShortDur(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Minute: "1m", 30 * time.Second: "30s", 90 * time.Second: "1m30s", time.Hour: "1h"} {
		if got := shortDur(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}

// With no live stream or tab open (no web server at all), the sweep alone
// keeps the change feed at store.LiveKeep rows and announces a rewrite that
// ran out of time with exactly one change row, however often it, or the
// live hub, runs again.
func TestLiveSweepWithNoSubscribers(t *testing.T) {
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	k, _ := secret.NewKey()
	st := store.New(d, k)
	// A fresh install has no X accounts; tests use the two sample ones.
	for _, h := range []string{"sample_writer", "sample_maker"} {
		if _, err := st.AddAccount(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC).Unix())
	st.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	owner, _, _ := st.CreateOwner(ctx, "owner@example.com", "Sample Owner")
	tok, _, _ := st.CreateAgentToken(ctx, owner, "sample-agent-1", []string{store.ScopeDraftsWrite, store.ScopeDraftsRevise})
	if err := st.SetAgentWebhook(ctx, owner, tok.ID, "https://hooks.example.test/rewrite", "sample-signing-key-0000000000000000"); err != nil {
		t.Fatal(err)
	}
	p, _, err := st.CreateDraft(ctx, tok, store.NewDraft{Account: "sample_writer", ProposedAt: st.Now().Add(6 * time.Hour), Parts: []store.PartIn{{Text: "Example draft for a rewrite"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartRewrite(ctx, owner, p.ID, p.Version, "Sample note"); err != nil {
		t.Fatal(err)
	}
	// Fill the feed past the cap with invented rows.
	if _, err := d.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO live_changes(kind, ref, v) SELECT 'log', ?, 1 FROM n`, store.LiveKeep+250, p.ID); err != nil {
		t.Fatal(err)
	}
	count := func() (n int) {
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_changes`).Scan(&n)
		return
	}
	timeouts := func(after int64) (n int) {
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_changes WHERE kind = 'rewrite' AND ref = ? AND seq > ?`, p.ID, after).Scan(&n)
		return
	}
	before, _, _ := st.LiveSeq(ctx)
	if count() <= store.LiveKeep {
		t.Fatalf("setup: %d rows", count())
	}

	var out syncBuf
	sctx, cancel := context.WithCancel(ctx)
	done := Promos(sctx, st, 5*time.Millisecond, log.New(&out, "", 0))
	defer func() { cancel(); <-done }()

	// Inside the window: pruned, but no timeout yet.
	deadline := time.Now().Add(5 * time.Second)
	for count() > store.LiveKeep {
		if time.Now().After(deadline) {
			t.Fatalf("the sweep did not prune: %d rows", count())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	if n := timeouts(before); n != 0 {
		t.Fatalf("timeout announced early: %d", n)
	}

	clock.Add(int64((store.RewriteTimeout + time.Minute) / time.Second))
	for timeouts(before) == 0 {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("the sweep did not note the rewrite timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // several more ticks
	cancel()
	<-done
	// The live hub calling it as well adds nothing.
	if n, err := st.MarkRewriteTimeouts(ctx, st.Now()); err != nil || n != 0 {
		t.Fatalf("second caller noted %d (%v)", n, err)
	}
	Live(ctx, st, log.New(&out, "", 0))
	if n := timeouts(before); n != 1 {
		t.Fatalf("timeout change rows: %d, want exactly 1", n)
	}
	if n := count(); n > store.LiveKeep {
		t.Fatalf("%d rows after sweeping", n)
	}
	var seq, v int64
	var kind string
	d.QueryRowContext(ctx, `SELECT seq, kind, v FROM live_changes ORDER BY seq DESC LIMIT 1`).Scan(&seq, &kind, &v)
	if kind != "rewrite" || v != int64(p.Version) {
		t.Fatalf("newest row: %s v%d", kind, v)
	}
	logs := out.String()
	if n := strings.Count(logs, "1 rewrite request(s) ran out of time"); n != 1 {
		t.Fatalf("timeout log lines %d: %q", n, logs)
	}
	if !strings.HasPrefix(logs, "promo sweep every 5ms (also rewrite timeouts and the live feed, newest 10000 changes kept)\n") {
		t.Fatalf("startup line: %q", logs)
	}
}
