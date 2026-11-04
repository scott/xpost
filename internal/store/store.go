// Package store is xpost's data layer: the owner login, sessions, passkeys,
// agent tokens, the draft queue and its state machine, media and the post
// log.
//
// Who may do what is enforced here, not in templates: agent methods take an
// *AgentToken and can only create pending drafts, read their own drafts and
// delete their own pending ones. Approving, editing, rescheduling and
// rejecting take the owner's *User, which only a full (password + passkey)
// session yields. Posting is the scheduler's (ClaimDue and friends), and no
// HTTP route reaches it.
package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/secret"
)

// Errors returned by the store. Handlers map ErrNotFound to 404.
var (
	ErrNotFound    = errors.New("not found")
	ErrBadInput    = errors.New("invalid input")
	ErrConflict    = errors.New("conflict")
	ErrStale       = errors.New("this draft changed since you opened it; reload and try again")
	ErrOwnerExists = errors.New("an owner already exists")
	ErrLinkInvalid = errors.New("this setup link is invalid, used or expired")
)

// InputError is a validation problem with a message safe to show.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }
func (e *InputError) Unwrap() error { return ErrBadInput }

func badInput(msg string) error { return &InputError{Msg: msg} }

// Lifetimes.
const (
	SetupTTL       = 72 * time.Hour
	StepSessionTTL = 10 * time.Minute // password done, passkey owed; or enrolling
	SessionTTL     = 24 * time.Hour   // full sessions: re-sign-in daily
	SessionIdleTTL = 8 * time.Hour
	FreshSignInTTL = 10 * time.Minute // adding or removing a passkey needs a sign-in this recent
	ChallengeTTL   = 5 * time.Minute
	MinSpacing     = 15 * time.Minute
	SlotStep       = 15 * time.Minute
	MaxParts       = 25
	MaxImages      = 4
	MaxMediaBytes  = 5 << 20
	MaxNoteLen     = 500
	MaxReasonLen   = 500
	MaxAltLen      = 1000 // X's alt text limit
	MaxPasskeys    = 10
)

// Store wraps the database, the master key and a clock.
type Store struct {
	DB    *sql.DB
	Key   *secret.Key
	Now   func() time.Time
	Rates Rates // per-part cost estimates from config; zero means unverified
	// NoBlogs hides the optional blog feature (XPOST_BLOGS_ENABLED off):
	// no blogs are listed, and blog drafts are left out of the queue, its
	// counts and the schedule. Rows already stored are kept, untouched.
	NoBlogs bool
}

// New returns a Store using the real clock.
func New(d *sql.DB, k *secret.Key) *Store { return &Store{DB: d, Key: k, Now: time.Now} }

func (s *Store) now() int64 { return s.Now().Unix() }

type scanner interface{ Scan(...any) error }

func tptr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0)
	return &t
}

// User is the owner's login. There is exactly one per install.
type User struct {
	ID           int64
	Email        string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
}

const userCols = `id, email, display_name, password_hash, created_at`

func scanUser(r scanner) (*User, error) {
	var u User
	var created int64
	if err := r.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	return &u, nil
}

// UserByID loads the owner by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// UserByEmail loads the owner by email (case-insensitive).
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, strings.TrimSpace(email)))
}

// Owner returns the install's owner, or ErrNotFound before create-owner.
func (s *Store) Owner(ctx context.Context) (*User, error) { return s.UserByID(ctx, 1) }

// CreateOwner creates the single owner and returns a one-time setup link
// token. The owner sets a password and registers a passkey through it.
func (s *Store) CreateOwner(ctx context.Context, email, name string) (*User, string, error) {
	email, name = strings.TrimSpace(email), strings.TrimSpace(name)
	if !strings.Contains(email, "@") || len(email) > 254 {
		return nil, "", badInput("a valid email is required")
	}
	if name == "" || len(name) > 80 {
		return nil, "", badInput("a display name of 1 to 80 characters is required")
	}
	if _, err := s.Owner(ctx); err == nil {
		return nil, "", ErrOwnerExists
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO users(id, email, display_name, created_at) VALUES (1, ?, ?, ?)`, email, name, s.now()); err != nil {
		return nil, "", err
	}
	tok, err := s.NewSetupLink(ctx, 1)
	if err != nil {
		return nil, "", err
	}
	u, err := s.UserByID(ctx, 1)
	return u, tok, err
}

// NewSetupLink revokes earlier links and returns a new one-time token. Using
// it lets the holder set a new password and register a passkey, so only the
// command line (server access) can make one.
func (s *Store) NewSetupLink(ctx context.Context, userID int64) (string, error) {
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE setup_links SET used_at = ? WHERE user_id = ? AND used_at IS NULL`, now, userID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO setup_links(token_hash, user_id, expires_at, created_at) VALUES (?,?,?,?)`,
		auth.HashToken(tok), userID, now+int64(SetupTTL/time.Second), now); err != nil {
		return "", err
	}
	return tok, tx.Commit()
}

// SetupLinkUser checks a setup token without using it.
func (s *Store) SetupLinkUser(ctx context.Context, tok string) (*User, error) {
	var uid int64
	err := s.DB.QueryRowContext(ctx, `SELECT user_id FROM setup_links WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, auth.HashToken(tok), s.now()).Scan(&uid)
	if err != nil {
		return nil, ErrLinkInvalid
	}
	return s.UserByID(ctx, uid)
}

