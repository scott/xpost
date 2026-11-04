package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
)

// Draft kinds.
const (
	KindX    = "x"
	KindBlog = "blog"
)

// Blog publish methods.
const (
	MethodAPI     = "api"     // a publisher REST API: base URL plus a sealed token
	MethodWebhook = "webhook" // a signed webhook to an agent (like the rewrite webhook)
)

// Blog test states. A blog publishes only while its last test passed.
const (
	TestUntested = "untested"
	TestPassed   = "passed"
	TestFailed   = "failed"
)

// Sealed blog fields. Each is bound to its blog and field.
const (
	BlogAPIToken = "api_token"
	BlogHookURL  = "hook_url"
	BlogHookKey  = "hook_key"
	BlogHookAuth = "hook_auth"
)

var blogSecretCols = map[string]string{BlogAPIToken: "api_token_sealed", BlogHookURL: "hook_url_sealed", BlogHookKey: "hook_key_sealed", BlogHookAuth: "hook_auth_sealed"}

func blogAAD(field string, id int64) []byte {
	return []byte("blogs." + field + ":" + strconv.FormatInt(id, 10))
}

// BlogAccountPrefix starts the destination handle a blog's drafts point
// at ("blog:<id>"); X handles can't contain a colon.
const BlogAccountPrefix = "blog:"

// Limits for blog settings.
const (
	MaxBlogs        = 20
	MaxTopics       = 50
	MaxTopicLen     = 60
	DefaultPubClock = "07:00"
)

// blogColors are the avatar colours new blogs get in turn (CSS classes).
var blogColors = []string{"sm", "jg", "bo", "bp", "bb", "bg"}

// Blog is a destination xpost publishes blog posts to. Secrets are never
// readable from here: only whether each is set.
type Blog struct {
	ID               int64
	Account          string // the destination row drafts point at ("blog:<id>")
	Name             string // e.g. "notes.example.com"
	SiteURL          string
	Method           string
	APIBase          string
	HasAPIToken      bool
	HasHookURL       bool
	HasHookKey       bool
	HasHookAuth      bool
	PublishClock     string // HH:MM in the blog's zone
	Zone             string // IANA zone for the default time and the local date; "" = xpost's zone
	PublishTokenID   int64  // the agent token that pulls and reports publishes (webhook blogs); 0 = none
	PromoAccount     string // an X handle, or ""
	Rules            blogtext.Rules
	CoverAltRequired bool
	TestState        string
	TestedAt         *time.Time
	TestDetail       string
	TestMs           int
	Color            string
	Paused           bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Location is the blog's time zone, or fallback (xpost's zone) when the
// blog has none or it no longer loads.
func (b *Blog) Location(fallback *time.Location) *time.Location {
	if b.Zone != "" {
		if l, err := time.LoadLocation(b.Zone); err == nil {
			return l
		}
	}
	return fallback
}

// Ready reports whether the blog may publish: its last test passed.
func (b *Blog) Ready() bool { return b.TestState == TestPassed }

// HasConnection reports whether the method's connection details are set:
// the API base and token, or the webhook URL and signing key.
func (b *Blog) HasConnection() bool {
	if b.Method == MethodAPI {
		return b.APIBase != "" && b.HasAPIToken
	}
	return b.HasHookURL && b.HasHookKey
}

// ClockLabel is the default publish time as "7:00 AM".
func (b *Blog) ClockLabel() string {
	t, err := time.Parse("15:04", b.PublishClock)
	if err != nil {
		return b.PublishClock
	}
	return t.Format("3:04 PM")
}

// LiveURL is where a published post lives: <site>/blog/<slug>/ on a
// webhook blog, and <site>/blog/<slug> (no trailing slash, as gander
// serves it) on a Publisher API blog.
func (b *Blog) LiveURL(slug string) string {
	u := strings.TrimRight(b.SiteURL, "/") + "/blog/" + slug
	if b.Method == MethodAPI {
		return u
	}
	return u + "/"
}

// BlogSlugTaken starts the publish error when the slug is already used on
// the blog by a post xpost did not create; the card shows it as blocked.
const BlogSlugTaken = "A post with this slug already exists on gander."

// BlogSlugTakenPrefix starts every "slug is taken" publish error, whatever
// the blog (gander, or a webhook agent's slug_exists report).
const BlogSlugTakenPrefix = "A post with this slug already exists on "

// ErrNoPublisher is a Test or publish for a blog whose publish method
// has no publisher on this server: no call was made.
var ErrNoPublisher = errors.New("Testing is not available on this server yet: no publisher runs here for this publish method.")

