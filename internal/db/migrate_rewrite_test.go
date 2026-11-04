package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0005 only adds tables: drafts written before it are untouched, a draft
// with no rewrite history needs no rows, the request state CHECK holds,
// and deleting a draft drops its requests and revisions.
func TestRewriteMigrationKeepsData(t *testing.T) {
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
		if m.Version >= 5 {
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
	must := func(db *sql.DB, q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(q, err)
		}
	}
	must(raw, `INSERT INTO agent_tokens(id, name, token_hash, last4, scopes, created_at) VALUES (1, 'sample-agent', 'h', 'abcd', 'drafts:write', ?)`, now)
	must(raw, `INSERT INTO drafts(id, account, status, proposed_at, source, token_id, created_at, updated_at) VALUES (7, 'sample_writer', 'pending', ?, 'sample-agent', 1, ?, ?)`, now, now, now)
	must(raw, `INSERT INTO draft_parts(draft_id, position, text) VALUES (7, 0, 'Sample text')`)
	raw.Close()

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var status, text string
	if err := d.QueryRow(`SELECT d.status, p.text FROM drafts d JOIN draft_parts p ON p.draft_id = d.id WHERE d.id = 7`).Scan(&status, &text); err != nil || status != "pending" || text != "Sample text" {
		t.Fatalf("draft changed: %q %q %v", status, text, err)
	}
	for _, table := range []string{"agent_webhooks", "rewrite_requests", "draft_revisions"} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}
	must(d, `INSERT INTO rewrite_requests(draft_id, token_id, note, sent_at, expires_at, state) VALUES (7, 1, '', ?, ?, 'open')`, now, now+3600)
	if _, err := d.Exec(`INSERT INTO rewrite_requests(draft_id, token_id, note, sent_at, expires_at, state) VALUES (7, 1, '', ?, ?, 'bogus')`, now, now); err == nil {
		t.Fatal("unknown rewrite state accepted")
	}
	must(d, `INSERT INTO draft_revisions(draft_id, request_id, version, parts_json, author, authored_at, created_at) VALUES (7, 1, 1, '[]', 'sample-agent', ?, ?)`, now, now)
	must(d, `DELETE FROM drafts WHERE id = 7`)
	var n int
	d.QueryRow(`SELECT (SELECT COUNT(*) FROM rewrite_requests) + (SELECT COUNT(*) FROM draft_revisions)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rewrite rows outlived their draft", n)
	}
}
