package secret

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	ct, err := k.Seal([]byte("JBSWY3DPEHPK3PXP"), []byte("users.totp_secret:1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("JBSWY3DPEHPK3PXP")) {
		t.Fatal("ciphertext contains plaintext")
	}
	pt, err := k.Open(ct, []byte("users.totp_secret:1"))
	if err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open = %q, %v", pt, err)
	}
	if _, err := k.Open(ct, []byte("users.totp_secret:2")); err == nil {
		t.Fatal("open with wrong aad should fail")
	}
	other, _ := NewKey()
	if _, err := other.Open(ct, []byte("users.totp_secret:1")); err == nil {
		t.Fatal("open with wrong key should fail")
	}
}

func TestParseKey(t *testing.T) {
	k, _ := NewKey()
	k2, err := ParseKey(k.Encode() + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k.Derive("x"), k2.Derive("x")) {
		t.Fatal("parsed key differs")
	}
	if _, err := ParseKey("c2hvcnQ="); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := ParseKey("!!!"); err == nil {
		t.Fatal("non-base64 accepted")
	}
	if bytes.Equal(k.Derive("a"), k.Derive("b")) {
		t.Fatal("derived subkeys collide")
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("XPOST_KEY", "")
	t.Setenv("XPOST_KEY_FILE", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("missing key should error")
	}
	k, _ := NewKey()
	t.Setenv("XPOST_KEY", k.Encode())
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
}
