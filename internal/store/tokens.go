package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/scott/xpost/internal/auth"
)

// Agent token scopes. There is deliberately no scope for approving, editing
// or posting: agents can never do those. drafts:revise only answers an open
// rewrite request the owner sent for one of the token's own drafts.
const (
	ScopeDraftsWrite  = "drafts:write"
	ScopeDraftsRevise = "drafts:revise"
	ScopeLogRead      = "log:read"
	// ScopeBlogsPublish pulls and reports the publishes of the one blog
	// whose Publishing token this is (Settings > Blogs). It can't create,
	// read or change drafts.
	ScopeBlogsPublish = "blogs:publish"
)

// TokenPrefix starts every agent token so a leaked one is recognisable.
const TokenPrefix = "xpt_"

// AgentToken is an agent's API credential. The secret itself is never stored.
type AgentToken struct {
	ID         int64
	Name       string
	Last4      string
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Has reports whether the token carries scope.
func (t *AgentToken) Has(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Masked is the display form, e.g. xpt_••••a1b2.
func (t *AgentToken) Masked() string { return TokenPrefix + "••••" + t.Last4 }

func cleanScopes(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		switch s {
		case "":
			continue
		case ScopeDraftsWrite, ScopeDraftsRevise, ScopeLogRead, ScopeBlogsPublish:
		default:
			return nil, badInput("unknown scope " + s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, badInput("pick at least one scope")
	}
	sort.Strings(out)
	return out, nil
}

// CreateAgentToken makes a token for owner u and returns the secret once.
func (s *Store) CreateAgentToken(ctx context.Context, u *User, name string, scopes []string) (*AgentToken, string, error) {
	if u == nil {
		return nil, "", ErrNotFound
	}
	name = CleanName(name)
	if name == "" {
		return nil, "", badInput("give the token a name, e.g. the agent it is for")
	}
	sc, err := cleanScopes(scopes)
	if err != nil {
		return nil, "", err
	}
	raw, err := auth.NewToken()
	if err != nil {
		return nil, "", err
	}
	secret := TokenPrefix + raw
	res, err := s.DB.ExecContext(ctx, `INSERT INTO agent_tokens(name, token_hash, last4, scopes, created_at) VALUES (?,?,?,?,?)`,
		name, auth.HashToken(secret), secret[len(secret)-4:], strings.Join(sc, " "), s.now())
	if err != nil {
		return nil, "", err
	}
	id, _ := res.LastInsertId()
	t, err := s.agentToken(ctx, `id = ?`, id)
	return t, secret, err
}

func (s *Store) agentToken(ctx context.Context, where string, arg any) (*AgentToken, error) {
	var t AgentToken
	var sc string
	var created int64
	var used, revoked sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT id, name, last4, scopes, created_at, last_used_at, revoked_at FROM agent_tokens WHERE `+where, arg).
		Scan(&t.ID, &t.Name, &t.Last4, &sc, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Scopes = strings.Fields(sc)
	t.CreatedAt, t.LastUsedAt, t.RevokedAt = time.Unix(created, 0), tptr(used), tptr(revoked)
	return &t, nil
}

// AgentTokenByID loads a token (live or revoked) by id.
func (s *Store) AgentTokenByID(ctx context.Context, id int64) (*AgentToken, error) {
	return s.agentToken(ctx, `id = ?`, id)
}

// AuthenticateAgent resolves a bearer secret to a live token.
func (s *Store) AuthenticateAgent(ctx context.Context, secret string) (*AgentToken, error) {
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) < len(TokenPrefix)+40 {
		return nil, ErrNotFound
	}
	t, err := s.agentToken(ctx, `token_hash = ?`, auth.HashToken(secret))
	if err != nil || t.RevokedAt != nil {
		return nil, ErrNotFound
	}
	now := s.Now()
	if t.LastUsedAt == nil || now.Sub(*t.LastUsedAt) > time.Minute {
		s.DB.ExecContext(ctx, `UPDATE agent_tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), t.ID)
	}
	return t, nil
}

// AgentTokens lists live tokens, newest first.
func (s *Store) AgentTokens(ctx context.Context) ([]*AgentToken, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM agent_tokens WHERE revoked_at IS NULL ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []*AgentToken
	for _, id := range ids {
		t, err := s.agentToken(ctx, `id = ?`, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// RevokeAgentToken revokes a token. Its drafts stay in the queue.
func (s *Store) RevokeAgentToken(ctx context.Context, u *User, id int64) error {
	if u == nil {
		return ErrNotFound
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE agent_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
