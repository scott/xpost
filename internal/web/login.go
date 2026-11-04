package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/store"
)

// Sign-in is two steps, both required: the password, then a passkey
// (user verification required). Only after both is the session "full".
// The first passkey is registered through a one-time setup link printed by
// `xpost create-owner` or `xpost setup-link`.

func (s *Server) loginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("GET /login/passkey", s.loginPasskeyPage)
	mux.HandleFunc("POST /login/passkey/begin", s.loginPasskeyBegin)
	mux.HandleFunc("POST /login/passkey/finish", s.loginPasskeyFinish)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /setup/passkey", s.setupPasskeyPage)
	mux.HandleFunc("GET /setup/{token}", s.setupPage)
	mux.HandleFunc("POST /setup/{token}", s.setupSubmit)
	mux.HandleFunc("POST /passkeys/begin", s.registerBegin)
	mux.HandleFunc("POST /passkeys/finish", s.registerFinish)
	mux.HandleFunc("GET /security", s.requireOwner(s.securityPage))
	mux.HandleFunc("POST /security/passkeys/{id}/remove", s.requireOwner(s.passkeyRemove))
}

// RelyingParty derives the WebAuthn RP ID and origin from a base URL. An IP
// address cannot be an RP ID, so it gives "".
func RelyingParty(baseURL string) (id, origin string) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || net.ParseIP(u.Hostname()) != nil {
		return "", ""
	}
	return strings.ToLower(u.Hostname()), u.Scheme + "://" + strings.ToLower(u.Host)
}

func newWebAuthn(cfg *Config) (*webauthn.WebAuthn, error) {
	explicit := cfg.RPID != "" || cfg.RPOrigin != ""
	id, origin := RelyingParty(cfg.BaseURL)
	if cfg.RPID == "" {
		cfg.RPID = id
	}
	if cfg.RPOrigin == "" {
		cfg.RPOrigin = origin
	}
	cfg.RPID = strings.ToLower(cfg.RPID)
	cfg.RPOrigin = strings.TrimRight(cfg.RPOrigin, "/")
	if cfg.RPID == "" || cfg.RPOrigin == "" {
		return nil, nil
	}
	ou, err := url.Parse(cfg.RPOrigin)
	okScheme := err == nil && (ou.Scheme == "https" || (ou.Scheme == "http" && ou.Hostname() == "localhost"))
	okHost := err == nil && (strings.ToLower(ou.Hostname()) == cfg.RPID || strings.HasSuffix(strings.ToLower(ou.Hostname()), "."+cfg.RPID))
	if !okScheme || !okHost {
		if !explicit {
			cfg.RPID, cfg.RPOrigin = "", ""
			return nil, nil
		}
		return nil, fmt.Errorf("origin %q must be https (or http on localhost) on the RP ID %q", cfg.RPOrigin, cfg.RPID)
	}
	return webauthn.New(&webauthn.Config{
		RPID:                  cfg.RPID,
		RPDisplayName:         "xpost",
		RPOrigins:             []string{cfg.RPOrigin},
		AttestationPreference: protocol.PreferNoAttestation,
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: store.ChallengeTTL, TimeoutUVD: store.ChallengeTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: store.ChallengeTTL, TimeoutUVD: store.ChallengeTTL},
		},
	})
}

// waUser adapts the owner and their passkeys to webauthn.User.
type waUser struct {
	u      *store.User
	handle []byte
	keys   []*store.Passkey
	creds  []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return w.handle }
func (w *waUser) WebAuthnName() string                       { return w.u.Email }
func (w *waUser) WebAuthnDisplayName() string                { return w.u.DisplayName }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Server) loadWAUser(ctx context.Context, u *store.User) (*waUser, error) {
	h, err := s.st.WebAuthnHandle(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	keys, err := s.st.PasskeysFor(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u, handle: h, keys: keys}
	for _, k := range keys {
		var tr []protocol.AuthenticatorTransport
		for _, t := range k.Transports {
			tr = append(tr, protocol.AuthenticatorTransport(t))
		}
		w.creds = append(w.creds, webauthn.Credential{
			ID: k.CredID, PublicKey: k.PublicKey, AttestationType: k.AttestationType, AttestationFormat: k.AttestationFormat, Transport: tr,
			Flags:         webauthn.CredentialFlags{UserPresent: true, UserVerified: true, BackupEligible: k.BackupEligible, BackupState: k.BackupState},
			Authenticator: webauthn.Authenticator{AAGUID: k.AAGUID, SignCount: k.SignCount},
		})
	}
	return w, nil
}

func (w *waUser) key(credID []byte) *store.Passkey {
	for _, k := range w.keys {
		if bytes.Equal(k.CredID, credID) {
			return k
		}
	}
	return nil
}

func (w *waUser) descriptors() []protocol.CredentialDescriptor {
	var out []protocol.CredentialDescriptor
	for _, c := range w.creds {
		out = append(out, c.Descriptor())
	}
	return out
}

type jsonErr struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	enc.Encode(v)
}

