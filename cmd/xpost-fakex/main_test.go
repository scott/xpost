package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

// The fake X signs in as exactly the accounts seed-demo posts from.
func TestFakeUsersMatchDemo(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	k, _ := secret.NewKey()
	st := store.New(d, k)
	if _, _, err := st.CreateOwner(ctx, "owner@example.com", "Sample Owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := demo.Seed(ctx, st); err != nil {
		t.Fatal(err)
	}
	accts, err := st.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range accts {
		got = append(got, a.Handle)
	}
	if strings.Join(got, ",") != defaultUsers || defaultUsers != demo.HandleA+","+demo.HandleB {
		t.Fatalf("seed-demo accounts %v, fakex -users default %q", got, defaultUsers)
	}
	for _, h := range strings.Split(defaultUsers, ",") {
		var n int
		d.QueryRowContext(ctx, `SELECT COUNT(*) FROM drafts WHERE account = ?`, h).Scan(&n)
		if n == 0 {
			t.Errorf("demo data has no drafts for %s", h)
		}
	}
}
