// Package auth holds xpost's sign-in primitives: argon2id password hashing,
// opaque tokens, CSRF tokens and the login rate limiter. Storage lives in
// the store package.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params are the argon2id cost settings encoded into each hash.
type Argon2Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams follows the OWASP argon2id guidance (64 MiB, t=3, p=2).
// Tests may lower it; verification always uses the parameters in the hash.
var DefaultParams = Argon2Params{Memory: 64 * 1024, Time: 3, Threads: 2, SaltLen: 16, KeyLen: 32}

// MinPasswordLen is the shortest password xpost accepts.
const MinPasswordLen = 12

// ErrMismatch means the password does not match the hash.
var ErrMismatch = errors.New("auth: password mismatch")

// ValidatePassword enforces xpost's password policy.
func ValidatePassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(pw) > 1024 {
		return errors.New("password is too long")
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(pw string) (string, error) {
	p := DefaultParams
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash from HashPassword in constant time.
func VerifyPassword(hash, pw string) error {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("auth: unsupported hash format")
	}
	var v int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return errors.New("auth: unsupported argon2 version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return errors.New("auth: bad argon2 params")
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return errors.New("auth: bad salt")
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return errors.New("auth: bad key")
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// dummyHash lets callers spend the same time on unknown emails as on known
// ones, so response timing does not reveal which emails have accounts.
var dummyHash string

// BurnPasswordTime runs a verification against a throwaway hash.
func BurnPasswordTime(pw string) {
	if dummyHash == "" {
		dummyHash, _ = HashPassword("xpost-dummy-password-for-timing")
	}
	_ = VerifyPassword(dummyHash, pw)
}
