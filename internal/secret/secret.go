// Package secret holds xpost's master key and the field encryption built on it
// (AES-256-GCM). The key comes from the environment, never from the database.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// KeySize is the master key length in bytes.
const KeySize = 32

// Key is the master key. Subkeys are derived per purpose so one leak (for
// example a CSRF HMAC) never exposes the field-encryption key.
type Key struct{ k [KeySize]byte }

// NewKey returns a random master key.
func NewKey() (*Key, error) {
	var k Key
	if _, err := rand.Read(k.k[:]); err != nil {
		return nil, err
	}
	return &k, nil
}

// Encode returns the key as base64 (for writing an env file).
func (k *Key) Encode() string { return base64.StdEncoding.EncodeToString(k.k[:]) }

// ParseKey decodes a base64 master key.
func ParseKey(s string) (*Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("secret: key is not base64")
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("secret: key must be %d bytes, got %d", KeySize, len(b))
	}
	var k Key
	copy(k.k[:], b)
	return &k, nil
}

// FromEnv loads the key from XPOST_KEY, or from the file named by XPOST_KEY_FILE.
func FromEnv() (*Key, error) {
	if v := os.Getenv("XPOST_KEY"); v != "" {
		return ParseKey(v)
	}
	if p := os.Getenv("XPOST_KEY_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("secret: read XPOST_KEY_FILE: %w", err)
		}
		return ParseKey(string(b))
	}
	return nil, errors.New("secret: set XPOST_KEY or XPOST_KEY_FILE (create one with `xpost keygen`)")
}

// Derive returns a 32-byte subkey for a purpose label.
func (k *Key) Derive(purpose string) []byte {
	m := hmac.New(sha256.New, k.k[:])
	m.Write([]byte("xpost/v1/" + purpose))
	return m.Sum(nil)
}

func (k *Key) aead() (cipher.AEAD, error) {
	b, err := aes.NewCipher(k.Derive("field-encryption"))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Seal encrypts plaintext. aad binds the ciphertext to its context (for
// example "users.totp_secret:42") so it cannot be swapped between rows.
func (k *Key) Seal(plaintext, aad []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, plaintext, aad), nil
}

// Open decrypts a value produced by Seal with the same aad.
func (k *Key) Open(ciphertext, aad []byte) ([]byte, error) {
	g, err := k.aead()
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < g.NonceSize() {
		return nil, errors.New("secret: ciphertext too short")
	}
	n := g.NonceSize()
	return g.Open(nil, ciphertext[:n], ciphertext[n:], aad)
}
