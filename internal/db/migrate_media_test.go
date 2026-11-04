package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0004 adds images to post_log and the upload tables. Existing log rows
// keep every value (images 0); the new CHECKs refuse unknown states.
func TestMediaMigrationKeepsData(t *testing.T) {
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
		if m.Version >= 4 {
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
	if _, err := raw.Exec(`INSERT INTO post_log(draft_id, account, part, parts, at, status, post_id, has_url, cost_micros, preview, idem_key, attempts) VALUES (7, 'sample_writer', 1, 1, ?, 'posted', '42', 1, 200000, 'Example', 'd7-v2-p1', 1)`, now); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var status, key string
	var cost, attempts, images int64
	if err := d.QueryRow(`SELECT status, idem_key, cost_micros, attempts, images FROM post_log WHERE draft_id = 7`).Scan(&status, &key, &cost, &attempts, &images); err != nil {
		t.Fatal(err)
	}
	if status != "posted" || key != "d7-v2-p1" || cost != 200000 || attempts != 1 || images != 0 {
		t.Fatalf("row changed: %s %s %d %d %d", status, key, cost, attempts, images)
	}
	for _, st := range []string{"uploading", "uploaded", "retry", "failed", "unknown"} {
		if _, err := d.Exec(`INSERT INTO media_uploads(draft_id, account, part, slot, media_id, idem_key, status, at) VALUES (7, 'a', 1, 1, 1, ?, ?, 0)`, "k-"+st, st); err != nil {
			t.Errorf("upload status %s refused: %v", st, err)
		}
		if _, err := d.Exec(`INSERT INTO media_calls(at, idem_key, status) VALUES (0, ?, ?)`, "k-"+st, st); err != nil {
			t.Errorf("ledger status %s refused: %v", st, err)
		}
	}
	if _, err := d.Exec(`INSERT INTO media_uploads(draft_id, account, part, slot, media_id, idem_key, status, at) VALUES (7, 'a', 1, 1, 1, 'k-bogus', 'posted', 0)`); err == nil {
		t.Error("media_uploads CHECK refuses nothing")
	}
	if _, err := d.Exec(`INSERT INTO media_calls(at, idem_key, status) VALUES (0, 'k', 'bogus')`); err == nil {
		t.Error("media_calls CHECK refuses nothing")
	}
	if _, err := d.Exec(`INSERT INTO media_uploads(draft_id, account, part, slot, media_id, idem_key, status, at) VALUES (7, 'a', 1, 1, 1, 'k-retry', 'retry', 0)`); err == nil {
		t.Error("upload idem_key not unique")
	}
}
