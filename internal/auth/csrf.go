package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// CSRF issues and checks synchronizer tokens bound to a per-browser random
// cookie value (signed double submit). A forged form from another origin can
// neither read the cookie nor compute the HMAC without the server key.
type CSRF struct{ key []byte }

// NewCSRF returns a CSRF helper with a dedicated HMAC key.
func NewCSRF(key []byte) *CSRF { return &CSRF{key: append([]byte(nil), key...)} }

// Token returns the form token for a cookie value.
func (c *CSRF) Token(cookieVal string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte("csrf:" + cookieVal))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Valid reports whether token matches cookieVal.
func (c *CSRF) Valid(cookieVal, token string) bool {
	if cookieVal == "" || token == "" {
		return false
	}
	return hmac.Equal([]byte(c.Token(cookieVal)), []byte(token))
}
