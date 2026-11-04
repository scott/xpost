package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/scott/xpost/internal/store"
)

func TestCleanWebhookAuthStripsHeaderName(t *testing.T) {
	strip := map[string]string{
		"Authorization: Bearer x":         "Bearer x",
		"authorization:Bearer x":          "Bearer x",
		"AUTHORIZATION:   Bearer x  ":     "Bearer x",
		"  Authorization:\tBearer x":      "Bearer x",
		"Bearer x":                        "Bearer x",
		"Basic dXNlcjpwYXNz":              "Basic dXNlcjpwYXNz",
		"Authorization: Authorization: x": "Authorization: x", // one copy only
		"x Authorization: y":              "x Authorization: y",
		"":                                "", // empty keeps the saved value
		"   ":                             "",
	}
	for in, want := range strip {
		got, err := store.CleanWebhookAuth(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	// Space before the colon is not the header name: kept as pasted.
	if got, err := store.CleanWebhookAuth("  AUTHORIZATION :"); err != nil || got != "AUTHORIZATION :" {
		t.Errorf("space before colon: %q, %v", got, err)
	}
	// "Bearer" is never added or removed.
	if got, _ := store.CleanWebhookAuth("Authorization: x"); got != "x" {
		t.Errorf("bare token: %q", got)
	}
	for _, bare := range []string{"Authorization:", "authorization:  ", " AUTHORIZATION:\t"} {
		_, err := store.CleanWebhookAuth(bare)
		var ie *store.InputError
		if err == nil || !errors.As(err, &ie) || !strings.Contains(err.Error(), "paste the Authorization header value") {
			t.Errorf("%q: %v", bare, err)
		}
	}
	// Line breaks after the prefix are still refused.
	if _, err := store.CleanWebhookAuth("Authorization: Bearer x\r\nX-Injected: 1"); err == nil {
		t.Error("line break after prefix accepted")
	}
	if _, err := store.CleanWebhookAuth("Authorization: " + strings.Repeat("a", store.MaxWebhookAuth+1)); err == nil {
		t.Error("overlong value accepted")
	}
}
