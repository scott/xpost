package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
)

// X account connection routes. All are owner-only (full session) and every
// POST passes CSRF. The agent API has no route here.
func (s *Server) xRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+AccountsPath, s.requireOwner(s.accountsPage))
	mux.HandleFunc("POST "+AccountsPath, s.requireOwner(s.accountAdd))
	mux.HandleFunc("POST "+AccountsPath+"/{handle}/connect", s.requireOwner(s.xConnect))
	mux.HandleFunc("GET "+xauth.CallbackPath, s.requireOwner(s.xCallback))
	mux.HandleFunc("POST "+AccountsPath+"/{handle}/disconnect", s.requireOwner(s.xDisconnect))
	mux.HandleFunc("POST "+AccountsPath+"/{handle}/check", s.requireOwner(s.xCheck))
	// The old top-level page and everything under it now live in Settings.
	mux.HandleFunc("/accounts", oldAccounts)
	mux.HandleFunc("/accounts/", oldAccounts)
}

// AccountsPath is Settings > X accounts. The X callback path
// (xauth.CallbackPath) is separate and did not move.
const AccountsPath = "/settings/accounts"

// accountsDone is where a finished action lands, with its flash key.
func accountsDone(key string) string { return AccountsPath + "?done=" + url.QueryEscape(key) }

// oldAccounts sends the old /accounts URLs (any method, any sub-path) to
// Settings > X accounts. The target path is fixed, so it can never point
// off this site; only the query string is carried over, re-encoded, minus any OAuth
// parameters. No
// action runs: an old POST lands on the page and has to be pressed again.
func oldAccounts(w http.ResponseWriter, r *http.Request) {
	to := AccountsPath
	if q, err := url.ParseQuery(r.URL.RawQuery); err == nil {
		for _, k := range []string{"code", "state", "error", "error_description"} {
			q.Del(k) // OAuth parameters never travel to another URL
		}
		if len(q) > 0 {
			to += "?" + q.Encode()
		}
	}
	w.Header().Set("Location", to)
	w.WriteHeader(http.StatusSeeOther)
}

var accountsFlash = map[string]string{
	"connected":           "Connected.",
	"disconnected":        "Disconnected. The tokens are deleted here and revoked at X.",
	"norevoke":            "Disconnected. The tokens are deleted here, but X did not confirm the revoke: remove xpost under Connected apps in X settings.",
	"checked":             "Connection checked: the token works.",
	"connected-norefresh": "Connected, but X sent no refresh token, so it will need reconnecting when the token expires.",
	"added":               "Account added. Connect it to post from it.",
	"noaccts":             "Add an X account first. New posts need an account to post as.",
}

// accountAdd adds an X account by handle (a fresh install has none).
func (s *Server) accountAdd(w http.ResponseWriter, r *http.Request, u *store.User) {
	a, err := s.st.AddAccount(r.Context(), r.PostFormValue("handle"))
	switch {
	case errors.Is(err, store.ErrBadHandle), errors.Is(err, store.ErrAccountExists):
		s.showAccounts(w, r, u, http.StatusBadRequest, err.Error(), "")
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.log.Printf("x account @%s added", a.Handle)
	http.Redirect(w, r, accountsDone("added"), http.StatusSeeOther)
}

type acctRow struct {
	A       store.Account
	Pending int
	Conn    *store.XConn
	Since   string
	Refresh string
}

func (s *Server) accountsPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.showAccounts(w, r, u, http.StatusOK, "", accountsFlash[r.URL.Query().Get("done")])
}

