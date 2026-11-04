package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// An invented Authorization value, built at run time so no file holds it
// whole.
var demoAuth = "Bearer " + "demo" + "-routine-value-" + "7c41e2"

func verifySig(t *testing.T, h hookHit) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(hookKey))
	mac.Write([]byte(h.h.Get("X-Xpost-Timestamp") + "." + string(h.body)))
	if h.h.Get("X-Xpost-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal("signature does not verify")
	}
}

func (e *rwEnv) hookForm(extra url.Values) resp {
	e.t.Helper()
	return e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), extra)
}

func TestWebhookAuthorizationHeader(t *testing.T) {
	e := newRewriteEnv(t)
	// Empty: no Authorization header at all, on test and on a request.
	e.hookForm(url.Values{"op": {"test"}})
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text one.")
	if r := e.rewrite(id, ""); r.status != http.StatusSeeOther {
		t.Fatalf("rewrite: %d", r.status)
	}
	for _, h := range e.hook.hits() {
		if v := h.h.Values("Authorization"); len(v) != 0 {
			t.Fatalf("%s sent Authorization with none saved", h.h.Get("X-Xpost-Event"))
		}
	}
	page := e.c.get("/settings").body
	mustHaveRW(t, page, "Authorization header", `name="auth"`, "(optional)")
	mustLackRW(t, page, `value="authclear"`)

	// Save it (URL kept, key kept). Leading and trailing space is trimmed.
	if r := e.hookForm(url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/hook"}, "auth": {"  " + demoAuth + " \n"}}); r.status != http.StatusSeeOther || !strings.Contains(r.loc, "done=hooksaved") {
		t.Fatalf("save auth: %d %s", r.status, r.loc)
	}
	if r := e.hookForm(url.Values{"op": {"test"}}); !strings.Contains(r.loc, "done=hooktested") {
		t.Fatalf("test: %s", r.loc)
	}
	id2 := e.propose(e.sec, sm, testNow.Add(2*time.Hour), "Sample text two.")
	if r := e.rewrite(id2, "Sample note"); r.status != http.StatusSeeOther {
		t.Fatalf("rewrite 2: %d", r.status)
	}
	hits := e.hook.hits()
	seen := map[string]bool{}
	for _, h := range hits[2:] {
		ev := h.h.Get("X-Xpost-Event")
		if got := h.h.Values("Authorization"); len(got) != 1 || got[0] != demoAuth {
			t.Fatalf("%s: Authorization %q", ev, got)
		}
		verifySig(t, h) // the HMAC is unchanged
		if strings.Contains(string(h.body), demoAuth) {
			t.Fatalf("%s: the value is in the payload", ev)
		}
		seen[ev] = true
	}
	if !seen["rewrite.test"] || !seen["rewrite.requested"] {
		t.Fatalf("events with the header: %v", seen)
	}

	// Never rendered: only "Set", Replace and Clear.
	page = e.c.get("/settings").body
	mustHaveRW(t, page, ">Set</span>", "Replace", `value="authclear"`, "Never shown again.")
	mustLackRW(t, page, demoAuth, demoAuth[7:], "7c41e2")

	// Sealed at rest, bound to its row and column.
	var sealed, key []byte
	if err := e.st.DB.QueryRow(`SELECT auth_sealed, key_sealed FROM agent_webhooks WHERE token_id = ?`, e.tok.ID).Scan(&sealed, &key); err != nil {
		t.Fatal(err)
	}
	if len(sealed) == 0 || strings.Contains(string(sealed), demoAuth[7:]) || strings.Contains(string(sealed), "Bearer") {
		t.Fatal("Authorization value stored in the clear")
	}
	if v, err := e.st.WebhookAuth(context.Background(), e.tok.ID); err != nil || v != demoAuth {
		t.Fatalf("open: %v", err)
	}
	e.st.DB.Exec(`UPDATE agent_webhooks SET key_sealed = ? WHERE token_id = ?`, sealed, e.tok.ID)
	if _, _, err := e.st.WebhookSecret(context.Background(), e.tok.ID); err == nil {
		t.Fatal("the sealed Authorization value opened as a sender key")
	}
	e.st.DB.Exec(`UPDATE agent_webhooks SET key_sealed = ? WHERE token_id = ?`, key, e.tok.ID)

	// A URL-only save keeps it; Clear removes it.
	e.hookForm(url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/hook2"}})
	if v, _ := e.st.WebhookAuth(context.Background(), e.tok.ID); v != demoAuth {
		t.Fatal("URL-only save lost the Authorization value")
	}
	if r := e.hookForm(url.Values{"op": {"authclear"}}); !strings.Contains(r.loc, "done=hookauthcleared") {
		t.Fatalf("clear: %s", r.loc)
	}
	n := len(e.hook.hits())
	e.hookForm(url.Values{"op": {"test"}})
	if h := e.hook.hits(); len(h) != n+1 || len(h[n].h.Values("Authorization")) != 0 {
		t.Fatal("Authorization still sent after Clear")
	}
	mustLackRW(t, e.c.get("/settings").body, `value="authclear"`)

	// Never logged.
	logs := e.logs.String()
	mustLackRW(t, logs, demoAuth, demoAuth[7:], "7c41e2", "Authorization:")
}

func TestWebhookAuthorizationRejectsLineBreaks(t *testing.T) {
	e := newRewriteEnv(t)
	for _, bad := range []string{"Bearer abc\r\nX-Injected: 1", "Bearer abc\ndef", "Bearer abc\rdef", "Bearer \x00abc", "Bearer \x1babc"} {
		r := e.hookForm(url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/other"}, "auth": {bad}})
		if r.status != http.StatusBadRequest {
			t.Fatalf("%q: %d", bad, r.status)
		}
		if strings.ContainsAny(bad, "\r\n") && !strings.Contains(r.body, "The Authorization header must be one line: remove the line break.") {
			t.Errorf("%q: no clear error", bad)
		}
		mustLackRW(t, r.body, "abc")
	}
	// Nothing was saved, not even the URL that came with it.
	w, _ := e.st.AgentWebhook(context.Background(), e.tok.ID)
	if w.HasAuth || strings.HasSuffix(w.URL, "/other") {
		t.Fatalf("a refused save changed the webhook: %+v", w)
	}
	if _, err := store.CleanWebhookAuth(strings.Repeat("a", store.MaxWebhookAuth+1)); err == nil {
		t.Fatal("overlong value accepted")
	}
	if err := e.st.SetAgentWebhookAuth(context.Background(), e.owner, 999, demoAuth); err == nil {
		t.Fatal("Authorization saved without a webhook")
	}
}

// A failed test names the HTTP status in the flash and the chip.
func TestWebhookTestFailureShowsStatus(t *testing.T) {
	e := newRewriteEnv(t)
	e.hook.setStatus(http.StatusUnauthorized)
	r := e.hookForm(url.Values{"op": {"test"}})
	if !strings.Contains(r.loc, "done=hookfailed&agent="+strconvI(e.tok.ID)) {
		t.Fatalf("redirect %s", r.loc)
	}
	page := e.c.get(r.loc[:strings.Index(r.loc, "#")]).body
	mustHaveRW(t, page, "Test failed: HTTP 401. The receiver refused the request; check the Authorization header. Rewrite stays on.", "Test failed · HTTP 401", "did not accept the test (HTTP 401)")
	e.hook.setStatus(http.StatusInternalServerError)
	r = e.hookForm(url.Values{"op": {"test"}})
	mustHaveRW(t, e.c.get(r.loc[:strings.Index(r.loc, "#")]).body, "Test failed: HTTP 500. Rewrite stays on; check the URL and key.", "Test failed · HTTP 500")
}

// A pasted full header line ("Authorization: Bearer ...") is saved as the
// value alone, so the receiver gets one Authorization, not two names.
func TestWebhookAuthorizationStripsPastedName(t *testing.T) {
	e := newRewriteEnv(t)
	for _, bare := range []string{"Authorization:", "authorization:   "} {
		r := e.hookForm(url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/other"}, "auth": {bare}})
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, "Paste the Authorization header value, not just the name.") {
			t.Fatalf("%q: %d", bare, r.status)
		}
	}
	if w, _ := e.st.AgentWebhook(context.Background(), e.tok.ID); w.HasAuth || strings.HasSuffix(w.URL, "/other") {
		t.Fatalf("a refused save changed the webhook: %+v", w)
	}
	if r := e.hookForm(url.Values{"op": {"save"}, "url": {e.hook.srv.URL + "/hook"}, "auth": {"Authorization: " + demoAuth}}); !strings.Contains(r.loc, "done=hooksaved") {
		t.Fatalf("save: %d %s", r.status, r.loc)
	}
	if r := e.hookForm(url.Values{"op": {"test"}}); !strings.Contains(r.loc, "done=hooktested") {
		t.Fatalf("test: %s", r.loc)
	}
	id := e.propose(e.sec, sm, testNow.Add(time.Hour), "Sample text one.")
	if r := e.rewrite(id, ""); r.status != http.StatusSeeOther {
		t.Fatalf("rewrite: %d", r.status)
	}
	hits := e.hook.hits()
	if len(hits) != 2 {
		t.Fatalf("hits: %d", len(hits))
	}
	for _, h := range hits {
		if got := h.h.Values("Authorization"); len(got) != 1 || got[0] != demoAuth {
			t.Fatalf("%s: Authorization %q", h.h.Get("X-Xpost-Event"), got)
		}
	}
	mustHaveRW(t, e.c.get("/settings").body, "e.g. Bearer ...", "A leading Authorization: is removed.")
	mustLackRW(t, e.logs.String(), demoAuth, "7c41e2")
}
