package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/scott/xpost/internal/auth"
)

// Session stages. Only StageFull may use the app; the others reach just the
// step they are for.
const (
	StagePassword = "password" // password ok, passkey assertion owed
	StageEnroll   = "enroll"   // setup link used, passkey registration owed
	StageFull     = "full"     // password + passkey
)

// Session is a signed-in browser.
type Session struct {
	IDHash     string
	UserID     int64
	Stage      string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// CreateSession stores a new session and returns its cookie token.
func (s *Store) CreateSession(ctx context.Context, userID int64, stage, ua string) (string, error) {
	tok, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	ttl := SessionTTL
	if stage != StageFull {
		ttl = StepSessionTTL
	}
	if len(ua) > 200 {
		ua = ua[:200]
	}
	now := s.now()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sessions(id_hash, user_id, stage, created_at, last_seen_at, expires_at, ua) VALUES (?,?,?,?,?,?,?)`,
		auth.HashToken(tok), userID, stage, now, now, now+int64(ttl/time.Second), ua)
	return tok, err
}

// LookupSession returns a live session and its user. Expired or idle
// sessions are deleted and reported as ErrNotFound.
func (s *Store) LookupSession(ctx context.Context, token string) (*Session, *User, error) {
	return s.lookupSession(ctx, token, true)
}

// PeekSession is LookupSession without counting as activity: it never moves
// last_seen_at, so a tab left open with live updates still reaches the idle
// timeout.
func (s *Store) PeekSession(ctx context.Context, token string) (*Session, *User, error) {
	return s.lookupSession(ctx, token, false)
}

func (s *Store) lookupSession(ctx context.Context, token string, touch bool) (*Session, *User, error) {
	if token == "" {
		return nil, nil, ErrNotFound
	}
	h := auth.HashToken(token)
	var ss Session
	var created, seen, exp int64
	err := s.DB.QueryRowContext(ctx, `SELECT id_hash, user_id, stage, created_at, last_seen_at, expires_at FROM sessions WHERE id_hash = ?`, h).
		Scan(&ss.IDHash, &ss.UserID, &ss.Stage, &created, &seen, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	ss.CreatedAt, ss.LastSeenAt, ss.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(exp, 0)
	now := s.Now()
	if !now.Before(ss.ExpiresAt) || now.Sub(ss.LastSeenAt) > SessionIdleTTL {
		s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, h)
		return nil, nil, ErrNotFound
	}
	u, err := s.UserByID(ctx, ss.UserID)
	if err != nil {
		return nil, nil, err
	}
	if touch && now.Sub(ss.LastSeenAt) > time.Minute {
		s.DB.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, now.Unix(), h)
	}
	return &ss, u, nil
}

// PromoteSession replaces a step session with a new full one (new token: no
// session fixation).
func (s *Store) PromoteSession(ctx context.Context, oldToken string, userID int64, ua string) (string, error) {
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, auth.HashToken(oldToken)); err != nil {
		return "", err
	}
	return s.CreateSession(ctx, userID, StageFull, ua)
}

// DeleteSession signs one browser out.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, auth.HashToken(token))
	return err
}

// Fresh reports whether a full session signed in within FreshSignInTTL.
func (ss *Session) Fresh(now time.Time) bool {
	return ss.Stage == StageFull && now.Sub(ss.CreatedAt) <= FreshSignInTTL
}