func (s *Server) showAccounts(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash string) {
	ctx := r.Context()
	accts, _ := s.st.Accounts(ctx)
	counts, _ := s.st.QueueCounts(ctx, "", "")
	conns, _ := s.st.XConnections(ctx)
	loc := s.zone(r)
	var rows []acctRow
	for _, a := range accts {
		row := acctRow{A: a, Pending: counts.PendingByAccount[a.Handle], Conn: conns[a.Handle]}
		if c := row.Conn; c != nil {
			row.Since = c.ConnectedAt.In(loc).Format("Jan 2, 3:04 PM")
			row.Refresh = "not yet"
			if c.RefreshedAt != nil {
				row.Refresh = s.ago(c.RefreshedAt)
			}
		}
		rows = append(rows, row)
	}
	if s.xc.Cfg.Enabled() {
		// The Connect form redirects to X's authorize page; allow that one
		// origin as a form target on this page only.
		if o := origin(s.xc.Cfg.AuthorizeURL); o != "" {
			w.Header().Set("Content-Security-Policy", strings.Replace(CSP, "form-action 'self'", "form-action 'self' "+o, 1))
		}
	}
	p := s.newPage(r, u)
	p.Nav, p.Title = "settings", "X accounts"
	p.Error, p.Flash = errMsg, flash
	p.Data = map[string]any{"Rows": rows, "Enabled": s.xc.Cfg.Enabled(), "Callback": s.callbackURL(), "Scopes": strings.Join(xauth.Scopes, " "),
		"EnvID": xauth.EnvClientID, "EnvSecret": xauth.EnvClientSecret, "EnvSecretFile": xauth.EnvClientSecretFile}
	s.render(w, status, "accounts", "app", p)
}

func (s *Server) callbackURL() string {
	if s.xc.Cfg.RedirectURL != "" {
		return s.xc.Cfg.RedirectURL
	}
	return strings.TrimRight(s.cfg.BaseURL, "/") + xauth.CallbackPath
}

func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func (s *Server) accountParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	h := r.PathValue("handle")
	accts, _ := s.st.Accounts(r.Context())
	for _, a := range accts {
		if a.Handle == h {
			return h, true
		}
	}
	s.notFound(w, r)
	return "", false
}

func (s *Server) xConnect(w http.ResponseWriter, r *http.Request, u *store.User) {
	h, ok := s.accountParam(w, r)
	if !ok {
		return
	}
	if !s.xc.Cfg.Enabled() {
		s.showAccounts(w, r, u, http.StatusServiceUnavailable, "Connecting is off: this server has no X client id configured.", "")
		return
	}
	state, err1 := xauth.NewState()
	verifier, err2 := xauth.NewVerifier()
	if err1 != nil || err2 != nil {
		s.internalError(w, errors.New("x connect: no randomness"))
		return
	}
	if err := s.st.SaveOAuthState(r.Context(), state, sessionToken(r), h, verifier); err != nil {
		s.internalError(w, errors.New("x connect: could not save state"))
		return
	}
	http.Redirect(w, r, s.xc.AuthURL(state, verifier), http.StatusSeeOther)
}

