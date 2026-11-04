// Package xfake is a local stand-in for X's OAuth 2.0 endpoints, users/me,
// create-post and media upload (upload, processing status, alt text), for
// tests and local demos. It never contacts X. Tokens
// it mints are random strings prefixed "fake-", tied to nothing real, and
// post ids are invented. Faults (429s, refusals, dropped or slow replies)
// can be queued per create-post call.
package xfake

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Server is the fake provider. Mount Handler on an httptest.Server (or a
// local listener) and point the XPOST_X_*_URL settings at it.
type Server struct {
	ClientID     string
	ClientSecret string            // checked with HTTP Basic when set
	Users        map[string]string // username -> user id
	AccessTTL    time.Duration     // default 2h, like X
	Now          func() time.Time  // default time.Now
	AutoApprove  string            // username to approve without the consent page (tests); Deny refuses
	MediaTTL     time.Duration     // upload lifetime, default 24h

	mu       sync.Mutex
	codes    map[string]codeGrant
	access   map[string]grant // live access tokens
	refresh  map[string]grant // live refresh tokens (single use)
	failNext map[string]int   // username -> refreshes to refuse with invalid_grant
	posts    []Post
	byID     map[string]*Post
	faults   []Fault
	nextID   int64

	media       map[string]*Media
	mediaOrder  []string
	mediaFaults map[string][]MediaFault
	nextMedia   int64

	// Counters and records for tests.
	TokenCalls, RefreshCalls, RevokeCalls, MeCalls, CreateCalls int
	UploadCalls, StatusCalls, MetadataCalls                     int
	Revoked                                                     []string
	LastAuthorize                                               url.Values
}

// Deny as AutoApprove makes the fake answer access_denied.
const Deny = "-deny-"

type codeGrant struct {
	challenge, method, redirect, username, scope string
	exp                                          time.Time
}

type grant struct {
	username, scope string
	exp             time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) init() {
	if s.codes == nil {
		s.codes, s.access, s.refresh, s.failNext = map[string]codeGrant{}, map[string]grant{}, map[string]grant{}, map[string]int{}
		s.byID, s.nextID = map[string]*Post{}, 1990000000000000001
		s.media, s.mediaFaults, s.nextMedia = map[string]*Media{}, map[string][]MediaFault{}, 1880000000000000001
	}
	if s.AccessTTL == 0 {
		s.AccessTTL = 2 * time.Hour
	}
}

func token(prefix string) string {
	b := make([]byte, 18)
	rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// Handler serves /i/oauth2/authorize, /2/oauth2/token, /2/oauth2/revoke and
// /2/users/me, plus /_fake/expire and /_fake/fail-refresh for demos.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /i/oauth2/authorize", s.authorize)
	mux.HandleFunc("POST /i/oauth2/authorize", s.authorize)
	mux.HandleFunc("POST /2/oauth2/token", s.token)
	mux.HandleFunc("POST /2/oauth2/revoke", s.revoke)
	mux.HandleFunc("GET /2/users/me", s.me)
	mux.HandleFunc("POST /2/tweets", s.createPost)
	mux.HandleFunc("POST /2/media/upload", s.upload)
	mux.HandleFunc("GET /2/media/upload", s.status)
	mux.HandleFunc("POST /2/media/metadata", s.metadata)
	mux.HandleFunc("GET /_fake/media", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"media": s.AllMedia()})
	})
	mux.HandleFunc("GET /_fake/posts", s.listPosts)
	mux.HandleFunc("POST /_fake/fault", s.faultForm)
	mux.HandleFunc("POST /_fake/expire", func(w http.ResponseWriter, r *http.Request) { s.ExpireAccess(r.FormValue("username")) })
	mux.HandleFunc("POST /_fake/fail-refresh", func(w http.ResponseWriter, r *http.Request) { s.FailRefresh(r.FormValue("username"), 1000) })
	return mux
}

// Post is a post the fake created.
type Post struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Text     string    `json:"text"`
	ReplyTo  string    `json:"reply_to,omitempty"`
	Media    []string  `json:"media_ids,omitempty"`
	Alts     []string  `json:"alts,omitempty"` // alt text of each image when posted
	At       time.Time `json:"at"`
}

// FaultKind is what the fake does with one create-post call.
type FaultKind int

