// Package xauth connects xpost accounts to X with OAuth 2.0 Authorization
// Code + PKCE (S256), and keeps their tokens fresh.
//
// This file is the only one that names X's hosts, as defaults for the
// OAuth, users/me, create-post and media endpoints and the public post
// links.
// Tests and CI point every endpoint at a local fake (package xfake). The
// one posting call lives in posts.go and runs only from the dispatcher,
// which cmd/xpost starts only when XPOST_SCHEDULER=on.
package xauth

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
)

// CallbackPath is where X sends the owner back. Register
// <base URL>/auth/x/callback in the X developer console.
const CallbackPath = "/auth/x/callback"

// Scopes requested on every connect.
var Scopes = []string{"tweet.read", "users.read", "tweet.write", "media.write", "offline.access"}

// Default endpoints: X's OAuth 2.0 and users/me (checked 2026-11-01 against
// docs.x.com). Override with the XPOST_X_*_URL settings.
const (
	DefaultAuthorizeURL = "https://x.com/i/oauth2/authorize"
	DefaultTokenURL     = "https://api.x.com/2/oauth2/token"
	DefaultRevokeURL    = "https://api.x.com/2/oauth2/revoke"
	DefaultMeURL        = "https://api.x.com/2/users/me"
	// Create a post (checked 2026-11-01 against docs.x.com: POST
	// /2/tweets, JSON body {text, reply.in_reply_to_tweet_id}).
	DefaultPostURL = "https://api.x.com/2/tweets"
	// Media (checked 2026-11-01 against docs.x.com): POST /2/media/upload
	// (multipart: media, media_category tweet_image or tweet_gif) returns
	// data.id, expires_after_secs and, while X processes it,
	// processing_info; GET on the same URL with command=STATUS&media_id
	// checks processing. POST /2/media/metadata sets alt text:
	// {id, metadata: {alt_text: {text}}}.
	DefaultMediaURL         = "https://api.x.com/2/media/upload"
	DefaultMediaMetadataURL = "https://api.x.com/2/media/metadata"
	// Public post links: <web>/<username>/status/<id>.
	DefaultWebURL = "https://x.com"
)

// Config keys (environment variables).
const (
	EnvClientID         = "XPOST_X_CLIENT_ID"
	EnvClientSecret     = "XPOST_X_CLIENT_SECRET"
	EnvClientSecretFile = "XPOST_X_CLIENT_SECRET_FILE"
	EnvAuthorizeURL     = "XPOST_X_AUTHORIZE_URL"
	EnvTokenURL         = "XPOST_X_TOKEN_URL"
	EnvRevokeURL        = "XPOST_X_REVOKE_URL"
	EnvMeURL            = "XPOST_X_ME_URL"
	EnvPostURL          = "XPOST_X_POST_URL"
	EnvMediaURL         = "XPOST_X_MEDIA_URL"
	EnvMediaMetaURL     = "XPOST_X_MEDIA_METADATA_URL"
	EnvWebURL           = "XPOST_X_WEB_URL"
)

// Config is the OAuth client configuration. ClientSecret is a secret: never
// log or render it.
type Config struct {
	ClientID     string
	ClientSecret string // empty means a public client (client_id in the body)
	AuthorizeURL string
	TokenURL     string
	RevokeURL    string
	MeURL        string
	PostURL      string
	MediaURL     string
	MediaMetaURL string
	WebURL       string
	RedirectURL  string // BaseURL + CallbackPath
	Timeout      time.Duration
}

// Enabled reports whether Connect can work (a client id is configured).
func (c *Config) Enabled() bool { return c != nil && c.ClientID != "" }

