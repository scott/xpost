package xauth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/scott/xpost/internal/store"
)

// RefreshSkew: refresh when the access token has less than this left.
const RefreshSkew = 5 * time.Minute

// ErrReconnect means the account must be connected again by the owner.
var ErrReconnect = errors.New("this X account needs to be reconnected")

// ErrNotConnected means the account has no X connection.
var ErrNotConnected = errors.New("this account is not connected to X")

// Manager hands out fresh access tokens for connected accounts. It
// refreshes before expiry and on a 401, stores rotated refresh tokens, and
// serialises refreshes per account so a single-use refresh token is never
// spent twice. Cross-process races are caught by the row's generation.
type Manager struct {
	Store  *store.Store
	Client *Client
	Log    *log.Logger // optional; never given a token

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (m *Manager) lock(account string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = map[string]*sync.Mutex{}
	}
	l := m.locks[account]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[account] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Manager) logf(format string, args ...any) {
	if m.Log != nil {
		m.Log.Printf(format, args...)
	}
}

// Access returns a usable access token, refreshing first if it expires
// within RefreshSkew.
func (m *Manager) Access(ctx context.Context, account string) (string, error) {
	tok, _, err := m.fresh(ctx, account)
	return tok, err
}

func (m *Manager) usable(t *store.XTokens) bool {
	return m.Store.Now().Add(RefreshSkew).Before(t.Expiry)
}

// fresh returns a usable token and the generation it came from.
func (m *Manager) fresh(ctx context.Context, account string) (string, int64, error) {
	t, c, err := m.load(ctx, account)
	if err != nil {
		return "", 0, err
	}
	if m.usable(t) {
		return t.Access, c.Generation, nil
	}
	return m.refresh(ctx, account, c.Generation, false)
}

func (m *Manager) load(ctx context.Context, account string) (*store.XTokens, *store.XConn, error) {
	t, c, err := m.Store.XTokensFor(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNotConnected
	}
	if err != nil {
		return nil, nil, err
	}
	if c.Status == store.XReconnect {
		return nil, nil, ErrReconnect
	}
	return t, c, nil
}

// refresh replaces the token the caller saw at generation seen. If the row
// moved on while we waited for the lock, the newer token is used instead
// (unless it is itself about to expire). force refreshes even a token that
// looks unexpired (after a 401).
func (m *Manager) refresh(ctx context.Context, account string, seen int64, force bool) (string, int64, error) {
	unlock := m.lock(account)
	defer unlock()
	t, c, err := m.load(ctx, account)
	if err != nil {
		return "", 0, err
	}
	if c.Generation != seen && m.usable(t) {
		return t.Access, c.Generation, nil // someone else refreshed while we waited
	}
	if !force && m.usable(t) {
		return t.Access, c.Generation, nil
	}
	if t.Refresh == "" {
		m.Store.MarkXReconnect(ctx, account, c.Generation, "no refresh token (offline.access missing)")
		return "", 0, ErrReconnect
	}
	nt, err := m.Client.Refresh(ctx, t.Refresh, m.Store.Now())
	if err != nil {
		if IsInvalidGrant(err) {
			// Another process may have rotated it a moment ago.
			if t2, c2, err2 := m.load(ctx, account); err2 == nil && c2.Generation != c.Generation {
				return t2.Access, c2.Generation, nil
			}
			m.Store.MarkXReconnect(ctx, account, c.Generation, "refresh refused (invalid_grant)")
			m.logf("x account @%s: refresh refused; marked for reconnect", account)
			return "", 0, ErrReconnect
		}
		return "", 0, err
	}
	ok, err := m.Store.UpdateXTokens(ctx, account, c.Generation, store.XTokens{Access: nt.Access, Refresh: nt.Refresh, Expiry: nt.Expiry, Scope: nt.Scope})
	if err != nil {
		return "", 0, err
	}
	if !ok {
		// Lost a cross-process race after spending the refresh token: the
		// winner's token is stored; use it.
		t2, c2, err := m.load(ctx, account)
		if err != nil {
			return "", 0, err
		}
		return t2.Access, c2.Generation, nil
	}
	m.logf("x account @%s: access token refreshed", account)
	return nt.Access, c.Generation + 1, nil
}

// Do runs call with a fresh access token. If call reports a 401 (an error
// for which IsUnauthorized is true), the token is refreshed and call is
// retried once.
func (m *Manager) Do(ctx context.Context, account string, call func(access string) error) error {
	access, gen, err := m.fresh(ctx, account)
	if err != nil {
		return err
	}
	err = call(access)
	if !IsUnauthorized(err) {
		return err
	}
	access, _, err = m.refresh(ctx, account, gen, true)
	if err != nil {
		return err
	}
	return call(access)
}

// Check calls users/me through Do (refreshing as needed) and returns the
// X user the stored token belongs to.
func (m *Manager) Check(ctx context.Context, account string) (*User, error) {
	var u *User
	err := m.Do(ctx, account, func(access string) error {
		var err error
		u, err = m.Client.Me(ctx, access)
		return err
	})
	return u, err
}

// Disconnect revokes the account's tokens at the provider (best effort)
// and deletes them locally. revoked is false when X did not confirm the
// revoke; err is set only when the local delete failed.
func (m *Manager) Disconnect(ctx context.Context, account string) (revoked bool, err error) {
	unlock := m.lock(account)
	defer unlock()
	t, _, lerr := m.Store.XTokensFor(ctx, account)
	if errors.Is(lerr, store.ErrNotFound) {
		return true, nil
	}
	revoked = false
	if lerr == nil && m.Client.Cfg.Enabled() {
		// Revoking the refresh token ends the grant; revoke the access token too.
		e1 := m.Client.Revoke(ctx, t.Refresh, "refresh_token")
		e2 := m.Client.Revoke(ctx, t.Access, "access_token")
		revoked = e1 == nil && e2 == nil
		if !revoked {
			m.logf("x account @%s: revoke at X failed: %v", account, errors.Join(e1, e2))
		}
	}
	if err := m.Store.DeleteXConnection(ctx, account); err != nil {
		return revoked, fmt.Errorf("delete local tokens: %w", err)
	}
	m.logf("x account @%s: disconnected, local tokens deleted", account)
	return revoked, nil
}
