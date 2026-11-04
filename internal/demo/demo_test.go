package demo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
)

// Demo data is invented: every draft and log preview says "Example".
func TestSeedIsExampleData(t *testing.T) {
	auth.DefaultParams = auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
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
	if _, err := Seed(ctx, st); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(`SELECT d.id, p.text FROM draft_parts p JOIN drafts d ON d.id = p.draft_id WHERE p.position = 0`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var id int64
		var text string
		rows.Scan(&id, &text)
		n++
		if !strings.HasPrefix(text, "Example") {
			t.Errorf("draft %d: %q", id, text)
		}
	}
	rows.Close()
	if n < 5 {
		t.Fatalf("%d demo drafts", n)
	}
	brows, err := d.Query(`SELECT d.status, p.title FROM blog_posts p JOIN drafts d ON d.id = p.draft_id`)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for brows.Next() {
		var status, title string
		brows.Scan(&status, &title)
		states[status]++
		if !strings.HasPrefix(title, "Example") {
			t.Errorf("blog post %q", title)
		}
	}
	brows.Close()
	if states["pending"] < 3 || states["approved"] != 1 || states["posted"] != 1 || states["failed"] != 1 {
		t.Fatalf("demo blog states %v", states)
	}
	// Promos: two pending, one approved early, one approved while its blog
	// is pending (waiting on blog), one held behind the failed publish.
	var pending, early, waiting, held int
	d.QueryRow(`SELECT COALESCE(SUM(p.status = 'pending'), 0), COALESCE(SUM(p.status = 'approved' AND b.status = 'approved'), 0),
		COALESCE(SUM(p.status = 'approved' AND b.status = 'pending'), 0), COALESCE(SUM(p.status = 'approved' AND b.status = 'failed'), 0)
		FROM drafts p JOIN drafts b ON b.id = p.promotes`).Scan(&pending, &early, &waiting, &held)
	if pending != 2 || early != 1 || waiting != 1 || held != 1 {
		t.Fatalf("demo promos: %d pending, %d early, %d waiting, %d held", pending, early, waiting, held)
	}
	// Replies: one per hand-off state, none ever in the post log.
	var rPending, rApproved, rOpened, rPosted, rLogged int
	d.QueryRow(`SELECT COALESCE(SUM(d.status = 'pending'), 0), COALESCE(SUM(d.status = 'approved' AND r.opened_at IS NULL), 0),
		COALESCE(SUM(d.status = 'approved' AND r.opened_at IS NOT NULL), 0), COALESCE(SUM(d.status = 'posted'), 0),
		(SELECT COUNT(*) FROM post_log l JOIN draft_replies x ON x.draft_id = l.draft_id) FROM draft_replies r JOIN drafts d ON d.id = r.draft_id`).Scan(&rPending, &rApproved, &rOpened, &rPosted, &rLogged)
	if rPending != 4 || rApproved != 1 || rOpened != 1 || rPosted != 2 || rLogged != 0 {
		t.Fatalf("demo replies: %d pending, %d approved, %d opened, %d posted, %d logged", rPending, rApproved, rOpened, rPosted, rLogged)
	}
	var notExample int
	d.QueryRow(`SELECT COUNT(*) FROM post_log WHERE example = 0`).Scan(&notExample)
	if notExample != 0 {
		t.Fatalf("%d log rows not marked example", notExample)
	}
}
