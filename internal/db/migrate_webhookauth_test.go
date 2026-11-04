package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 0006 adds agent_webhooks.auth_sealed; webhooks saved before it keep
// their URL and key and have no Authorization value.
func TestWebhookAuthMigrationKeepsData(t *testing.T) {
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
		if m.Version >= 6 {
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
	if _, err := raw.Exec(`INSERT INTO agent_tokens(id, name, token_hash, last4, scopes, created_at) VALUES (1, 'sample-agent', 'h', 'abcd', 'drafts:write', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO agent_webhooks(token_id, url, key_sealed, key_last4, updated_at) VALUES (1, 'https://hooks.example.test/x', x'0102', 'wxyz', 1)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	d, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var u, last4 string
	var auth []byte
	if err := d.QueryRow(`SELECT url, key_last4, auth_sealed FROM agent_webhooks WHERE token_id = 1`).Scan(&u, &last4, &auth); err != nil {
		t.Fatal(err)
	}
	if u != "https://hooks.example.test/x" || last4 != "wxyz" || auth != nil {
		t.Fatalf("row changed: %q %q %v", u, last4, auth)
	}
}
