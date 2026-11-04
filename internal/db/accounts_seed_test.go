package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldSeedSQL is the account seed 0001 carried until open source prep:
// existing databases ran it, so their two accounts are real data.
const oldSeedSQL = `INSERT INTO accounts(handle, initials, color, position) VALUES ('sample_writer', 'SW', 'sm', 1), ('sample_maker', 'SM', 'jg', 2)`

func oldSeed(t *testing.T, raw *sql.DB) {
	t.Helper()
	if _, err := raw.Exec(oldSeedSQL); err != nil {
		t.Fatal(err)
	}
}

func countAccounts(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM accounts WHERE kind = 'x'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A fresh database has no X accounts.
func TestFreshInstallSeedsNoAccounts(t *testing.T) {
	d, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if n := countAccounts(t, d); n != 0 {
		t.Fatalf("fresh install has %d accounts", n)
	}
}

// A database that ran the old 0001 (with its seed) keeps those accounts
// through every later migration: no migration deletes accounts.
func TestOldSeededAccountsSurvive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`)
	if _, err := raw.Exec(ms[0].SQL); err != nil {
		t.Fatal(err)
	}
	oldSeed(t, raw)
	raw.Exec(`INSERT INTO schema_migrations VALUES (?, ?, ?)`, ms[0].Version, ms[0].Name, time.Now().Unix())
	raw.Close()
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var got string
	d.QueryRow(`SELECT group_concat(handle, ',') FROM (SELECT handle FROM accounts WHERE kind = 'x' ORDER BY position)`).Scan(&got)
	if got != "sample_writer,sample_maker" {
		t.Fatalf("accounts after all migrations: %q", got)
	}
	for _, m := range ms {
		if containsDeleteAccounts(m.SQL) {
			t.Errorf("migration %s deletes accounts", m.Name)
		}
	}
}

func containsDeleteAccounts(sql string) bool {
	u := strings.ToUpper(sql)
	return strings.Contains(u, "DELETE FROM ACCOUNTS") || strings.Contains(u, "DROP TABLE ACCOUNTS")
}
