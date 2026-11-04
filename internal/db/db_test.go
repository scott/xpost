package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xpost.db")
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var mode string
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v), want wal", mode, err)
	}
	var fk int
	if err := d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d (%v)", fk, err)
	}
	for _, table := range []string{"users", "sessions", "webauthn_credentials", "agent_tokens", "drafts", "draft_parts", "part_media", "media", "draft_events", "post_log", "accounts", "settings"} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Errorf("table %s missing", table)
		}
	}
	// Reopening applies nothing twice.
	d.Close()
	d2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	ms, _ := Migrations()
	var n int
	d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if n != len(ms) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(ms))
	}
}

func TestOneOwnerOnly(t *testing.T) {
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO users(id, email, display_name, created_at) VALUES (1, 'a@example.com', 'A', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO users(id, email, display_name, created_at) VALUES (2, 'b@example.com', 'B', 0)`); err == nil {
		t.Fatal("a second owner row was accepted")
	}
}