// FromEnv reads the config keys. A missing client id is not an error: the
// app runs with Connect disabled.
func FromEnv(getenv func(string) string, baseURL string) (*Config, error) {
	c := &Config{
		ClientID:     strings.TrimSpace(getenv(EnvClientID)),
		ClientSecret: strings.TrimSpace(getenv(EnvClientSecret)),
		AuthorizeURL: or(getenv(EnvAuthorizeURL), DefaultAuthorizeURL),
		TokenURL:     or(getenv(EnvTokenURL), DefaultTokenURL),
		RevokeURL:    or(getenv(EnvRevokeURL), DefaultRevokeURL),
		MeURL:        or(getenv(EnvMeURL), DefaultMeURL),
		PostURL:      or(getenv(EnvPostURL), DefaultPostURL),
		MediaURL:     or(getenv(EnvMediaURL), DefaultMediaURL),
		MediaMetaURL: or(getenv(EnvMediaMetaURL), DefaultMediaMetadataURL),
		WebURL:       strings.TrimRight(or(getenv(EnvWebURL), DefaultWebURL), "/"),
		RedirectURL:  strings.TrimRight(baseURL, "/") + CallbackPath,
	}
	if f := strings.TrimSpace(getenv(EnvClientSecretFile)); f != "" {
		if c.ClientSecret != "" {
			return nil, fmt.Errorf("set %s or %s, not both", EnvClientSecret, EnvClientSecretFile)
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvClientSecretFile, err)
		}
		c.ClientSecret = strings.TrimSpace(string(b))
	}
	if c.ClientSecret != "" && c.ClientID == "" {
		return nil, fmt.Errorf("%s is set but %s is not", EnvClientSecret, EnvClientID)
	}
	return c, c.Validate()
}

func or(v, def string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return def
}

// Validate checks every URL is https, or http on a loopback host (a local
// fake provider).
func (c *Config) Validate() error {
	for name, raw := range map[string]string{"authorize": c.AuthorizeURL, "token": c.TokenURL, "revoke": c.RevokeURL, "users/me": c.MeURL, "post": c.PostURL, "media": c.MediaURL, "media metadata": c.MediaMetaURL, "web": c.WebURL, "callback": c.RedirectURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return fmt.Errorf("x oauth: %s URL %q is not absolute", name, raw)
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && loopback(u.Hostname())) {
			return fmt.Errorf("x oauth: %s URL must be https (http only on localhost)", name)
		}
	}
	return nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// StatusURL is the public link for a post.
func (c *Config) StatusURL(username, id string) string {
	return c.WebURL + "/" + url.PathEscape(username) + "/status/" + url.PathEscape(id)
}

// IntentURL is x.com's compose page with a reply filled in: the owner
// presses Post there. It is a link for the owner's browser; xpost itself
// never requests it.
func (c *Config) IntentURL(text, inReplyTo string) string {
	// Spaces as %20, not +: every decoder reads %20 as a space.
	esc := func(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }
	return c.WebURL + "/intent/post?text=" + esc(text) + "&in_reply_to=" + esc(inReplyTo)
}

// statusHosts are the hosts a pasted x.com post link may use.
var statusHosts = map[string]bool{"x.com": true, "www.x.com": true, "mobile.x.com": true}

// Status link errors.
var (
	ErrNotXLink    = errors.New("not an x.com link")
	ErrProfileLink = errors.New("a profile link, not a post")
	ErrNotPostLink = errors.New("not a link to a post")
)

// ParseStatusLink reads an x.com post link (https:// optional; www. and
// mobile. allowed; a query, fragment or trailing slash is ignored) and
// returns its handle and post id. It checks the format only.
func ParseStatusLink(raw string) (handle, id string, err error) {
	v := strings.TrimSpace(raw)
	if v == "" || strings.ContainsAny(v, " \t\r\n\"'<>\\`") {
		return "", "", ErrNotXLink
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, perr := url.Parse(v)
	if perr != nil || u.Scheme != "https" && u.Scheme != "http" || u.User != nil || u.Port() != "" || !statusHosts[strings.ToLower(u.Hostname())] {
		return "", "", ErrNotXLink
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(segs) == 1 && segs[0] != "":
		return "", "", ErrProfileLink
	case len(segs) == 3 && segs[1] == "status" && segs[0] != "" && segs[2] != "" && len(segs[2]) <= 30 && strings.Trim(segs[2], "0123456789") == "":
		return segs[0], segs[2], nil
	}
	return "", "", ErrNotPostLink
}

// ErrNotConfigured is returned when no client id is set.
var ErrNotConfigured = errors.New("X connection is not configured on this server")