func (s *Server) passkeysOff(w http.ResponseWriter) bool {
	if s.wa == nil {
		writeJSON(w, http.StatusNotFound, jsonErr{Error: "Passkeys are not available on this server address."})
		return true
	}
	return false
}

// startCeremony stores the challenge under a fresh cookie token, bound to
// the current session.
func (s *Server) startCeremony(w http.ResponseWriter, r *http.Request, purpose string, sd *webauthn.SessionData) error {
	tok, err := auth.NewToken()
	if err != nil {
		return err
	}
	b, err := json.Marshal(sd)
	if err != nil {
		return err
	}
	if err := s.st.SaveChallenge(r.Context(), tok, purpose, sessionToken(r), string(b)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Value: tok, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: int(store.ChallengeTTL.Seconds())})
	return nil
}

func (s *Server) takeCeremony(w http.ResponseWriter, r *http.Request, purpose string) (*webauthn.SessionData, error) {
	tok := ""
	if c, err := r.Cookie(CeremonyCookie); err == nil {
		tok = c.Value
	}
	http.SetCookie(w, &http.Cookie{Name: CeremonyCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	ch, err := s.st.TakeChallenge(r.Context(), tok, purpose, sessionToken(r))
	if err != nil {
		return nil, err
	}
	var sd webauthn.SessionData
	if err := json.Unmarshal([]byte(ch.Data), &sd); err != nil {
		return nil, store.ErrChallenge
	}
	return &sd, nil
}

// protocolReason is the library's short reason (DevInfo can carry client
// data, so only the type and details are logged).
func protocolReason(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe.Type + ": " + pe.Details
	}
	if err == nil {
		return "no credential"
	}
	return err.Error()
}

// ---- Password step ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.fullSession(r); err == nil {
		http.Redirect(w, r, "/queue", http.StatusSeeOther)
		return
	}
	s.showLogin(w, r, http.StatusOK, "", "")
}

func (s *Server) showLogin(w http.ResponseWriter, r *http.Request, status int, email, msg string) {
	p := s.newPage(r, nil)
	p.Title = "Sign in"
	p.Error = msg
	p.Data = map[string]any{"Email": email, "Passkeys": s.wa != nil}
	s.render(w, status, "login", "auth", p)
}

const loginError = "That email and password didn't match."

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	pw := r.PostFormValue("password")
	acct := "email:" + email
	show := func(status int, msg string) { s.showLogin(w, r, status, email, msg) }
	if s.limited(w, r, acct, show) {
		return
	}
	ctx := r.Context()
	u, err := s.st.UserByEmail(ctx, email)
	if err != nil || u.PasswordHash == "" {
		auth.BurnPasswordTime(pw)
		s.fail(r, acct, "bad_password")
		show(http.StatusUnauthorized, loginError)
		return
	}
	if auth.VerifyPassword(u.PasswordHash, pw) != nil {
		s.fail(r, acct, "bad_password")
		show(http.StatusUnauthorized, loginError)
		return
	}
	if s.wa == nil || s.st.PasskeyCount(ctx, u.ID) == 0 {
		// A password alone never signs in, and never lets anyone add the
		// first passkey: that needs a setup link from the command line.
		show(http.StatusForbidden, "Your passkey isn't set up yet. Run `xpost setup-link` on the server and open the link it prints.")
		return
	}
	if old := sessionToken(r); old != "" {
		s.st.DeleteSession(ctx, old)
	}
	tok, err := s.st.CreateSession(ctx, u.ID, store.StagePassword, r.UserAgent())
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.setCookie(w, SessionCookie, tok, 0)
	http.Redirect(w, r, "/login/passkey", http.StatusSeeOther)
}

// ---- Passkey step ----

func (s *Server) loginPasskeyPage(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.stepSession(r, store.StagePassword); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p := s.newPage(r, nil)
	p.Title = "Confirm with your passkey"
	s.render(w, http.StatusOK, "login_passkey", "auth", p)
}

const passkeyError = "That passkey didn't work. Try again, or start over with your password."

