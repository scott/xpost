package blog

import (
	"net/url"
	"testing"
)

// SameHost: case-insensitive host, port-aware with https as 443.
func TestSameHost(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://notes.example.test/blog/x", "https://Notes.Example.Test", true},
		{"https://notes.example.test:443/blog/x", "https://notes.example.test", true},
		{"https://notes.example.test/blog/x", "https://notes.example.test:443/", true},
		{"https://notes.example.test/blog/x", "https://notes.example.test:8443", false},
		{"https://notes.example.test:8443/blog/x", "https://notes.example.test:9443", false},
		{"https://notes.example.test.evil.test/blog/x", "https://notes.example.test", false},
		{"https://other.example.test/blog/x", "https://notes.example.test", false},
		{"https:///blog/x", "https://notes.example.test", false},
		{"https://[::1]:8443/blog/x", "https://[::1]:8443", true},
		// Unicode case folding is not the same host: KELVIN SIGN folds to
		// k and LONG S to s, but DNS sees different names.
		{"https://\u212Aample.example.test/blog/x", "https://kample.example.test", false},
		{"https://\u017Fample.example.test/blog/x", "https://sample.example.test", false},
		{"https://kample.example.test/blog/x", "https://\u212Aample.example.test", false},
		{"https://KAMPLE.example.test/blog/x", "https://kample.example.test", true},
	} {
		a, _ := url.Parse(c.a)
		b, _ := url.Parse(c.b)
		if got := SameHost(a, b); got != c.want {
			t.Errorf("%s vs %s: %v", c.a, c.b, got)
		}
	}
}

// SameHost with a nil URL is false, not a panic.
func TestSameHostNil(t *testing.T) {
	u, _ := url.Parse("https://notes.example.test/blog/x")
	defer func() {
		if x := recover(); x != nil {
			t.Fatalf("panicked: %v", x)
		}
	}()
	if SameHost(nil, u) || SameHost(u, nil) || SameHost(nil, nil) {
		t.Fatal("a nil URL matched")
	}
}