// xCallback finishes the connect. Nothing from the query (code, state) is
// ever echoed or logged.
func (s *Server) xCallback(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	q := r.URL.Query()
	fail := func(status int, msg string) { s.showAccounts(w, r, u, status, msg, "") }
	if !s.xc.Cfg.Enabled() {
		fail(http.StatusServiceUnavailable, "Connecting is off: this server has no X client id configured.")
		return
	}
	account, verifier, err := s.st.TakeOAuthState(ctx, q.Get("state"), sessionToken(r))
	if err != nil {
		s.fail(r, "", "bad_oauth_state")
		fail(http.StatusBadRequest, store.ErrOAuthState.Error())
		return
	}
	if e := q.Get("error"); e != "" {
		msg := "X did not authorize the connection."
		if e == "access_denied" {
			msg = "You cancelled at X, so nothing was connected."
		}
		fail(http.StatusBadRequest, msg)
		return
	}
	code := q.Get("code")
	if code == "" {
		fail(http.StatusBadRequest, "X sent no authorization code. Press Connect again.")
		return
	}
	tok, err := s.xc.Exchange(ctx, code, verifier, s.st.Now())
	if err != nil {
		s.log.Printf("x connect @%s: %v", account, err)
		fail(http.StatusBadGateway, "X refused the connection ("+errCode(err)+"). Press Connect again.")
		return
	}
	me, err := s.xc.Me(ctx, tok.Access)
	if err != nil {
		s.log.Printf("x connect @%s: %v", account, err)
		s.revokeQuietly(tok)
		fail(http.StatusBadGateway, "X issued a token but users/me failed ("+errCode(err)+"). Nothing was saved. Press Connect again.")
		return
	}
	if !strings.EqualFold(me.Username, account) {
		// Signed in to X as someone else: never store that account's tokens
		// under this handle.
		s.revokeQuietly(tok)
		fail(http.StatusConflict, "You authorized @"+me.Username+" but this is @"+account+". Nothing was saved. Sign in to X as @"+account+" and press Connect again.")
		return
	}
	if err := s.st.SaveXConnection(ctx, account, me.ID, me.Username, store.XTokens{Access: tok.Access, Refresh: tok.Refresh, Expiry: tok.Expiry, Scope: tok.Scope}); err != nil {
		s.revokeQuietly(tok)
		s.internalError(w, errors.New("x connect: could not store tokens"))
		return
	}
	s.log.Printf("x account @%s connected (X user id %s)", account, me.ID)
	if tok.Refresh == "" {
		http.Redirect(w, r, accountsDone("connected-norefresh"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, accountsDone("connected"), http.StatusSeeOther)
}

func (s *Server) revokeQuietly(t *xauth.Token) {
	ctx, cancel := contextTimeout(10 * time.Second)
	defer cancel()
	s.xc.Revoke(ctx, t.Refresh, "refresh_token")
	s.xc.Revoke(ctx, t.Access, "access_token")
}

func errCode(err error) string {
	var e *xauth.Error
	if errors.As(err, &e) {
		if e.Code != "" {
			return e.Code
		}
		return "HTTP " + itoaInt(e.Status)
	}
	return "network error"
}

func (s *Server) xDisconnect(w http.ResponseWriter, r *http.Request, u *store.User) {
	h, ok := s.accountParam(w, r)
	if !ok {
		return
	}
	revoked, err := s.xm.Disconnect(r.Context(), h)
	if err != nil {
		s.internalError(w, errors.New("x disconnect: could not delete tokens"))
		return
	}
	if !revoked {
		http.Redirect(w, r, accountsDone("norevoke"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, accountsDone("disconnected"), http.StatusSeeOther)
}

func (s *Server) xCheck(w http.ResponseWriter, r *http.Request, u *store.User) {
	h, ok := s.accountParam(w, r)
	if !ok {
		return
	}
	if !s.xc.Cfg.Enabled() {
		s.showAccounts(w, r, u, http.StatusServiceUnavailable, "Connecting is off: this server has no X client id configured.", "")
		return
	}
	me, err := s.xm.Check(r.Context(), h)
	switch {
	case errors.Is(err, xauth.ErrReconnect):
		s.showAccounts(w, r, u, http.StatusConflict, "X refused to refresh @"+h+"'s token. Reconnect it.", "")
	case errors.Is(err, xauth.ErrNotConnected):
		s.showAccounts(w, r, u, http.StatusConflict, "@"+h+" is not connected.", "")
	case err != nil:
		s.log.Printf("x check @%s: %v", h, err)
		s.showAccounts(w, r, u, http.StatusBadGateway, "Could not reach X to check @"+h+" ("+errCode(err)+").", "")
	case !strings.EqualFold(me.Username, h):
		s.showAccounts(w, r, u, http.StatusConflict, "The token for @"+h+" now belongs to @"+me.Username+". Disconnect and connect again.", "")
	default:
		http.Redirect(w, r, accountsDone("checked"), http.StatusSeeOther)
	}
}

func itoaInt(n int) string { return strconv.Itoa(n) }

func contextTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
