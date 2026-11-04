package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/scott/xpost/internal/auth"
)

// OAuthStateTTL is how long a Connect click stays valid.
const OAuthStateTTL = 10 * time.Minute

// X connection statuses.
const (
	XConnected = "connected"
	XReconnect = "reconnect" // refresh failed for good; the owner must connect again
)

// ErrOAuthState covers every bad callback state: unknown, used, expired,
// another session's, or another account's. One error so nothing leaks.
var ErrOAuthState = errors.New("this connect link is invalid or expired; press Connect again")

// SaveOAuthState records a pending authorization for account, bound to the
// owner's session. state and verifier are secrets: state is stored hashed
// and the verifier sealed.
func (s *Store) SaveOAuthState(ctx context.Context, state, sessionTok, account, verifier string) error {
	if state == "" || sessionTok == "" || verifier == "" {
		return ErrOAuthState
	}
	if !s.accountExists(ctx, account) {
		return ErrNotFound
	}
	sealed, err := s.Key.Seal([]byte(verifier), []byte("oauth_states.verifier:"+auth.HashToken(state)))
	if err != nil {
		return err
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Expired rows go; so does any earlier pending click for this account.
	if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at <= ? OR account = ?`, now, account); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_states(state_hash, session_hash, account, verifier_enc, created_at, expires_at) VALUES (?,?,?,?,?,?)`,
		auth.HashToken(state), auth.HashToken(sessionTok), account, sealed, now, now+int64(OAuthStateTTL/time.Second)); err != nil {
		return err
	}
	return tx.Commit()
}

// TakeOAuthState consumes a state (single use) and returns its account and
// verifier. It fails unless the state is live and belongs to sessionTok.
func (s *Store) TakeOAuthState(ctx context.Context, state, sessionTok string) (account, verifier string, err error) {
	if state == "" || sessionTok == "" {
		return "", "", ErrOAuthState
	}
	h := auth.HashToken(state)
	var sess string
	var sealed []byte
	var exp int64
	err = s.DB.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash = ? RETURNING session_hash, account, verifier_enc, expires_at`, h).
		Scan(&sess, &account, &sealed, &exp)
	if err != nil || exp <= s.now() || sess != auth.HashToken(sessionTok) {
		return "", "", ErrOAuthState
	}
	v, err := s.Key.Open(sealed, []byte("oauth_states.verifier:"+h))
	if err != nil {
		return "", "", ErrOAuthState
	}
	return account, string(v), nil
}

// XTokens is a decrypted token pair. Never log or render it.
type XTokens struct {
	Access  string
	Refresh string
	Expiry  time.Time
	Scope   string
}

// XConn is an account's X connection, without tokens.
type XConn struct {
	Account     string
	XUserID     string
	XUsername   string
	Status      string
	LastError   string
	Scope       string
	Expiry      time.Time
	ConnectedAt time.Time
	RefreshedAt *time.Time
	Generation  int64
}

func tokAAD(account, kind string) []byte { return []byte("x_connections." + kind + ":" + account) }

func (s *Store) seal(account, kind, v string) ([]byte, error) {
	if v == "" {
		return nil, nil
	}
	return s.Key.Seal([]byte(v), tokAAD(account, kind))
}

// SaveXConnection stores (or replaces) an account's connection.
func (s *Store) SaveXConnection(ctx context.Context, account, xUserID, xUsername string, t XTokens) error {
	if !s.accountExists(ctx, account) {
		return ErrNotFound
	}
	if t.Access == "" {
		return badInput("no access token")
	}
	a, err := s.seal(account, "access", t.Access)
	if err != nil {
		return err
	}
	r, err := s.seal(account, "refresh", t.Refresh)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO x_connections(account, x_user_id, x_username, access_enc, refresh_enc, expires_at, scope, status, last_error, connected_at, generation)
		VALUES (?,?,?,?,?,?,?,?, '', ?, 1)
		ON CONFLICT(account) DO UPDATE SET x_user_id=excluded.x_user_id, x_username=excluded.x_username, access_enc=excluded.access_enc,
			refresh_enc=excluded.refresh_enc, expires_at=excluded.expires_at, scope=excluded.scope, status=excluded.status, last_error='',
			connected_at=excluded.connected_at, refreshed_at=NULL, generation=x_connections.generation+1`,
		account, xUserID, xUsername, a, r, t.Expiry.Unix(), t.Scope, XConnected, now)
	return err
}