// ErrLiveOff is a Test or publish on a blog while live
// publishing is switched off on this server: no call was made.
var ErrLiveOff = errors.New("Live publishing is disabled on this server. Test made no call and nothing was sent.")

const blogCols = `b.id, b.account, b.name, b.site_url, b.method, b.api_base, b.api_token_sealed IS NOT NULL, b.hook_url_sealed IS NOT NULL, b.hook_key_sealed IS NOT NULL,
	b.hook_auth_sealed IS NOT NULL, b.publish_clock, b.zone, COALESCE(b.publish_token_id, 0), COALESCE(b.promo_account, ''), b.banned_topics, b.lint_en, b.lint_banned, b.lint_h1, b.cover_alt_required,
	b.test_state, b.tested_at, b.test_detail, b.test_ms, a.color, a.paused, b.created_at, b.updated_at`

const blogFrom = ` FROM blogs b JOIN accounts a ON a.handle = b.account`

func scanBlog(r scanner) (*Blog, error) {
	var b Blog
	var topics, en, banned, h1 string
	var cover, paused int
	var tested sql.NullInt64
	var created, updated int64
	if err := r.Scan(&b.ID, &b.Account, &b.Name, &b.SiteURL, &b.Method, &b.APIBase, &b.HasAPIToken, &b.HasHookURL, &b.HasHookKey, &b.HasHookAuth, &b.PublishClock,
		&b.Zone, &b.PublishTokenID, &b.PromoAccount, &topics, &en, &banned, &h1, &cover, &b.TestState, &tested, &b.TestDetail, &b.TestMs, &b.Color, &paused, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	json.Unmarshal([]byte(topics), &b.Rules.Topics)
	b.Rules.EnDash, b.Rules.Banned, b.Rules.LeadingH1 = blogtext.Level(en), blogtext.Level(banned), blogtext.Level(h1)
	b.CoverAltRequired, b.Paused = cover == 1, paused == 1
	b.TestedAt = tptr(tested)
	b.CreatedAt, b.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &b, nil
}

// Blogs lists the blogs that are set up (not removed), oldest first.
func (s *Store) Blogs(ctx context.Context) ([]*Blog, error) {
	if s.NoBlogs {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+blogCols+blogFrom+` WHERE b.removed_at IS NULL ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Blog
	for rows.Next() {
		b, err := scanBlog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BlogByID loads a blog that is not removed.
func (s *Store) BlogByID(ctx context.Context, id int64) (*Blog, error) {
	return scanBlog(s.DB.QueryRowContext(ctx, `SELECT `+blogCols+blogFrom+` WHERE b.id = ? AND b.removed_at IS NULL`, id))
}

// BlogForAccount loads the blog behind a destination row, removed or not
// (a removed blog's published posts still show it).
func (s *Store) BlogForAccount(ctx context.Context, account string) (*Blog, error) {
	return scanBlog(s.DB.QueryRowContext(ctx, `SELECT `+blogCols+blogFrom+` WHERE b.account = ?`, account))
}

// BlogByName finds a blog that is not removed by its name (any case).
func (s *Store) BlogByName(ctx context.Context, name string) (*Blog, error) {
	return scanBlog(s.DB.QueryRowContext(ctx, `SELECT `+blogCols+blogFrom+` WHERE b.name = ? COLLATE NOCASE AND b.removed_at IS NULL`, strings.TrimSpace(name)))
}

// BlogIn is a blog's plain settings, as the owner saves them. Secrets are
// set separately (SetBlogSecret).
type BlogIn struct {
	Name             string
	SiteURL          string
	Method           string
	APIBase          string
	PublishClock     string // HH:MM; "" means 07:00
	Zone             string // IANA zone, or "" for xpost's zone
	PublishTokenID   int64  // webhook blogs: the blogs:publish token, or 0
	PromoAccount     string // an X handle or ""
	Topics           []string
	EnDash           blogtext.Level
	Banned           blogtext.Level
	LeadingH1        blogtext.Level
	CoverAltRequired bool
}

func cleanHTTPS(raw, what string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badInput("enter the " + what)
	}
	if len(raw) > 500 {
		return "", badInput("the " + what + " is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", badInput("the " + what + " must be an https:// address with no query")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func (s *Store) cleanBlogIn(ctx context.Context, in BlogIn) (BlogIn, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 60 || strings.ContainsAny(in.Name, "\r\n\t") {
		return in, badInput("a blog name of 1 to 60 characters is required")
	}
	var err error
	if in.SiteURL, err = cleanHTTPS(in.SiteURL, "site URL"); err != nil {
		return in, err
	}
	switch in.Method {
	case MethodAPI:
		if strings.TrimSpace(in.APIBase) != "" {
			if in.APIBase, err = cleanHTTPS(in.APIBase, "API base URL"); err != nil {
				return in, err
			}
			// The site root only: xpost adds the Publisher API path itself.
			if u, _ := url.Parse(in.APIBase); u == nil || strings.Trim(u.Path, "/") != "" {
				return in, badInput("the API base URL must be the site root, like https://example.com, with no path")
			}
		}
	case MethodWebhook:
		in.APIBase = ""
	default:
		return in, badInput("pick a publish method: Publisher API or Webhook to agent")
	}
	if in.PublishClock == "" {
		in.PublishClock = DefaultPubClock
	}
	if _, err := time.Parse("15:04", in.PublishClock); err != nil || len(in.PublishClock) != 5 {
		return in, badInput("pick a default publish time")
	}
	in.Zone = strings.TrimSpace(in.Zone)
	if in.Zone != "" {
		if len(in.Zone) > 64 || strings.ContainsAny(in.Zone, " \t\r\n") {
			return in, badInput("the time zone must be an IANA name such as Europe/London")
		}
		if l, err := time.LoadLocation(in.Zone); err != nil || in.Zone == "Local" {
			return in, badInput("unknown time zone " + in.Zone + "; use an IANA name such as Europe/London")
		} else {
			in.Zone = l.String()
		}
	}
	if in.Method != MethodWebhook {
		in.PublishTokenID = 0
	}
	if in.PublishTokenID != 0 {
		t, err := s.AgentTokenByID(ctx, in.PublishTokenID)
		if err != nil || t.RevokedAt != nil || !t.Has(ScopeBlogsPublish) {
			return in, badInput("pick an active token with the " + ScopeBlogsPublish + " scope as the publishing token")
		}
	}
	in.PromoAccount = strings.TrimPrefix(strings.TrimSpace(in.PromoAccount), "@")
	if in.PromoAccount != "" && !s.accountExists(ctx, in.PromoAccount) {
		return in, badInput("unknown X account @" + in.PromoAccount)
	}
	var topics []string
	seen := map[string]bool{}
	for _, t := range in.Topics {
		t = strings.Join(strings.Fields(t), " ")
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		if len([]rune(t)) > MaxTopicLen {
			return in, badInput(fmt.Sprintf("a banned topic can be up to %d characters", MaxTopicLen))
		}
		seen[strings.ToLower(t)] = true
		topics = append(topics, t)
	}
	if len(topics) > MaxTopics {
		return in, badInput(fmt.Sprintf("up to %d banned topics", MaxTopics))
	}
	in.Topics = topics
	def := blogtext.DefaultRules()
	for _, p := range []struct {
		l   *blogtext.Level
		def blogtext.Level
	}{{&in.EnDash, def.EnDash}, {&in.Banned, def.Banned}, {&in.LeadingH1, def.LeadingH1}} {
		if *p.l == "" {
			*p.l = p.def
		}
		if !blogtext.ValidLevel(string(*p.l)) {
			return in, badInput("a lint rule is Block or Warn")
		}
	}
	return in, nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateBlog adds a blog. It starts Untested, so it can't publish until a
// test passes.
func (s *Store) CreateBlog(ctx context.Context, u *User, in BlogIn) (*Blog, error) {
	if u == nil {
		return nil, ErrNotFound
	}
	in, err := s.cleanBlogIn(ctx, in)
	if err != nil {
		return nil, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var live, total int64
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blogs WHERE removed_at IS NULL`).Scan(&live)
	if live >= MaxBlogs {
		return nil, badInput(fmt.Sprintf("up to %d blogs", MaxBlogs))
	}
	var dup int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM blogs WHERE name = ? COLLATE NOCASE AND removed_at IS NULL`, in.Name).Scan(&dup)
	if dup > 0 {
		return nil, badInput("there is already a blog named " + in.Name)
	}
	// Ids are never reused (removed blogs keep their row), so the
	// destination handle "blog:<id>" is unique for good.
	tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM blogs`).Scan(&total)
	id := total + 1
	handle := BlogAccountPrefix + strconv.FormatInt(id, 10)
	now := s.now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(handle, initials, color, position, kind) VALUES (?, '', ?, ?, 'blog')`,
		handle, blogColors[int(total)%len(blogColors)], 1000+id); err != nil {
		return nil, err
	}
	topics, _ := json.Marshal(in.Topics)
	cover := 0
	if in.CoverAltRequired {
		cover = 1
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO blogs(id, account, name, site_url, method, api_base, publish_clock, zone, publish_token_id, promo_account, banned_topics, lint_en, lint_banned, lint_h1,
		cover_alt_required, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, handle, in.Name, in.SiteURL, in.Method, in.APIBase, in.PublishClock, in.Zone, nullID(in.PublishTokenID), nullIf(in.PromoAccount), string(topics), in.EnDash, in.Banned, in.LeadingH1, cover, now, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.BlogByID(ctx, id)
}

// UpdateBlog saves a blog's plain settings. Changing the method, the site
// URL or the API base URL sets the blog back to Untested.
func (s *Store) UpdateBlog(ctx context.Context, u *User, id int64, in BlogIn) (*Blog, error) {
	if u == nil {
		return nil, ErrNotFound
	}
	old, err := s.BlogByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if in, err = s.cleanBlogIn(ctx, in); err != nil {
		return nil, err
	}
	var dup int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM blogs WHERE name = ? COLLATE NOCASE AND removed_at IS NULL AND id != ?`, in.Name, id).Scan(&dup)
	if dup > 0 {
		return nil, badInput("there is already a blog named " + in.Name)
	}
	topics, _ := json.Marshal(in.Topics)
	cover := 0
	if in.CoverAltRequired {
		cover = 1
	}
	now := s.now()
	if _, err := s.DB.ExecContext(ctx, `UPDATE blogs SET name = ?, site_url = ?, method = ?, api_base = ?, publish_clock = ?, zone = ?, publish_token_id = ?, promo_account = ?, banned_topics = ?,
		lint_en = ?, lint_banned = ?, lint_h1 = ?, cover_alt_required = ?, updated_at = ? WHERE id = ? AND removed_at IS NULL`,
		in.Name, in.SiteURL, in.Method, in.APIBase, in.PublishClock, in.Zone, nullID(in.PublishTokenID), nullIf(in.PromoAccount), string(topics), in.EnDash, in.Banned, in.LeadingH1, cover, now, id); err != nil {
		return nil, err
	}
	if old.Method != in.Method || old.SiteURL != in.SiteURL || old.APIBase != in.APIBase {
		if err := s.resetBlogTest(ctx, id); err != nil {
			return nil, err
		}
	}
	return s.BlogByID(ctx, id)
}

