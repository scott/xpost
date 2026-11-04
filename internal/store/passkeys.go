package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/scott/xpost/internal/auth"
)

// Passkey ceremony purposes.
const (
	PurposeLogin    = "login"
	PurposeRegister = "register"
)

// ErrChallenge means the ceremony is unknown, used, expired or bound to a
// different session.
var ErrChallenge = errors.New("this passkey request expired or was already used; try again")

// ErrLastPasskey refuses removing the only passkey: sign-in needs one.
var ErrLastPasskey = errors.New("this is your only passkey")

// Passkey is one registered WebAuthn credential. Nothing here is secret.
type Passkey struct {
	ID                int64
	CredID            []byte
	PublicKey         []byte
	SignCount         uint32
	Transports        []string
	AAGUID            []byte
	AttestationType   string
	AttestationFormat string
	BackupEligible    bool
	BackupState       bool
	Name              string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
}

// WebAuthnHandle returns the owner's random WebAuthn user handle, creating
// it on first use. It is never the row id or the email.
func (s *Store) WebAuthnHandle(ctx context.Context, userID int64) ([]byte, error) {
	var h []byte
	if err := s.DB.QueryRowContext(ctx, `SELECT webauthn_handle FROM users WHERE id = ?`, userID).Scan(&h); err != nil {
		return nil, err
	}
	if len(h) > 0 {
		return h, nil
	}
	h = make([]byte, 32)
	if _, err := rand.Read(h); err != nil {
		return nil, err
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE users SET webauthn_handle = ? WHERE id = ? AND webauthn_handle IS NULL`, h, userID); err != nil {
		return nil, err
	}
	return s.WebAuthnHandle(ctx, userID)
}

// PasskeysFor lists the owner's passkeys, oldest first.
func (s *Store) PasskeysFor(ctx context.Context, userID int64) ([]*Passkey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, cred_id, public_key, sign_count, transports, aaguid, attestation_type, attestation_fmt, backup_eligible, backup_state, name, created_at, last_used_at
		FROM webauthn_credentials WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Passkey
	for rows.Next() {
		var p Passkey
		var tr string
		var be, bs int
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&p.ID, &p.CredID, &p.PublicKey, &p.SignCount, &tr, &p.AAGUID, &p.AttestationType, &p.AttestationFormat, &be, &bs, &p.Name, &created, &used); err != nil {
			return nil, err
		}
		if tr != "" {
			p.Transports = strings.Split(tr, ",")
		}
		p.BackupEligible, p.BackupState = be == 1, bs == 1
		p.CreatedAt, p.LastUsedAt = time.Unix(created, 0), tptr(used)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// PasskeyCount is how many passkeys the owner has.
func (s *Store) PasskeyCount(ctx context.Context, userID int64) int {
	var n int
	s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?`, userID).Scan(&n)
	return n
}

// CleanName trims a label to 1..60 printable characters.
func CleanName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if r := []rune(name); len(r) > 60 {
		name = strings.TrimSpace(string(r[:60]))
	}
	return name
}

// AddPasskey stores a newly registered credential.
func (s *Store) AddPasskey(ctx context.Context, userID int64, p *Passkey) (int64, error) {
	p.Name = CleanName(p.Name)
	if p.Name == "" {
		p.Name = "Passkey"
	}
	if s.PasskeyCount(ctx, userID) >= MaxPasskeys {
		return 0, badInput("you already have the maximum number of passkeys")
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO webauthn_credentials(user_id, cred_id, public_key, sign_count, transports, aaguid, attestation_type, attestation_fmt, backup_eligible, backup_state, name, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, userID, p.CredID, p.PublicKey, p.SignCount, strings.Join(p.Transports, ","), p.AAGUID, p.AttestationType, p.AttestationFormat,
		b(p.BackupEligible), b(p.BackupState), p.Name, s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, badInput("that passkey is already registered")
		}
		return 0, err
	}
	return res.LastInsertId()
}

// RecordPasskeyUse saves the new sign counter after a sign-in.
func (s *Store) RecordPasskeyUse(ctx context.Context, id int64, count uint32, backupState bool) error {
	bs := 0
	if backupState {
		bs = 1
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE webauthn_credentials SET sign_count = ?, backup_state = ?, last_used_at = ? WHERE id = ?`, count, bs, s.now(), id)
	return err
}

// RemovePasskey deletes one of the owner's passkeys, never the last.
func (s *Store) RemovePasskey(ctx context.Context, userID, id int64) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ?`, userID).Scan(&n)
	var found int
	tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ? AND id = ?`, userID, id).Scan(&found)
	if found == 0 {
		return ErrNotFound
	}
	if n <= 1 {
		return ErrLastPasskey
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Challenge is a stored, in-flight ceremony.
type Challenge struct {
	Purpose     string
	SessionHash string
	Data        string
}

// SaveChallenge stores ceremony data under tok, bound to the session.
func (s *Store) SaveChallenge(ctx context.Context, tok, purpose, sessionTok, data string) error {
	now := s.now()
	s.DB.ExecContext(ctx, `DELETE FROM webauthn_challenges WHERE expires_at <= ?`, now)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO webauthn_challenges(id_hash, purpose, session_hash, data_json, expires_at) VALUES (?,?,?,?,?)`,
		auth.HashToken(tok), purpose, auth.HashToken(sessionTok), data, now+int64(ChallengeTTL/time.Second))
	return err
}

// TakeChallenge consumes a ceremony (single use). It must match the purpose
// and the session that started it.
func (s *Store) TakeChallenge(ctx context.Context, tok, purpose, sessionTok string) (*Challenge, error) {
	if tok == "" || sessionTok == "" {
		return nil, ErrChallenge
	}
	var c Challenge
	var exp int64
	err := s.DB.QueryRowContext(ctx, `DELETE FROM webauthn_challenges WHERE id_hash = ? RETURNING purpose, session_hash, data_json, expires_at`, auth.HashToken(tok)).
		Scan(&c.Purpose, &c.SessionHash, &c.Data, &exp)
	if err != nil || c.Purpose != purpose || exp <= s.now() || c.SessionHash != auth.HashToken(sessionTok) {
		return nil, ErrChallenge
	}
	return &c, nil
}