func (s *Server) loginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	_, u, err := s.stepSession(r, store.StagePassword)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, jsonErr{Error: "Start again with your password."})
		return
	}
	acct := "email:" + strings.ToLower(u.Email)
	if s.limited(w, r, acct, func(st int, m string) { writeJSON(w, st, jsonErr{Error: m}) }) {
		return
	}
	wu, err := s.loadWAUser(r.Context(), u)
	if err != nil {
		s.internalError(w, err)
		return
	}
	opts, sd, err := s.wa.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.startCeremony(w, r, store.PurposeLogin, sd); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) loginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	ctx := r.Context()
	_, u, err := s.stepSession(r, store.StagePassword)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, jsonErr{Error: "Start again with your password."})
		return
	}
	acct := "email:" + strings.ToLower(u.Email)
	if s.limited(w, r, acct, func(st int, m string) { writeJSON(w, st, jsonErr{Error: m}) }) {
		return
	}
	sd, err := s.takeCeremony(w, r, store.PurposeLogin)
	if err != nil {
		s.fail(r, acct, "bad_passkey_challenge")
		writeJSON(w, http.StatusUnauthorized, jsonErr{Error: store.ErrChallenge.Error()})
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(r.PostFormValue("credential")))
	var cred *webauthn.Credential
	var wu *waUser
	if err == nil {
		wu, err = s.loadWAUser(ctx, u)
		if err == nil {
			cred, err = s.wa.ValidateLogin(wu, *sd, parsed)
		}
	}
	if err != nil || cred == nil || wu.key(cred.ID) == nil {
		s.log.Printf("passkey sign-in rejected: %v", protocolReason(err))
		s.fail(r, acct, "bad_passkey")
		writeJSON(w, http.StatusUnauthorized, jsonErr{Error: passkeyError})
		return
	}
	k := wu.key(cred.ID)
	if cred.Authenticator.CloneWarning {
		s.log.Printf("warning: passkey %d sign count did not increase; possible cloned authenticator", k.ID)
	}
	if err := s.st.RecordPasskeyUse(ctx, k.ID, cred.Authenticator.SignCount, cred.Flags.BackupState); err != nil {
		s.log.Printf("passkey use not recorded: %v", err)
	}
	tok, err := s.st.PromoteSession(ctx, sessionToken(r), u.ID, r.UserAgent())
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.acctLim.Reset(acct)
	s.setCookie(w, SessionCookie, tok, 0)
	writeJSON(w, http.StatusOK, map[string]string{"next": "/queue"})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if tok := sessionToken(r); tok != "" {
		s.st.DeleteSession(r.Context(), tok)
	}
	s.setCookie(w, SessionCookie, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- Setup link: password, then first passkey ----

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	u, err := s.st.SetupLinkUser(r.Context(), r.PathValue("token"))
	if err != nil {
		s.notice(w, r, http.StatusNotFound, "Link not valid", "This setup link is invalid, used or expired. Run `xpost setup-link` on the server for a new one.")
		return
	}
	s.showSetup(w, r, http.StatusOK, u, "")
}

func (s *Server) showSetup(w http.ResponseWriter, r *http.Request, status int, u *store.User, msg string) {
	p := s.newPage(r, nil)
	p.Title = "Set up xpost"
	p.Error = msg
	p.Data = map[string]any{"Email": u.Email, "Token": r.PathValue("token"), "Min": auth.MinPasswordLen, "Passkeys": s.wa != nil}
	s.render(w, status, "setup", "auth", p)
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok := r.PathValue("token")
	u, err := s.st.SetupLinkUser(ctx, tok)
	if err != nil {
		s.fail(r, "", "bad_setup_link")
		s.notice(w, r, http.StatusNotFound, "Link not valid", "This setup link is invalid, used or expired. Run `xpost setup-link` on the server for a new one.")
		return
	}
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("confirm") {
		s.showSetup(w, r, http.StatusBadRequest, u, "The two passwords don't match.")
		return
	}
	stok, err := s.st.UseSetupLink(ctx, tok, pw, r.UserAgent())
	if err != nil {
		var ie *store.InputError
		if errors.As(err, &ie) {
			s.showSetup(w, r, http.StatusBadRequest, u, ie.Msg)
			return
		}
		if errors.Is(err, store.ErrLinkInvalid) {
			s.notice(w, r, http.StatusNotFound, "Link not valid", err.Error())
			return
		}
		s.internalError(w, err)
		return
	}
	s.setCookie(w, SessionCookie, stok, 0)
	http.Redirect(w, r, "/setup/passkey", http.StatusSeeOther)
}

func (s *Server) setupPasskeyPage(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.stepSession(r, store.StageEnroll); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p := s.newPage(r, nil)
	p.Title = "Add your passkey"
	p.Data = map[string]any{"Passkeys": s.wa != nil}
	s.render(w, http.StatusOK, "setup_passkey", "auth", p)
}

// registerSession allows passkey registration from an enroll session (the
// setup link) or a full session that signed in within FreshSignInTTL.
func (s *Server) registerSession(r *http.Request) (*store.Session, *store.User, bool) {
	ss, u, err := s.st.LookupSession(r.Context(), sessionToken(r))
	if err != nil {
		return nil, nil, false
	}
	if ss.Stage == store.StageEnroll || ss.Fresh(s.st.Now()) {
		return ss, u, true
	}
	return nil, nil, false
}