// UseSetupLink sets the password, marks the link used, signs the owner out
// everywhere and returns an enroll-stage session (passkey registration
// owed). Errors from ValidatePassword are InputErrors.
func (s *Store) UseSetupLink(ctx context.Context, tok, password, ua string) (string, error) {
	if err := auth.ValidatePassword(password); err != nil {
		return "", badInput(err.Error())
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id, uid int64
	if err := tx.QueryRowContext(ctx, `SELECT id, user_id FROM setup_links WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, auth.HashToken(tok), now).Scan(&id, &uid); err != nil {
		return "", ErrLinkInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE setup_links SET used_at = ? WHERE id = ?`, now, id); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, uid); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, uid); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return s.CreateSession(ctx, uid, StageEnroll, ua)
}

// ---- Settings ----

// Setting keys.
const (
	SettingTZ       = "tz"
	SettingCapCents = "cap_cents"
)

// Setting returns a setting or def.
func (s *Store) Setting(ctx context.Context, key, def string) string {
	var v string
	if err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Zone returns the display zone: the owner's setting, else def.
func (s *Store) Zone(ctx context.Context, def *time.Location) *time.Location {
	if name := s.Setting(ctx, SettingTZ, ""); name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return def
}

// Spend cap bounds, in cents. The cap has no default: until the owner
// sets one, the scheduler refuses to post.
const (
	MinCapCents = 1         // $0.01
	MaxCapCents = 1_000_000 // $10,000
)

// CapCents is the owner's monthly spend cap in cents. ok is false when no
// valid cap is stored, which keeps posting off (fail closed).
func (s *Store) CapCents(ctx context.Context) (cents int64, ok bool) {
	v, err := strconv.ParseInt(s.Setting(ctx, SettingCapCents, ""), 10, 64)
	if err != nil || v < MinCapCents || v > MaxCapCents {
		return 0, false
	}
	return v, true
}

// SetCapCents stores the cap after checking its bounds.
func (s *Store) SetCapCents(ctx context.Context, u *User, cents int64) error {
	if u == nil {
		return ErrNotFound
	}
	if cents < MinCapCents || cents > MaxCapCents {
		return badInput("The cap must be between $0.01 and $10,000.00.")
	}
	return s.SetSetting(ctx, SettingCapCents, strconv.FormatInt(cents, 10))
}

// ---- Accounts ----

// Account is an X account drafts may target.
type Account struct {
	Handle    string
	Initials  string
	Color     string // CSS class
	Paused    bool
	XStatus   string // "" (not connected), XConnected or XReconnect
	XUsername string // the X account the tokens belong to
}

// Accounts lists the configured X accounts in display order (blog
// destinations share the table but are listed by Blogs).
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.handle, a.initials, a.color, a.paused, COALESCE(x.status, ''), COALESCE(x.x_username, '')
		FROM accounts a LEFT JOIN x_connections x ON x.account = a.handle WHERE a.kind = 'x' ORDER BY a.position`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var p int
		if err := rows.Scan(&a.Handle, &a.Initials, &a.Color, &p, &a.XStatus, &a.XUsername); err != nil {
			return nil, err
		}
		a.Paused = p == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// ErrBadHandle and ErrAccountExists are AddAccount's refusals; their text
// is shown as is.
var (
	ErrBadHandle     = errors.New("An X handle is 1 to 15 letters, digits or underscores, without the @.")
	ErrAccountExists = errors.New("That X account is already in xpost.")
)

var handleRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

// accountColors are the avatar classes new accounts take in turn.
var accountColors = []string{"sm", "jg"}

// AddAccount adds an X account drafts may target, last in display order.
// A fresh install has none: the owner adds them in Settings > X accounts,
// and only seed-demo adds the sample ones. handle may carry a leading @.
func (s *Store) AddAccount(ctx context.Context, handle string) (Account, error) {
	h := strings.TrimPrefix(strings.TrimSpace(handle), "@")
	if !handleRE.MatchString(h) {
		return Account{}, ErrBadHandle
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, err
	}
	defer tx.Rollback()
	var n, pos, xs int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE lower(handle) = lower(?)`, h).Scan(&n)
	if n > 0 {
		return Account{}, ErrAccountExists
	}
	tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0), (SELECT COUNT(*) FROM accounts WHERE kind = 'x') FROM accounts`).Scan(&pos, &xs)
	a := Account{Handle: h, Initials: handleInitials(h), Color: accountColors[xs%len(accountColors)]}
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts(handle, initials, color, position) VALUES (?, ?, ?, ?)`, a.Handle, a.Initials, a.Color, pos+1); err != nil {
		return Account{}, err
	}
	return a, tx.Commit()
}

// handleInitials is the avatar text: the first letters of the first two
// underscore-separated words (sample_writer: SW), else the first two
// characters (example: EX).
func handleInitials(h string) string {
	var ws []string
	for _, w := range strings.Split(h, "_") {
		if w != "" {
			ws = append(ws, w)
		}
	}
	if len(ws) >= 2 {
		return strings.ToUpper(ws[0][:1] + ws[1][:1])
	}
	if len(ws) == 0 {
		return strings.ToUpper(h[:1])
	}
	return strings.ToUpper(ws[0][:min(2, len(ws[0]))])
}

// accountExists reports whether handle is an X account.
func (s *Store) accountExists(ctx context.Context, handle string) bool {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE handle = ? AND kind = 'x'`, handle).Scan(&n)
	return n == 1
}
