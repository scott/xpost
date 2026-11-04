package xauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NewVerifier returns a PKCE code verifier (43 base64url characters).
func NewVerifier() (string, error) { return randString(32) }

// NewState returns a random state value.
func NewState() (string, error) { return randString(32) }

func randString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge is the S256 code challenge for verifier.
func Challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Client talks to the OAuth endpoints in Config.
type Client struct {
	Cfg  *Config
	HTTP *http.Client
	Now  func() time.Time // optional clock (rate-limit Retry-After)
}

// NewClient returns a client with a bounded timeout.
func NewClient(cfg *Config) *Client {
	t := cfg.Timeout
	if t == 0 {
		t = 20 * time.Second
	}
	return &Client{Cfg: cfg, HTTP: &http.Client{Timeout: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// AuthURL is where the owner's browser goes to approve the connection.
func (c *Client) AuthURL(state, verifier string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {c.Cfg.ClientID},
		"redirect_uri":          {c.Cfg.RedirectURL},
		"scope":                 {strings.Join(Scopes, " ")},
		"state":                 {state},
		"code_challenge":        {Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(c.Cfg.AuthorizeURL, "?") {
		sep = "&"
	}
	return c.Cfg.AuthorizeURL + sep + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// Token is a token response. Never log or render it.
type Token struct {
	Access  string
	Refresh string
	Expiry  time.Time
	Scope   string
}

// Error is an OAuth or HTTP error from the provider. It carries only the
// status and the OAuth error code: never a token, code or secret, and not
// the provider's free-text description.
type Error struct {
	Op     string // "token", "refresh", "revoke", "users/me"
	Status int
	Code   string // OAuth error code, e.g. invalid_grant
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("x oauth %s: %s (HTTP %d)", e.Op, e.Code, e.Status)
	}
	return fmt.Sprintf("x oauth %s: HTTP %d", e.Op, e.Status)
}

// IsInvalidGrant reports whether err means the refresh token (or code) is
// no longer valid, so the owner has to connect again.
func IsInvalidGrant(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "invalid_grant"
}

// IsUnauthorized reports a 401 from a resource call.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// netError hides transport details that might include request data; the
// URL of our own endpoints carries no secret, but keep it short anyway.
func netError(op string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("x oauth %s: %s failed: %v", op, ue.Op, ue.Err)
	}
	return fmt.Errorf("x oauth %s: %v", op, err)
}

func (c *Client) postForm(ctx context.Context, op, endpoint string, form url.Values) ([]byte, error) {
	if !c.Cfg.Enabled() {
		return nil, ErrNotConfigured
	}
	if c.Cfg.ClientSecret == "" {
		form.Set("client_id", c.Cfg.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, netError(op, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.Cfg.ClientSecret != "" {
		// Confidential client: HTTP Basic with the url-encoded id and secret.
		req.SetBasicAuth(url.QueryEscape(c.Cfg.ClientID), url.QueryEscape(c.Cfg.ClientSecret))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, netError(op, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var oe struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &oe)
		return nil, &Error{Op: op, Status: resp.StatusCode, Code: cleanCode(oe.Error)}
	}
	return body, nil
}

// cleanCode keeps only a plausible OAuth error code.
func cleanCode(s string) string {
	if len(s) > 40 {
		return ""
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z') {
			return ""
		}
	}
	return s
}

func parseToken(op string, body []byte, now time.Time) (*Token, error) {
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return nil, &Error{Op: op, Status: http.StatusOK, Code: "bad_token_response"}
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return nil, &Error{Op: op, Status: http.StatusOK, Code: "unsupported_token_type"}
	}
	exp := tr.ExpiresIn
	if exp <= 0 {
		exp = 3600 // conservative when the provider omits it
	}
	return &Token{Access: tr.AccessToken, Refresh: tr.RefreshToken, Expiry: now.Add(time.Duration(exp) * time.Second), Scope: tr.Scope}, nil
}

// Exchange trades an authorization code and its PKCE verifier for tokens.
func (c *Client) Exchange(ctx context.Context, code, verifier string, now time.Time) (*Token, error) {
	body, err := c.postForm(ctx, "token", c.Cfg.TokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.Cfg.RedirectURL},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	return parseToken("token", body, now)
}

// Refresh trades a refresh token for new tokens. X rotates refresh tokens:
// the caller must store the new one.
func (c *Client) Refresh(ctx context.Context, refresh string, now time.Time) (*Token, error) {
	body, err := c.postForm(ctx, "refresh", c.Cfg.TokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	})
	if err != nil {
		return nil, err
	}
	return parseToken("refresh", body, now)
}

// Revoke revokes a token at the provider.
func (c *Client) Revoke(ctx context.Context, token, hint string) error {
	if token == "" {
		return nil
	}
	_, err := c.postForm(ctx, "revoke", c.Cfg.RevokeURL, url.Values{"token": {token}, "token_type_hint": {hint}})
	return err
}

// User is the X account a token belongs to.
type User struct {
	ID       string
	Username string
	Name     string
}

// Me returns the token's X user (GET users/me).
func (c *Client) Me(ctx context.Context, access string) (*User, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.Cfg.MeURL, nil)
	if err != nil {
		return nil, netError("users/me", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, netError("users/me", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &Error{Op: "users/me", Status: resp.StatusCode}
	}
	var out struct {
		Data struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Data.ID == "" || out.Data.Username == "" {
		return nil, &Error{Op: "users/me", Status: resp.StatusCode, Code: "bad_response"}
	}
	return &User{ID: out.Data.ID, Username: out.Data.Username, Name: out.Data.Name}, nil
}