const (
	// Status answers with Fault.Status and an error body; nothing is created.
	Status FaultKind = iota + 1
	// RateLimit answers 429 with x-rate-limit-* headers (reset = Fault.Reset).
	RateLimit
	// DropAfterCreate creates the post, then closes the connection without
	// replying: the caller cannot know it posted.
	DropAfterCreate
	// DropBeforeCreate closes the connection without creating anything.
	DropBeforeCreate
	// Hang creates the post (if Fault.Create) and then waits for Fault.Release
	// to close, or for the client to give up, before replying.
	Hang
	// Garbled creates the post and answers 201 with an unreadable body.
	Garbled
)

// Fault is a queued behaviour for the next create-post call.
type Fault struct {
	Kind    FaultKind
	Status  int
	Reset   time.Time
	Create  bool          // Hang: create the post before hanging
	Release chan struct{} // Hang: close to let the reply go out
	Arrived chan struct{} // Hang: closed when the request has arrived (optional)
}

// Queue appends faults; each create-post call takes the next one (or
// succeeds normally when none are left). Use Fault{} for a normal success
// in the middle of a sequence.
func (s *Server) Queue(f ...Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.faults = append(s.faults, f...)
}

// Posts returns the posts created so far, oldest first.
func (s *Server) Posts() []Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Post(nil), s.posts...)
}

func xProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"title": title, "detail": detail, "status": status, "type": "about:blank"})
}

func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// create stores a post; the caller holds s.mu.
func (s *Server) create(username, text, replyTo string, media ...string) *Post {
	p := &Post{ID: fmt.Sprint(s.nextID), Username: username, Text: text, ReplyTo: replyTo, Media: media, At: s.now()}
	for _, id := range media {
		p.Alts = append(p.Alts, s.media[id].Alt)
	}
	s.nextID++
	s.posts = append(s.posts, *p)
	s.byID[p.ID] = p
	return p
}

