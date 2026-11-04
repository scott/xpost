package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// Tests use localhost as the relying party, like local dev.
const (
	testRPID   = "localhost"
	testOrigin = "http://localhost:8095"
)

// softKey is a software authenticator for tests: a P-256 key, "none"
// attestation and hand-built CBOR. Keys are generated per test; nothing
// is real or fixed.
type softKey struct {
	t          *testing.T
	rpID       string // hashed into authenticator data
	origin     string // written into client data
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	count      uint32
	flags      byte // extra flags (backup bits); UP and UV are always set
	noUV       bool
}

func newSoftKey(t *testing.T) *softKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	rand.Read(id)
	return &softKey{t: t, rpID: testRPID, origin: testOrigin, key: k, credID: id}
}

var b64u = base64.RawURLEncoding

// Minimal CBOR (RFC 8949) for the few shapes WebAuthn needs.
func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return []byte{major<<5 | 25, byte(n >> 8), byte(n)}
	}
	return []byte{major<<5 | 26, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
}

func cborInt(v int) []byte {
	if v >= 0 {
		return cborHead(0, uint64(v))
	}
	return cborHead(1, uint64(-1-v))
}
func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }
func cborMap(n int) []byte      { return cborHead(5, uint64(n)) }
func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func (k *softKey) coseKey() []byte {
	x, y := make([]byte, 32), make([]byte, 32)
	k.key.PublicKey.X.FillBytes(x)
	k.key.PublicKey.Y.FillBytes(y)
	return cat(cborMap(5), cborInt(1), cborInt(2), cborInt(3), cborInt(-7), cborInt(-1), cborInt(1), cborInt(-2), cborBytes(x), cborInt(-3), cborBytes(y))
}

func (k *softKey) authData(attested bool) []byte {
	h := sha256.Sum256([]byte(k.rpID))
	flags := byte(0x01) | k.flags // UP
	if !k.noUV {
		flags |= 0x04
	}
	if attested {
		flags |= 0x40
	}
	cnt := make([]byte, 4)
	binary.BigEndian.PutUint32(cnt, k.count)
	ad := cat(h[:], []byte{flags}, cnt)
	if attested {
		l := make([]byte, 2)
		binary.BigEndian.PutUint16(l, uint16(len(k.credID)))
		ad = cat(ad, make([]byte, 16), l, k.credID, k.coseKey()) // zero AAGUID, like "none" attestation
	}
	return ad
}

func (k *softKey) clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": k.origin, "crossOrigin": false})
	return b
}

type wireOptions struct {
	PublicKey struct {
		Challenge        string                `json:"challenge"`
		User             struct{ ID string }   `json:"user"`
		AllowCredentials []struct{ ID string } `json:"allowCredentials"`
	} `json:"publicKey"`
}

func parseOptions(t *testing.T, body string) wireOptions {
	t.Helper()
	var o wireOptions
	if err := json.Unmarshal([]byte(body), &o); err != nil || o.PublicKey.Challenge == "" {
		t.Fatalf("options %v: %s", err, body)
	}
	return o
}

// create answers navigator.credentials.create.
func (k *softKey) create(optionsJSON string) string {
	o := parseOptions(k.t, optionsJSON)
	k.userHandle, _ = b64u.DecodeString(o.PublicKey.User.ID)
	att := cat(cborMap(3), cborText("fmt"), cborText("none"), cborText("attStmt"), cborMap(0), cborText("authData"), cborBytes(k.authData(true)))
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.credID), "rawId": b64u.EncodeToString(k.credID), "type": "public-key",
		"authenticatorAttachment": "platform", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(k.clientData("webauthn.create", o.PublicKey.Challenge)),
			"attestationObject": b64u.EncodeToString(att), "transports": []string{"internal", "hybrid"}},
	})
	return string(b)
}

// get answers navigator.credentials.get; each use bumps the counter.
func (k *softKey) get(optionsJSON string) string {
	o := parseOptions(k.t, optionsJSON)
	k.count++
	ad := k.authData(false)
	cd := k.clientData("webauthn.get", o.PublicKey.Challenge)
	h := sha256.Sum256(cd)
	digest := sha256.Sum256(cat(ad, h[:]))
	sig, err := ecdsa.SignASN1(rand.Reader, k.key, digest[:])
	if err != nil {
		k.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"id": b64u.EncodeToString(k.credID), "rawId": b64u.EncodeToString(k.credID), "type": "public-key", "clientExtensionResults": map[string]any{},
		"response": map[string]any{"clientDataJSON": b64u.EncodeToString(cd), "authenticatorData": b64u.EncodeToString(ad),
			"signature": b64u.EncodeToString(sig), "userHandle": b64u.EncodeToString(k.userHandle)},
	})
	return string(b)
}
