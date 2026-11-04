package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
)

// UpstreamPost is one post a blog's live site lists, as an import read it.
// Nothing here came from xpost: it is the site's word.
type UpstreamPost struct {
	Slug        string
	Title       string
	Description string
	Date        string // YYYY-MM-DD, as the site lists it
	URL         string // the live URL, checked to be on the blog's site
	Ref         string // xpost's reference for it ("gander:<slug>@<created_at>"); "" for webhook blogs
	Live        bool   // the site serves it publicly (webhook blogs: 200 HTML at the exact live URL)
	Why         string // why it is not live (status only, never a URL or body)
}

// Import actions.
const (
	ImportNew   = "import" // added as a Published post
	ImportAdopt = "adopt"  // a blocked draft of xpost's becomes Published (same title, no promos, live)
	ImportSkip  = "skip"
)

// MaxImport caps one import: a site listing more is refused whole.
const MaxImport = 200

// ImportLine says what an import does, or would do, with one post.
type ImportLine struct {
	Slug    string
	Title   string
	Date    string
	URL     string
	Action  string
	Reason  string
	DraftID int64    // the xpost draft already on this slug, if any
	Changed []string // fields the site shows differently from xpost (never applied)
	up      UpstreamPost
}

// ImportPlan is a dry run, or what an import did.
type ImportPlan struct {
	Lines              []ImportLine
	New, Adopted, Skip int
	Key                string // changes whenever the plan would
}

// ErrImportChanged refuses an import whose plan is not the one the owner
// dry ran: the site or xpost changed in between. Nothing was written.
var ErrImportChanged = errors.New("The site or xpost changed since the dry run. Nothing was imported. Check the new dry run, then import again.")

// ErrImportTooMany refuses a site that lists more posts than one import takes.
var ErrImportTooMany = fmt.Errorf("The site lists more than %d posts. Nothing was imported.", MaxImport)