const xconnCols = `account, x_user_id, x_username, status, last_error, scope, expires_at, connected_at, refreshed_at, generation`

func scanXConn(r scanner) (*XConn, error) {
	var c XConn
	var exp, conn int64
	var ref sql.NullInt64
	if err := r.Scan(&c.Account, &c.XUserID, &c.XUsername, &c.Status, &c.LastError, &c.Scope, &exp, &conn, &ref, &c.Generation); err != nil {
		return nil, err
	}
	c.Expiry, c.ConnectedAt, c.RefreshedAt = time.Unix(exp, 0), time.Unix(conn, 0), tptr(ref)
	return &c, nil
}

// XConnection returns an account's connection (no tokens), or ErrNotFound.
func (s *Store) XConnection(ctx context.Context, account string) (*XConn, error) {
	c, err := scanXConn(s.DB.QueryRowContext(ctx, `SELECT `+xconnCols+` FROM x_connections WHERE account = ?`, account))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// XConnections returns every connection keyed by account.
func (s *Store) XConnections(ctx context.Context) (map[string]*XConn, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+xconnCols+` FROM x_connections`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*XConn{}
	for rows.Next() {
		c, err := scanXConn(rows)
		if err != nil {
			return nil, err
		}
		out[c.Account] = c
	}
	return out, rows.Err()
}

// XTokensFor decrypts an account's tokens and returns them with the row's
// generation.
func (s *Store) XTokensFor(ctx context.Context, account string) (*XTokens, *XConn, error) {
	var a, r []byte
	row := s.DB.QueryRowContext(ctx, `SELECT access_enc, refresh_enc, `+xconnCols+` FROM x_connections WHERE account = ?`, account)
	var c XConn
	var exp, conn int64
	var ref sql.NullInt64
	if err := row.Scan(&a, &r, &c.Account, &c.XUserID, &c.XUsername, &c.Status, &c.LastError, &c.Scope, &exp, &conn, &ref, &c.Generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	c.Expiry, c.ConnectedAt, c.RefreshedAt = time.Unix(exp, 0), time.Unix(conn, 0), tptr(ref)
	t := &XTokens{Expiry: c.Expiry, Scope: c.Scope}
	pa, err := s.Key.Open(a, tokAAD(account, "access"))
	if err != nil {
		return nil, nil, fmt.Errorf("x tokens for %s: cannot decrypt (wrong key?)", account)
	}
	t.Access = string(pa)
	if len(r) > 0 {
		pr, err := s.Key.Open(r, tokAAD(account, "refresh"))
		if err != nil {
			return nil, nil, fmt.Errorf("x tokens for %s: cannot decrypt (wrong key?)", account)
		}
		t.Refresh = string(pr)
	}
	return t, &c, nil
}

// UpdateXTokens replaces the tokens after a refresh, only if the row is
// still at generation gen. It reports false when another refresh won.
// An empty t.Refresh keeps the stored refresh token (no rotation).
func (s *Store) UpdateXTokens(ctx context.Context, account string, gen int64, t XTokens) (bool, error) {
	a, err := s.seal(account, "access", t.Access)
	if err != nil {
		return false, err
	}
	q := `UPDATE x_connections SET access_enc = ?, expires_at = ?, scope = CASE WHEN ? = '' THEN scope ELSE ? END, status = ?, last_error = '', refreshed_at = ?, generation = generation + 1`
	args := []any{a, t.Expiry.Unix(), t.Scope, t.Scope, XConnected, s.now()}
	if t.Refresh != "" {
		r, err := s.seal(account, "refresh", t.Refresh)
		if err != nil {
			return false, err
		}
		q += `, refresh_enc = ?`
		args = append(args, r)
	}
	q += ` WHERE account = ? AND generation = ?`
	args = append(args, account, gen)
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkXReconnect flags the connection as needing the owner to connect
// again, only if it is still at generation gen. reason must not contain
// secrets.
func (s *Store) MarkXReconnect(ctx context.Context, account string, gen int64, reason string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE x_connections SET status = ?, last_error = ?, generation = generation + 1 WHERE account = ? AND generation = ?`,
		XReconnect, reason, account, gen)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteXConnection removes the connection and its tokens.
func (s *Store) DeleteXConnection(ctx context.Context, account string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM x_connections WHERE account = ?`, account)
	return err
}
