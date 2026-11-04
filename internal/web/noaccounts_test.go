package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// noAccountsEnv is a fresh install: the 0001 migration no longer seeds X
// accounts (the harness adds two sample ones; this removes them).
func noAccountsEnv(t *testing.T) (*env, *client) {
	t.Helper()
	e := newEnv(t, nil)
	if _, err := e.st.DB.ExecContext(context.Background(), `DELETE FROM accounts WHERE kind = 'x'`); err != nil {
		t.Fatal(err)
	}
	c, _ := e.setupOwner()
	return e, c
}

// A first run with no accounts renders every page, with an empty state
// pointing to Settings > X accounts; New post sends you there.
func TestFirstRunNoAccounts(t *testing.T) {
	e, c := noAccountsEnv(t)
	for _, p := range []string{"/queue", "/queue?status=approved", "/schedule", "/log", "/settings"} {
		if r := c.get(p); r.status != http.StatusOK {
			t.Errorf("GET %s: %d", p, r.status)
		}
	}
	q := c.get("/queue").body
	mustHave(t, q, `data-noaccts`, `<a href="/settings/accounts">Add one in Settings &gt; X accounts</a>`)
	sc := c.get("/schedule").body
	mustHave(t, sc, `data-noaccts`)
	mustLack(t, sc, `class="sect">Accounts`)
	mustLack(t, q, "@sample_writer", "@sample_maker")
	a := c.get("/settings/accounts").body
	mustHave(t, a, "No X accounts yet.", `action="/settings/accounts" data-addacct`, `name="handle"`)
	if r := c.get("/compose"); r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=noaccts" {
		t.Fatalf("compose: %d %q", r.status, r.loc)
	}
	mustHave(t, c.get("/settings/accounts?done=noaccts").body, "Add an X account first.")
	// An agent cannot propose for an account that is not there.
	_, sec := e.token("sample-agent")
	if r := e.api("POST", "/api/v1/drafts", sec, draftBody(sm, testNow.Add(time.Hour), "Example post")); r.status == http.StatusCreated {
		t.Fatalf("proposal with no accounts: %d", r.status)
	}
}

// Settings > X accounts adds an account by handle: CSRF required, the @
// optional, bad or duplicate handles refused, initials from the handle.
func TestAddAccount(t *testing.T) {
	e, c := noAccountsEnv(t)
	if r := c.postRaw("/settings/accounts", url.Values{"handle": {"example_new"}}, nil); r.status != http.StatusForbidden {
		t.Fatalf("no csrf: %d", r.status)
	}
	for _, bad := range []string{"", "has space", "toolong_handle_16", "x.com/someone", "@"} {
		if r := c.post("/settings/accounts", "/settings/accounts", url.Values{"handle": {bad}}); r.status != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, r.status)
		}
	}
	r := c.post("/settings/accounts", "/settings/accounts", url.Values{"handle": {" @Example_New "}})
	if r.status != http.StatusSeeOther || r.loc != "/settings/accounts?done=added" {
		t.Fatalf("add: %d %q", r.status, r.loc)
	}
	if r := c.post("/settings/accounts", "/settings/accounts", url.Values{"handle": {"example_NEW"}}); r.status != http.StatusBadRequest {
		t.Fatalf("duplicate: %d", r.status)
	}
	accts, _ := e.st.Accounts(context.Background())
	if len(accts) != 1 || accts[0].Handle != "Example_New" || accts[0].Initials != "EN" || accts[0].Color != "sm" {
		t.Fatalf("accounts %+v", accts)
	}
	mustHave(t, c.get("/settings/accounts?done=added").body, "Account added.", `data-account="Example_New"`)
	mustLack(t, c.get("/queue").body, "data-noaccts")
	mustHave(t, c.get("/schedule").body, `class="sect">Accounts`)
	if r := c.get("/compose"); r.status != http.StatusOK {
		t.Fatalf("compose after add: %d", r.status)
	}
	_, sec := e.token("sample-agent")
	if r := e.api("POST", "/api/v1/drafts", sec, draftBody("Example_New", testNow.Add(time.Hour), "Example post")); r.status != http.StatusCreated {
		t.Fatalf("proposal: %d %s", r.status, r.body)
	}
}