func (s *Server) registerBegin(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	_, u, ok := s.registerSession(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, jsonErr{Error: "Sign out and sign in again, then add the passkey within 10 minutes."})
		return
	}
	ctx := r.Context()
	if s.st.PasskeyCount(ctx, u.ID) >= store.MaxPasskeys {
		writeJSON(w, http.StatusBadRequest, jsonErr{Error: fmt.Sprintf("You already have %d passkeys; remove one first.", store.MaxPasskeys)})
		return
	}
	wu, err := s.loadWAUser(ctx, u)
	if err != nil {
		s.internalError(w, err)
		return
	}
	opts, sd, err := s.wa.BeginRegistration(wu,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationRequired}),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(wu.descriptors()))
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.startCeremony(w, r, store.PurposeRegister, sd); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (s *Server) registerFinish(w http.ResponseWriter, r *http.Request) {
	if s.passkeysOff(w) {
		return
	}
	ss, u, ok := s.registerSession(r)
	if !ok {
		writeJSON(w, http.StatusForbidden, jsonErr{Error: "Sign out and sign in again, then add the passkey within 10 minutes."})
		return
	}
	ctx := r.Context()
	sd, err := s.takeCeremony(w, r, store.PurposeRegister)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonErr{Error: store.ErrChallenge.Error()})
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes([]byte(r.PostFormValue("credential")))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonErr{Error: "The browser sent a passkey xpost could not read."})
		return
	}
	wu, err := s.loadWAUser(ctx, u)
	if err != nil {
		s.internalError(w, err)
		return
	}
	cred, err := s.wa.CreateCredential(wu, *sd, parsed)
	if err != nil || !cred.Flags.UserVerified {
		s.log.Printf("passkey registration rejected: %v", protocolReason(err))
		writeJSON(w, http.StatusBadRequest, jsonErr{Error: "That passkey could not be verified. Try again."})
		return
	}
	k := &store.Passkey{CredID: cred.ID, PublicKey: cred.PublicKey, SignCount: cred.Authenticator.SignCount, AAGUID: cred.Authenticator.AAGUID,
		AttestationType: cred.AttestationType, AttestationFormat: cred.AttestationFormat, BackupEligible: cred.Flags.BackupEligible,
		BackupState: cred.Flags.BackupState, Name: r.PostFormValue("name")}
	for _, t := range cred.Transport {
		k.Transports = append(k.Transports, string(t))
	}
	if _, err := s.st.AddPasskey(ctx, u.ID, k); err != nil {
		var ie *store.InputError
		if errors.As(err, &ie) {
			writeJSON(w, http.StatusBadRequest, jsonErr{Error: ie.Msg})
			return
		}
		s.internalError(w, err)
		return
	}
	if ss.Stage == store.StageEnroll {
		// Password (set through the link) plus this passkey: a full session,
		// under a new token.
		tok, err := s.st.PromoteSession(ctx, sessionToken(r), u.ID, r.UserAgent())
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.setCookie(w, SessionCookie, tok, 0)
		writeJSON(w, http.StatusOK, map[string]string{"next": "/queue"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"next": "/security?done=added"})
}

// ---- Security & passkeys page ----

var securityFlash = map[string]string{"added": "Passkey added.", "removed": "Passkey removed."}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.showSecurity(w, r, u, http.StatusOK, "", securityFlash[r.URL.Query().Get("done")])
}

func (s *Server) showSecurity(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash string) {
	ctx := r.Context()
	keys, err := s.st.PasskeysFor(ctx, u.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	ss, _, _ := s.fullSession(r)
	p := s.newPage(r, u)
	p.Nav, p.Title = "security", "Security & passkeys"
	p.Error, p.Flash = errMsg, flash
	p.Data = map[string]any{"Keys": keys, "Fresh": ss != nil && ss.Fresh(s.st.Now()), "Max": store.MaxPasskeys, "Full": len(keys) >= store.MaxPasskeys, "Only": len(keys) <= 1}
	s.render(w, status, "security", "app", p)
}

func (s *Server) passkeyRemove(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ss, _, _ := s.fullSession(r)
	if ss == nil || !ss.Fresh(s.st.Now()) {
		s.showSecurity(w, r, u, http.StatusForbidden, "Sign out and sign in again, then remove the passkey within 10 minutes.", "")
		return
	}
	switch err := s.st.RemovePasskey(r.Context(), u.ID, id); {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
	case errors.Is(err, store.ErrLastPasskey):
		s.showSecurity(w, r, u, http.StatusConflict, "This is your only passkey. Add another before removing it.", "")
	case err != nil:
		s.internalError(w, err)
	default:
		http.Redirect(w, r, "/security?done=removed", http.StatusSeeOther)
	}
}
