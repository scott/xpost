package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// Backup writes a consistent copy of the database at src to dest with
// VACUUM INTO, checks the copy with PRAGMA integrity_check, and leaves it
// mode 0600. src is opened read-only and is never migrated; dest must not
// exist. On any failure dest is removed.
func Backup(ctx context.Context, src, dest string) (err error) {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("backup: %s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("backup: %w", err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	d, err := sql.Open("sqlite", "file:"+src+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			os.Remove(dest)
		}
	}()
	old := umask077()
	_, err = d.ExecContext(ctx, `VACUUM INTO ?`, dest)
	restoreUmask(old)
	if err != nil {
		return fmt.Errorf("backup: vacuum into: %w", err)
	}
	if err = os.Chmod(dest, 0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return IntegrityCheck(ctx, dest)
}

// IntegrityCheck opens path read-only and requires PRAGMA integrity_check
// to answer "ok".
func IntegrityCheck(ctx context.Context, path string) error {
	q := url.Values{}
	q.Set("mode", "ro")
	c, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	defer c.Close()
	var res string
	if err := c.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("integrity check: %s", res)
	}
	return nil
}