func (s *Store) resetBlogTest(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE blogs SET test_state = 'untested', tested_at = NULL, test_detail = '', test_ms = 0, updated_at = ? WHERE id = ?`, s.now(), id)
	return err
}

// CleanBlogSecret checks a secret for its field. The Authorization header
// follows the rewrite webhook's rules (one leading "Authorization:" is
// removed).
func CleanBlogSecret(field, v string) (string, error) {
	switch field {
	case BlogHookAuth:
		return CleanWebhookAuth(v)
	case BlogHookURL:
		v = strings.TrimSpace(v)
		if v == "" {
			return "", nil
		}
		return cleanHTTPS(v, "publish webhook URL")
	case BlogHookKey:
		v = strings.TrimSpace(v)
		if v != "" && (len(v) < 16 || len(v) > 512 || strings.ContainsAny(v, " \t\r\n")) {
			return "", badInput("the sender key should be 16 to 512 characters with no spaces")
		}
		return v, nil
	case BlogAPIToken:
		v = strings.TrimSpace(v)
		if strings.ContainsAny(v, "\r\n") {
			return "", badInput("the API token must be one line: remove the line break")
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return "", badInput("the API token contains a control character")
			}
		}
		if len(v) > 4096 {
			return "", badInput("the API token is too long")
		}
		return v, nil
	}
	return "", ErrBadInput
}

// SetBlogSecret seals and saves one connection secret. Any change sets
// the blog back to Untested. An empty value is refused (Clear removes).
func (s *Store) SetBlogSecret(ctx context.Context, u *User, id int64, field, value string) error {
	if u == nil {
		return ErrNotFound
	}
	col, ok := blogSecretCols[field]
	if !ok {
		return ErrBadInput
	}
	v, err := CleanBlogSecret(field, value)
	if err != nil {
		return err
	}
	if v == "" {
		return badInput("paste a value to save, or use Clear")
	}
	if _, err := s.BlogByID(ctx, id); err != nil {
		return err
	}
	sealed, err := s.Key.Seal([]byte(v), blogAAD(field, id))
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE blogs SET `+col+` = ? WHERE id = ? AND removed_at IS NULL`, sealed, id); err != nil {
		return err
	}
	return s.resetBlogTest(ctx, id)
}

