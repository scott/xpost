package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0007 adds drafts.kind and accounts.kind (existing rows are X) and the
// blogs and blog_posts tables, without touching existing drafts.
func TestBlogsMigrationKeepsDrafts(t *testing.T) {
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
	raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`)
	for _, m := range ms {
		if m.Version >= 7 {
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
	if _, err := raw.Exec(`INSERT INTO drafts(id, account, status, proposed_at, scheduled_at, source, created_at, updated_at) VALUES (5, 'sample_writer', 'approved', 100, 100, 'sample-agent', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var kind, status string
	if err := d.QueryRow(`SELECT kind, status FROM drafts WHERE id = 5`).Scan(&kind, &status); err != nil || kind != "x" || status != "approved" {
		t.Fatalf("draft: %q %q %v", kind, status, err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM accounts WHERE kind = 'x'`).Scan(&n)
	if n != 2 {
		t.Fatalf("x accounts: %d", n)
	}
	if _, err := d.Exec(`UPDATE drafts SET kind = 'page' WHERE id = 5`); err == nil {
		t.Fatal("kind accepts a value other than x or blog")
	}
	if _, err := d.Exec(`INSERT INTO blogs(account, name, site_url, method, created_at, updated_at) VALUES ('sample_writer', 'n', 'https://n.example.test', 'ftp', 1, 1)`); err == nil {
		t.Fatal("method accepts a value other than api or webhook")
	}
}
