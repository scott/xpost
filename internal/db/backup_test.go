package db

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	d, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ExecContext(ctx, `CREATE TABLE sample(v TEXT); INSERT INTO sample VALUES ('example row')`); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "copy.db")
	if err := Backup(ctx, src, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v %v", fi.Mode(), err)
	}
	c, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var v string
	var n int
	if err := c.QueryRowContext(ctx, `SELECT v FROM sample`).Scan(&v); err != nil || v != "example row" {
		t.Fatalf("copy row %q %v", v, err)
	}
	if err := c.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("copy migrations %d %v", n, err)
	}
	// Never overwrites; a missing source is an error and creates nothing.
	if err := Backup(ctx, src, dest); err == nil {
		t.Fatal("overwrote an existing backup")
	}
	if err := Backup(ctx, filepath.Join(dir, "missing.db"), filepath.Join(dir, "x.db")); err == nil {
		t.Fatal("backup of a missing DB succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("backup created the source")
	}
	// A corrupt file fails the integrity check.
	bad := filepath.Join(dir, "bad.db")
	os.WriteFile(bad, []byte("not a database, example bytes"), 0o600)
	if err := IntegrityCheck(ctx, bad); err == nil {
		t.Fatal("garbage passed the integrity check")
	}
}
