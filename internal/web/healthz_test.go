package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.Version = "0123456789abcdef0123456789abcdef01234567" })
	resp, err := http.Get(e.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !body.OK || body.Version != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("healthz: %d %+v", resp.StatusCode, body)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("healthz headers: %v", resp.Header)
	}
	for _, m := range []string{"POST", "DELETE"} {
		req, _ := http.NewRequest(m, e.ts.URL+"/healthz", nil)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == 200 {
			t.Fatalf("%s /healthz: 200", m)
		}
	}
}

func TestHealthzFailsWhenDatabaseIsDown(t *testing.T) {
	e := newEnv(t, nil)
	e.st.DB.Close()
	resp, err := http.Get(e.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable || strings.TrimSpace(string(b)) != `{"ok":false}` {
		t.Fatalf("healthz with closed DB: %d %q", resp.StatusCode, b)
	}
	if strings.Contains(e.logs.String(), "sql:") {
		t.Fatalf("raw DB error logged: %q", e.logs.String())
	}
}