var dayRe = func(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// PlanImport is the dry run: what importing ups into b would do. It reads
// xpost only and writes nothing.
func (s *Store) PlanImport(ctx context.Context, b *Blog, ups []UpstreamPost) (*ImportPlan, error) {
	return planImport(ctx, s.DB, b, ups)
}

type heldPost struct {
	id               int64
	status, errText  string
	title, desc, url string
	ref              string
	promos           int
}

func planImport(ctx context.Context, q querier, b *Blog, ups []UpstreamPost) (*ImportPlan, error) {
	if len(ups) > MaxImport {
		return nil, ErrImportTooMany
	}
	plan := &ImportPlan{}
	seen := map[string]bool{}
	for _, u := range ups {
		u.Slug, u.Title, u.Description = strings.TrimSpace(u.Slug), strings.TrimSpace(u.Title), strings.TrimSpace(u.Description)
		ln := ImportLine{Slug: u.Slug, Title: u.Title, Date: u.Date, URL: u.URL, Action: ImportSkip, up: u}
		skip := func(why string) {
			ln.Reason = why
			plan.Lines = append(plan.Lines, ln)
		}
		switch {
		case !blogtext.ValidSlug(u.Slug):
			ln.Slug = ""
			skip("The site lists a post whose slug xpost can't use.")
			continue
		case seen[u.Slug]:
			skip("The site lists this slug more than once.")
			continue
		}
		seen[u.Slug] = true
		switch {
		case u.Title == "" || !oneLine(u.Title) || len([]rune(u.Title)) > MaxTitle:
			skip("The title is empty, more than one line or too long.")
			continue
		case !oneLine(u.Description) || len([]rune(u.Description)) > MaxDescription:
			skip("The description is more than one line or too long.")
			continue
		case !dayRe(u.Date):
			skip("The date is not a YYYY-MM-DD day.")
			continue
		case !u.Live:
			why := u.Why
			if why == "" {
				why = "it is not public."
			}
			skip("Not live: " + why)
			continue
		case u.URL == "":
			skip("The site gave no live URL for it.")
			continue
		}
		var h heldPost
		err := q.QueryRowContext(ctx, `SELECT d.id, d.status, d.error, bp.title, bp.description, bp.published_url, bp.ref,
			(SELECT COUNT(*) FROM drafts p WHERE p.promotes = d.id AND p.status != 'rejected')
			FROM blog_posts bp JOIN drafts d ON d.id = bp.draft_id WHERE bp.blog_id = ? AND bp.slug = ?`, b.ID, u.Slug).
			Scan(&h.id, &h.status, &h.errText, &h.title, &h.desc, &h.url, &h.ref, &h.promos)
		if errors.Is(err, sql.ErrNoRows) {
			ln.Action, ln.Reason = ImportNew, "Live on the site and not in xpost yet."
			plan.Lines = append(plan.Lines, ln)
			continue
		}
		if err != nil {
			return nil, err
		}
		ln.DraftID = h.id
		if h.status == StatusPosted {
			// What the site shows differently is reported, never applied.
			if h.title != u.Title {
				ln.Changed = append(ln.Changed, "title")
			}
			if h.desc != u.Description {
				ln.Changed = append(ln.Changed, "description")
			}
			if h.url != u.URL {
				ln.Changed = append(ln.Changed, "live URL")
			}
			skip(fmt.Sprintf("Already in xpost as published post #%d. Nothing is changed.", h.id))
			continue
		}
		blocked := h.status == StatusFailed && (strings.HasPrefix(h.errText, BlogSlugTakenPrefix) || strings.Contains(h.errText, BlogSlugTaken))
		switch {
		case !blocked:
			skip(fmt.Sprintf("Draft #%d (%s) uses this slug in xpost.", h.id, statusWord(h.status)))
		case b.Method == MethodAPI && u.Ref == "":
			skip(fmt.Sprintf("Draft #%d is blocked on this slug, but the blog gave no reference for the live post.", h.id))
		case h.title != u.Title:
			skip(fmt.Sprintf("Draft #%d is blocked on this slug, but its title differs from the live post. Rename or reject it.", h.id))
		case h.promos > 0:
			skip(fmt.Sprintf("Draft #%d is blocked on this slug and has promos. Reject them first: import never starts a promo.", h.id))
		case b.Method == MethodAPI && h.ref != "" && h.ref != u.Ref:
			skip(fmt.Sprintf("Draft #%d is blocked on this slug and was staged as another post.", h.id))
		default:
			ln.Action, ln.Reason = ImportAdopt, fmt.Sprintf("Draft #%d is blocked by this live post with the same title. It becomes Published.", h.id)
			plan.Lines = append(plan.Lines, ln)
		}
	}
	hsh := sha256.New()
	fmt.Fprintf(hsh, "%d\n", b.ID)
	for _, ln := range plan.Lines {
		switch ln.Action {
		case ImportNew:
			plan.New++
		case ImportAdopt:
			plan.Adopted++
		default:
			plan.Skip++
		}
		fmt.Fprintf(hsh, "%q %q %d %q %q %q %q %q\n", ln.Action, ln.Slug, ln.DraftID, ln.up.Title, ln.up.Description, ln.up.Date, ln.up.URL, ln.up.Ref)
	}
	plan.Key = hex.EncodeToString(hsh.Sum(nil))[:24]
	return plan, nil
}

func statusWord(st string) string {
	switch st {
	case StatusPosting:
		return "publishing"
	case StatusPosted:
		return "published"
	}
	return st
}

// ApplyImport imports ups into b, but only when the plan is still the one
// with key (the owner's dry run). New posts are added as Published with no
// body and no promos; blocked drafts the plan adopts become Published. It
// never updates a post already in xpost, and never calls a blog or X.
func (s *Store) ApplyImport(ctx context.Context, u *User, b *Blog, ups []UpstreamPost, key string, loc *time.Location) (*ImportPlan, error) {
	if u == nil {
		return nil, ErrNotFound
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := planImport(ctx, tx, b, ups)
	if err != nil {
		return nil, err
	}
	if key == "" || plan.Key != key {
		return plan, ErrImportChanged
	}
	now := s.now()
	zone := b.Location(loc)
	for _, ln := range plan.Lines {
		switch ln.Action {
		case ImportNew:
			// The site gives a day only: noon in the blog's zone keeps that
			// day in nearby zones too.
			d0, _ := time.Parse("2006-01-02", ln.up.Date)
			day := time.Date(d0.Year(), d0.Month(), d0.Day(), 12, 0, 0, 0, zone)
			res, err := tx.ExecContext(ctx, `INSERT INTO drafts(kind, account, status, proposed_at, scheduled_at, source, created_at, updated_at)
				VALUES ('blog', ?, 'posted', ?, ?, 'import', ?, ?)`, b.Account, day.Unix(), day.Unix(), now, now)
			if err != nil {
				return nil, err
			}
			id, _ := res.LastInsertId()
			if _, err := tx.ExecContext(ctx, `INSERT INTO blog_posts(draft_id, blog_id, slug, title, description, tags, markdown, published_url, ref, imported_at)
				VALUES (?,?,?,?,?,'[]','',?,?,?)`, id, b.ID, ln.Slug, ln.up.Title, ln.up.Description, ln.up.URL, ln.up.Ref, now); err != nil {
				return nil, err
			}
			if err := event(ctx, tx, id, now, ownerActor, "imported", "From the live site: "+ln.up.URL); err != nil {
				return nil, err
			}
		case ImportAdopt:
			res, err := tx.ExecContext(ctx, `UPDATE drafts SET status = 'posted', error = '', lease_until = 0, version = version + 1, updated_at = ?
				WHERE id = ? AND status = 'failed' AND kind = 'blog'`, now, ln.DraftID)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return nil, ErrConflict
			}
			// gander: xpost's ref for the live post. Webhook blogs have no
			// site reference, so the ref xpost had is kept; the live URL and
			// adopted_at record the match.
			if _, err := tx.ExecContext(ctx, `UPDATE blog_posts SET published_url = ?, ref = CASE WHEN ? != '' THEN ? ELSE ref END, adopted_at = ? WHERE draft_id = ?`,
				ln.up.URL, ln.up.Ref, ln.up.Ref, now, ln.DraftID); err != nil {
				return nil, err
			}
			if err := event(ctx, tx, ln.DraftID, now, ownerActor, "imported", "Adopted the live post: "+ln.up.URL); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return plan, nil
}
