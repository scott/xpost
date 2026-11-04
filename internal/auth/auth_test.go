package auth

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Cheap argon2 for tests; VerifyPassword reads params from the hash.
	DefaultParams = Argon2Params{Memory: 1024, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	os.Exit(m.Run())
}

func TestPasswordHashVerify(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("unexpected hash format %q", h)
	}
	if err := VerifyPassword(h, "correct horse battery"); err != nil {
		t.Fatalf("verify good password: %v", err)
	}
	if err := VerifyPassword(h, "correct horse batterY"); err != ErrMismatch {
		t.Fatalf("verify bad password = %v, want ErrMismatch", err)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("same password produced identical hashes (salt not random)")
	}
}

func TestVerifyPasswordUsesParamsFromHash(t *testing.T) {
	old := DefaultParams
	DefaultParams = Argon2Params{Memory: 2048, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
	h, _ := HashPassword("long enough password")
	DefaultParams = old
	if err := VerifyPassword(h, "long enough password"); err != nil {
		t.Fatalf("hash made with other params should still verify: %v", err)
	}
}

func TestVerifyPasswordRejectsMalformed(t *testing.T) {
	for _, h := range []string{"", "plain", "$2a$10$abc", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=18$m=1024,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=19$m=x$c2FsdA$a2V5", "$argon2id$v=19$m=1024,t=1,p=1$***$a2V5", "$argon2id$v=19$m=1024,t=1,p=1$c2FsdA$"} {
		if err := VerifyPassword(h, "whatever"); err == nil {
			t.Errorf("malformed hash %q verified", h)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("short"); err == nil {
		t.Error("short password accepted")
	}
	if err := ValidatePassword("twelve chars"); err != nil {
		t.Errorf("12-char password rejected: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 2000)); err == nil {
		t.Error("huge password accepted")
	}
}

func TestTokens(t *testing.T) {
	a, _ := NewToken()
	b, _ := NewToken()
	if a == b || len(a) < 40 {
		t.Fatalf("weak tokens %q %q", a, b)
	}
	if HashToken(a) == a || HashToken(a) != HashToken(a) || HashToken(a) == HashToken(b) {
		t.Fatal("HashToken not a stable distinct hash")
	}
}

// RFC 6238 Appendix B vectors (SHA1, secret "12345678901234567890"), last 6 digits.
func TestCSRF(t *testing.T) {
	c := NewCSRF([]byte("0123456789abcdef0123456789abcdef"))
	tok := c.Token("cookie-a")
	if !c.Valid("cookie-a", tok) {
		t.Fatal("valid token rejected")
	}
	if c.Valid("cookie-b", tok) {
		t.Fatal("token valid for another cookie")
	}
	if c.Valid("", c.Token("")) || c.Valid("cookie-a", "") {
		t.Fatal("empty values accepted")
	}
	other := NewCSRF([]byte("ffffffffffffffffffffffffffffffff"))
	if other.Valid("cookie-a", tok) {
		t.Fatal("token valid under another key")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := NewLimiter(3, 10*time.Minute)
	l.SetClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if b, _ := l.Blocked("ip:203.0.113.7"); b {
			t.Fatalf("blocked after %d failures", i)
		}
		l.Fail("ip:203.0.113.7")
		now = now.Add(time.Minute)
	}
	b, retry := l.Blocked("ip:203.0.113.7")
	if !b {
		t.Fatal("not blocked after 3 failures")
	}
	if retry != 7*time.Minute {
		t.Fatalf("retry after %v, want 7m", retry)
	}
	if b, _ := l.Blocked("ip:198.51.100.1"); b {
		t.Fatal("other key blocked")
	}
	now = now.Add(7*time.Minute + time.Second)
	if b, _ := l.Blocked("ip:203.0.113.7"); b {
		t.Fatal("still blocked after oldest failure left the window")
	}
	l.Fail("k")
	l.Fail("k")
	l.Fail("k")
	l.Reset("k")
	if b, _ := l.Blocked("k"); b {
		t.Fatal("blocked after reset")
	}
}

func TestFailLineSanitizes(t *testing.T) {
	l := FailLine("not-an-ip\nxpost auth: login failed ip=1.2.3.4", "Bad Reason!")
	if strings.Contains(l, "\n") || strings.Contains(l, "1.2.3.4") {
		t.Fatalf("injection not neutralized: %q", l)
	}
	if l != "xpost auth: login failed ip=invalid reason=other" {
		t.Fatalf("got %q", l)
	}
}