// createPost is POST /2/tweets: JSON {text, reply: {in_reply_to_tweet_id}}.
// It requires a live access token with tweet.write. Replies must target a
// post by the same user (xpost only threads its own posts), and duplicate
// text from the same user is refused, as X does.
func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	s.CreateCalls++
	at, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.access[at]
	if !ok || !s.now().Before(g.exp) {
		s.mu.Unlock()
		xProblem(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return
	}
	if !strings.Contains(" "+g.scope+" ", " tweet.write ") {
		s.mu.Unlock()
		xProblem(w, http.StatusForbidden, "Forbidden", "missing tweet.write scope")
		return
	}
	var body struct {
		Text  string `json:"text"`
		Reply *struct {
			InReplyTo string `json:"in_reply_to_tweet_id"`
		} `json:"reply"`
		Media *struct {
			MediaIDs []string `json:"media_ids"`
		} `json:"media"`
	}
	if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil || (strings.TrimSpace(body.Text) == "" && body.Media == nil) {
		s.mu.Unlock()
		xProblem(w, http.StatusBadRequest, "Invalid Request", "One or more parameters to your request was invalid.")
		return
	}
	var f Fault
	if len(s.faults) > 0 {
		f, s.faults = s.faults[0], s.faults[1:]
	}
	replyTo := ""
	if body.Reply != nil {
		replyTo = body.Reply.InReplyTo
		parent, ok := s.byID[replyTo]
		if !ok {
			s.mu.Unlock()
			xProblem(w, http.StatusBadRequest, "Invalid Request", "The reply target does not exist.")
			return
		}
		if parent.Username != g.username {
			s.mu.Unlock()
			xProblem(w, http.StatusForbidden, "Forbidden", "fake x: xpost may only reply to its own posts")
			return
		}
	}
	var media []string
	if body.Media != nil {
		media = body.Media.MediaIDs
		if msg := s.checkPostMedia(g.username, media); msg != "" {
			s.mu.Unlock()
			xProblem(w, http.StatusBadRequest, "Invalid Request", msg)
			return
		}
	}
	for _, p := range s.posts {
		if p.Username == g.username && p.Text == body.Text && len(media) == 0 && len(p.Media) == 0 && f.Kind != Status && f.Kind != RateLimit && f.Kind != DropBeforeCreate {
			s.mu.Unlock()
			xProblem(w, http.StatusForbidden, "Forbidden", "You are not allowed to create a Tweet with duplicate content.")
			return
		}
	}
	switch f.Kind {
	case Status:
		s.mu.Unlock()
		xProblem(w, f.Status, http.StatusText(f.Status), "fake x: injected failure")
		return
	case RateLimit:
		s.mu.Unlock()
		w.Header().Set("x-rate-limit-limit", "100")
		w.Header().Set("x-rate-limit-remaining", "0")
		w.Header().Set("x-rate-limit-reset", fmt.Sprint(f.Reset.Unix()))
		xProblem(w, http.StatusTooManyRequests, "Too Many Requests", "Too Many Requests")
		return
	case DropBeforeCreate:
		s.mu.Unlock()
		hijackClose(w)
		return
	case DropAfterCreate:
		s.create(g.username, body.Text, replyTo, media...)
		s.mu.Unlock()
		hijackClose(w)
		return
	case Hang:
		var p *Post
		if f.Create {
			p = s.create(g.username, body.Text, replyTo, media...)
		}
		s.mu.Unlock()
		if f.Arrived != nil {
			close(f.Arrived)
		}
		select {
		case <-f.Release:
		case <-r.Context().Done():
			return
		}
		if p == nil {
			s.mu.Lock()
			p = s.create(g.username, body.Text, replyTo, media...)
			s.mu.Unlock()
		}
		writePost(w, p)
		return
	case Garbled:
		s.create(g.username, body.Text, replyTo, media...)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"data":`)
		return
	}
	p := s.create(g.username, body.Text, replyTo, media...)
	s.mu.Unlock()
	writePost(w, p)
}

func writePost(w http.ResponseWriter, p *Post) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": p.ID, "text": p.Text, "edit_history_tweet_ids": []string{p.ID}}})
}

func (s *Server) listPosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"posts": s.Posts()})
}

// faultForm queues a fault for demos: kind=status&status=503,
// kind=ratelimit&seconds=60, kind=drop-after, kind=drop-before,
// kind=garbled; target=post (default), upload, metadata or status; for
// uploads also kind=process&checks=2 and kind=process-fail.
func (s *Server) faultForm(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	target := r.PostForm.Get("target")
	switch target {
	case "", "post", UploadCall, MetadataCall, StatusCall:
	default:
		http.Error(w, "unknown target", http.StatusBadRequest)
		return
	}
	var f Fault
	var mf MediaFault
	switch r.PostForm.Get("kind") {
	case "status":
		fmt.Sscan(r.PostForm.Get("status"), &f.Status)
		if f.Status < 400 || f.Status > 599 {
			http.Error(w, "status must be 4xx or 5xx", http.StatusBadRequest)
			return
		}
		f.Kind = Status
	case "ratelimit":
		var sec int
		fmt.Sscan(r.PostForm.Get("seconds"), &sec)
		f.Kind, f.Reset = RateLimit, s.now().Add(time.Duration(sec)*time.Second)
	case "drop-after":
		f.Kind = DropAfterCreate
	case "drop-before":
		f.Kind = DropBeforeCreate
	case "garbled":
		f.Kind = Garbled
	case "process", "process-fail":
		if target != UploadCall {
			http.Error(w, "process faults are for target=upload", http.StatusBadRequest)
			return
		}
		mf.Process, mf.FailProcess = true, r.PostForm.Get("kind") == "process-fail"
		fmt.Sscan(r.PostForm.Get("checks"), &mf.Checks)
	default:
		http.Error(w, "unknown kind", http.StatusBadRequest)
		return
	}
	if target == "" || target == "post" {
		s.Queue(f)
		return
	}
	mf.Fault = f
	s.QueueMedia(target, mf)
}

// ExpireAccess invalidates every live access token for username, so the
// next users/me call gets a 401.
func (s *Server) ExpireAccess(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	for k, g := range s.access {
		if g.username == username {
			delete(s.access, k)
		}
	}
}

// FailRefresh makes the next n refreshes for username fail with invalid_grant.
func (s *Server) FailRefresh(username string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.failNext[username] = n
}

// LiveAccess reports whether an access token is currently valid.
func (s *Server) LiveAccess(tok string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.access[tok]
	return ok && s.now().Before(g.exp)
}

var consent = template.Must(template.New("c").Parse(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Fake X: authorize</title></head>
<body><h1>Fake X (local test provider)</h1><p>Authorize <b>{{.Client}}</b> for: {{.Scope}}</p>
<form method="post">{{range $k, $v := .Q}}<input type="hidden" name="{{$k}}" value="{{index $v 0}}">{{end}}
{{range .Users}}<button name="as" value="{{.}}">Authorize as @{{.}}</button> {{end}}<button name="as" value="">Cancel</button></form></body></html>`))

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := url.Values{}
	for _, k := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method"} {
		q.Set(k, r.Form.Get(k))
	}
	s.mu.Lock()
	s.init()
	s.LastAuthorize = q
	s.mu.Unlock()
	if q.Get("response_type") != "code" || q.Get("client_id") != s.ClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("state") == "" {
		http.Error(w, "fake x: bad authorize request", http.StatusBadRequest)
		return
	}
	as := s.AutoApprove
	if as == Deny {
		as = ""
	} else if as == "" {
		if r.Method == "GET" {
			var users []string
			for u := range s.Users {
				users = append(users, u)
			}
			sortStrings(users)
			consent.Execute(w, map[string]any{"Client": s.ClientID, "Scope": q.Get("scope"), "Q": q, "Users": users})
			return
		}
		as = r.PostForm.Get("as")
	}
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "fake x: bad redirect_uri", http.StatusBadRequest)
		return
	}
	rq := back.Query()
	rq.Set("state", q.Get("state"))
	if _, ok := s.Users[as]; !ok {
		rq.Set("error", "access_denied")
	} else {
		code := token("fake-code-")
		s.mu.Lock()
		s.codes[code] = codeGrant{challenge: q.Get("code_challenge"), method: "S256", redirect: q.Get("redirect_uri"), username: as, scope: q.Get("scope"), exp: s.now().Add(30 * time.Second)}
		s.mu.Unlock()
		rq.Set("code", code)
	}
	back.RawQuery = rq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func oauthErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": "fake x: " + code})
}

