package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0003 rebuilds post_log to widen its status CHECK. Rows written by slice
// 1/2 survive, drafts and their parts are untouched, and the new states
// are accepted while unknown ones are still refused.
func TestSchedulerMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xpost.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Version >= 3 {
			break
		}
		if _, err := raw.Exec(m.SQL); err != nil {
			t.Fatal(m.Name, err)
		}
		if m.Version == 1 {
			oldSeed(t, raw) // an install from before 0001 stopped seeding accounts
		}
		raw.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, m.Version, m.Name, time.Now().Unix())
	}
	now := int64(1780000000)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO drafts(id, account, status, proposed_at, scheduled_at, source, created_at, updated_at) VALUES (7, 'sample_writer', 'posted', ?, ?, 'example', ?, ?)`, now, now, now, now)
	mustExec(`INSERT INTO draft_parts(draft_id, position, text) VALUES (7, 0, 'Example')`)
	mustExec(`INSERT INTO post_log(draft_id, account, part, parts, at, status, post_id, url, has_url, cost_micros, preview, idem_key) VALUES (7, 'sample_writer', 1, 1, ?, 'posted', '42', 'https://x.invalid/a/status/42', 1, 200000, 'Example', 'd7-v2-p1')`, now)
	raw.Close()

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var status, postID, key string
	var hasURL, cost, attempts, retry int64
	var replyTo string
	if err := d.QueryRow(`SELECT status, post_id, idem_key, has_url, cost_micros, attempts, retry_at, reply_to FROM post_log WHERE draft_id = 7`).Scan(&status, &postID, &key, &hasURL, &cost, &attempts, &retry, &replyTo); err != nil {
		t.Fatal(err)
	}
	if status != "posted" || postID != "42" || key != "d7-v2-p1" || hasURL != 1 || cost != 200000 || attempts != 0 || retry != 0 || replyTo != "" {
		t.Fatalf("row changed: %s %s %s %d %d %d %d %q", status, postID, key, hasURL, cost, attempts, retry, replyTo)
	}
	var parts, lease int
	d.QueryRow(`SELECT COUNT(*) FROM draft_parts WHERE draft_id = 7`).Scan(&parts)
	d.QueryRow(`SELECT lease_until FROM drafts WHERE id = 7`).Scan(&lease)
	if parts != 1 || lease != 0 {
		t.Fatalf("drafts touched: parts %d lease %d", parts, lease)
	}
	for _, st := range []string{"posting", "retry", "unknown", "failed"} {
		if _, err := d.Exec(`INSERT INTO post_log(draft_id, account, part, parts, at, status, idem_key) VALUES (8, 'a', 1, 1, 0, ?, ?)`, st, "k-"+st); err != nil {
			t.Errorf("status %s refused: %v", st, err)
		}
	}
	if _, err := d.Exec(`INSERT INTO post_log(draft_id, account, part, parts, at, status, idem_key) VALUES (8, 'a', 1, 1, 0, 'bogus', 'k-bogus')`); err == nil {
		t.Error("CHECK no longer refuses unknown statuses")
	}
	if _, err := d.Exec(`INSERT INTO post_log(draft_id, account, part, parts, at, status, idem_key) VALUES (8, 'a', 1, 1, 0, 'posted', 'd7-v2-p1')`); err == nil {
		t.Error("idem_key no longer unique")
	}
}