// ClearBlogSecret removes one connection secret and sets the blog back to
// Untested.
func (s *Store) ClearBlogSecret(ctx context.Context, u *User, id int64, field string) error {
	if u == nil {
		return ErrNotFound
	}
	col, ok := blogSecretCols[field]
	if !ok {
		return ErrBadInput
	}
	if _, err := s.BlogByID(ctx, id); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE blogs SET `+col+` = NULL WHERE id = ? AND removed_at IS NULL`, id); err != nil {
		return err
	}
	return s.resetBlogTest(ctx, id)
}

// BlogSecret opens one connection secret for a publish adapter; "" when it
// is not set. Callers must never log or render it.
func (s *Store) BlogSecret(ctx context.Context, id int64, field string) (string, error) {
	col, ok := blogSecretCols[field]
	if !ok {
		return "", ErrBadInput
	}
	var sealed []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT `+col+` FROM blogs WHERE id = ? AND removed_at IS NULL`, id).Scan(&sealed); err != nil {
		return "", ErrNotFound
	}
	if sealed == nil {
		return "", nil
	}
	v, err := s.Key.Open(sealed, blogAAD(field, id))
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// RecordBlogTest stores a dry-run Test's outcome. detail is short (a
// status or "timeout"), never a response body or a secret.
func (s *Store) RecordBlogTest(ctx context.Context, u *User, id int64, ok bool, detail string, ms int) error {
	if u == nil {
		return ErrNotFound
	}
	state := TestFailed
	if ok {
		state = TestPassed
	}
	detail = s.CleanBlogTestDetail(ctx, id, detail)
	res, err := s.DB.ExecContext(ctx, `UPDATE blogs SET test_state = ?, tested_at = ?, test_detail = ?, test_ms = ? WHERE id = ? AND removed_at IS NULL`, state, s.now(), detail, ms, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BlogBusyError refuses removing a blog that still has posts on the way.
type BlogBusyError struct {
	Name                         string
	Pending, Approved, Scheduled int // pending includes failed posts (they wait in Pending); scheduled counts approved posts with a future time plus any publishing now
}

func (e *BlogBusyError) Total() int { return e.Pending + e.Approved + e.Scheduled }

func (e *BlogBusyError) Error() string {
	return fmt.Sprintf("Can't remove %s yet: %d posts are pending, approved or scheduled.", e.Name, e.Total())
}

func (e *BlogBusyError) Unwrap() error { return ErrConflict }

// BlogBusy counts a blog's posts that block removing it. Approved posts
// whose time is still ahead count as scheduled; approved posts that are
// due (or publishing) count as approved.
func (s *Store) BlogBusy(ctx context.Context, b *Blog) *BlogBusyError {
	return s.blogBusy(ctx, s.DB, b)
}

func (s *Store) blogBusy(ctx context.Context, q querier, b *Blog) *BlogBusyError {
	e := &BlogBusyError{Name: b.Name}
	now := s.now()
	q.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status IN ('pending','failed')), 0),
		COALESCE(SUM((status = 'approved' AND scheduled_at <= ?) OR status = 'posting'), 0),
		COALESCE(SUM(status IN ('approved','scheduled') AND scheduled_at > ?), 0)
		FROM drafts WHERE account = ?`, now, now, b.Account).Scan(&e.Pending, &e.Approved, &e.Scheduled)
	return e
}

// RemoveBlog stops publishing to a blog and deletes its sealed secrets.
// It is refused while the blog has pending, approved, scheduled or
// publishing posts. Published history stays (the row is kept, paused).
func (s *Store) RemoveBlog(ctx context.Context, u *User, id int64) error {
	if u == nil {
		return ErrNotFound
	}
	b, err := s.BlogByID(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Inside the transaction (an immediate one: it holds the write lock),
	// so no post can arrive between the check and the removal.
	if busy := s.blogBusy(ctx, tx, b); busy.Total() > 0 {
		return busy
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE blogs SET removed_at = ?, api_token_sealed = NULL, hook_url_sealed = NULL, hook_key_sealed = NULL, hook_auth_sealed = NULL,
		test_state = 'untested', updated_at = ? WHERE id = ?`, now, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET paused = 1 WHERE handle = ?`, b.Account); err != nil {
		return err
	}
	return tx.Commit()
}

// DefaultPublishAt is the next time after now at the blog's default clock
// time in loc (the owner's display zone).
func DefaultPublishAt(now time.Time, clock string, loc *time.Location) time.Time {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		t, _ = time.Parse("15:04", DefaultPubClock)
	}
	l := now.In(loc)
	at := time.Date(l.Year(), l.Month(), l.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	for !at.After(now) {
		l = l.AddDate(0, 0, 1)
		at = time.Date(l.Year(), l.Month(), l.Day(), t.Hour(), t.Minute(), 0, 0, loc)
	}
	return at
}