func (s *Server) clientOK(r *http.Request) bool {
	if s.ClientSecret == "" {
		return r.PostForm.Get("client_id") == s.ClientID
	}
	id, sec, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	sec, _ = url.QueryUnescape(sec)
	return ok && id == s.ClientID && subtle.ConstantTimeCompare([]byte(sec), []byte(s.ClientSecret)) == 1
}

func (s *Server) issue(w http.ResponseWriter, username, scope string) {
	at, rt := token("fake-at-"), token("fake-rt-")
	s.access[at] = grant{username: username, scope: scope, exp: s.now().Add(s.AccessTTL)}
	if strings.Contains(" "+scope+" ", " offline.access ") {
		s.refresh[rt] = grant{username: username, scope: scope}
	} else {
		rt = ""
	}
	out := map[string]any{"token_type": "bearer", "access_token": at, "expires_in": int(s.AccessTTL / time.Second), "scope": scope}
	if rt != "" {
		out["refresh_token"] = rt
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if !s.clientOK(r) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.TokenCalls++
		code := r.PostForm.Get("code")
		g, ok := s.codes[code]
		delete(s.codes, code) // single use
		if !ok || s.now().After(g.exp) || r.PostForm.Get("redirect_uri") != g.redirect {
			oauthErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		h := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(h[:]) != g.challenge {
			oauthErr(w, http.StatusBadRequest, "invalid_grant") // PKCE mismatch
			return
		}
		s.issue(w, g.username, g.scope)
	case "refresh_token":
		s.RefreshCalls++
		rt := r.PostForm.Get("refresh_token")
		g, ok := s.refresh[rt]
		delete(s.refresh, rt) // rotation: a refresh token works once
		if !ok {
			oauthErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		if s.failNext[g.username] > 0 {
			s.failNext[g.username]--
			oauthErr(w, http.StatusBadRequest, "invalid_grant")
			return
		}
		s.issue(w, g.username, g.scope)
	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	if !s.clientOK(r) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	s.RevokeCalls++
	t := r.PostForm.Get("token")
	if _, ok := s.access[t]; ok {
		delete(s.access, t)
		s.Revoked = append(s.Revoked, "access")
	}
	if _, ok := s.refresh[t]; ok {
		delete(s.refresh, t)
		s.Revoked = append(s.Revoked, "refresh")
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"revoked":true}`)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.MeCalls++
	at, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.access[at]
	if !ok || !s.now().Before(g.exp) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"title":"Unauthorized","status":401}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"id": s.Users[g.username], "username": g.username, "name": "Example " + g.username}})
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
