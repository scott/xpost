// Package web serves xpost's server-rendered review UI and the agent
// draft-intake API.
//
// Two separate front doors: the browser UI needs a full owner session
// (password + passkey) and a CSRF token on every state change; the /api/v1
// bearer API takes agent tokens only, never reads cookies, and has no route
// that approves, edits or posts.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Cookie names.
const (
	SessionCookie  = "xpost_session"
	CSRFCookie     = "xpost_csrf"
	CeremonyCookie = "xpost_wa"
)

// Config holds server options.
type Config struct {
	BaseURL       string         // public origin, e.g. https://post.example.com
	RPID          string         // WebAuthn relying party ID; default: the BaseURL host
	RPOrigin      string         // expected WebAuthn origin; default: the BaseURL origin
	SecureCookies bool           // Secure cookies (true behind HTTPS)
	TrustProxy    bool           // client IP from X-Forwarded-For when the peer is loopback
	Location      *time.Location // default display zone until the owner picks one
	Demo          bool           // show the "Example data" badge
	// BlogsEnabled turns on the optional blog feature (XPOST_BLOGS_ENABLED):
	// blog routes, Settings > Blogs and its import, the Blog queue kind and
	// agents proposing kind "blog". Off, those routes 404 and the store
	// hides blog rows (store.Store.NoBlogs).
	BlogsEnabled bool
	FailLog      io.Writer     // failed sign-in lines; default stderr
	Log          io.Writer     // app log; default stderr
	IPLimit      int           // failures per IP per window (default 20)
	AccountLimit int           // failures per account per window (default 8)
	LimitWindow  time.Duration // default 15m
	X            *xauth.Config // X OAuth client; nil or no client id disables Connect
	XHTTP        *http.Client  // optional HTTP client for the X endpoints (tests)
	Version      string        // build version reported by /healthz
	// BlogWake, when set, asks the blog publisher to run now; it is set
	// only when a publisher runs (slice 1: the fake one, -fake-poster).
	// Without it Publish now is off.
	BlogWake func()
	// BlogTest, when set, runs a blog's dry-run Test (slice 1: the fake
	// publisher, -fake-poster). Without it Settings > Blogs can't test.
	BlogTest func(ctx context.Context, b *store.Blog) (string, error)
	// BlogImport, when set, reads a blog's live posts for the import (GET
	// only). It is set only when XPOST_BLOG_IMPORT=1; nil, the import is
	// off and makes no call.
	BlogImport func(ctx context.Context, b *store.Blog) ([]store.UpstreamPost, error)
	// ImportAllow is the only check BlogImportHTTP's client passes
	// requests through (the import's GETs). Nil refuses every request.
	ImportAllow   func(*http.Request) bool
	Scheduler     string                                                       // "on" (real X poster), "fake" (local fake poster) or "" (off); display only
	XstatsURL     string                                                       // XPOST_XSTATS_URL: xstats base for "View in xstats" links; "" hides them
	WebhookHTTP   *http.Client                                                 // optional: only its TLS settings are used for rewrite webhooks (tests); dialing, proxy and redirects are always xpost's own
	WebhookLookup func(ctx context.Context, host string) ([]netip.Addr, error) // optional DNS lookup for rewrite webhooks (tests); default net.DefaultResolver
	// Live updates (/live/events, /live/poll). Zero values take the
	// defaults: tail the change feed every 1s, heartbeat every 25s, end a
	// stream after 30 min (the browser reconnects and resumes), at most 4
	// streams per session and 64 in all.
	LivePoll          time.Duration
	LiveHeartbeat     time.Duration
	LiveMaxAge        time.Duration
	LiveMaxPerSession int
	LiveMaxTotal      int
}

// XManager is the server's X token manager, shared with the dispatcher so
// token refreshes for an account are serialised in one place.
func (s *Server) XManager() *xauth.Manager { return s.xm }

// Server is the xpost web app.
type Server struct {
	st      *store.Store
	cfg     Config
	csrf    *auth.CSRF
	ipLim   *auth.Limiter
	acctLim *auth.Limiter
	pages   map[string]*template.Template
	log     *log.Logger
	wa      *webauthn.WebAuthn // nil: passkeys unavailable, so nobody can sign in
	xc      *xauth.Client      // always set; Connect is disabled unless xc.Cfg.Enabled()
	xm      *xauth.Manager
	live    *liveHub
}

// New builds a Server.
func New(st *store.Store, cfg Config) (*Server, error) {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.FailLog == nil {
		cfg.FailLog = os.Stderr
	}
	if cfg.Log == nil {
		cfg.Log = os.Stderr
	}
	if cfg.IPLimit == 0 {
		cfg.IPLimit = 20
	}
	if cfg.AccountLimit == 0 {
		cfg.AccountLimit = 8
	}
	if cfg.LimitWindow == 0 {
		cfg.LimitWindow = 15 * time.Minute
	}
	if cfg.LiveHeartbeat <= 0 {
		cfg.LiveHeartbeat = 25 * time.Second
	}
	if cfg.LiveMaxAge <= 0 {
		cfg.LiveMaxAge = 30 * time.Minute
	}
	if st.Key == nil {
		return nil, errors.New("web: the store needs the master key (XPOST_KEY)")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	var err error
	if cfg.XstatsURL, err = ParseXstatsURL(cfg.XstatsURL); err != nil {
		return nil, err
	}
	wa, err := newWebAuthn(&cfg)
	if err != nil {
		return nil, fmt.Errorf("passkeys: %w", err)
	}
	s := &Server{
		st:      st,
		cfg:     cfg,
		csrf:    auth.NewCSRF(st.Key.Derive("csrf")),
		ipLim:   auth.NewLimiter(cfg.IPLimit, cfg.LimitWindow),
		acctLim: auth.NewLimiter(cfg.AccountLimit, cfg.LimitWindow),
		log:     log.New(cfg.Log, "xpost: ", log.LstdFlags),
		wa:      wa,
	}
	if wa == nil {
		s.log.Printf("passkeys unavailable: base URL %q gives no usable WebAuthn relying party (needs https, or http://localhost); sign-in will not work", cfg.BaseURL)
	}
	xcfg := cfg.X
	if xcfg == nil {
		xcfg = &xauth.Config{}
	}
	if xcfg.Enabled() {
		if xcfg.RedirectURL == "" {
			xcfg.RedirectURL = strings.TrimRight(cfg.BaseURL, "/") + xauth.CallbackPath
		}
		if err := xcfg.Validate(); err != nil {
			return nil, err
		}
	}
	s.xc = xauth.NewClient(xcfg)
	if cfg.XHTTP != nil {
		s.xc.HTTP = cfg.XHTTP
	}
	s.xm = &xauth.Manager{Store: st, Client: s.xc, Log: s.log}
	s.live = newLiveHub(st, s.log, cfg)
	s.ipLim.SetClock(st.Now)
	s.acctLim.SetClock(st.Now)
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	// Off, the store hides blog rows from every page and count too.
	st.NoBlogs = !cfg.BlogsEnabled
	return s, nil
}

// Handler returns the HTTP handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("GET /{$}", s.home)
	s.loginRoutes(mux)
	s.reviewRoutes(mux)
	s.replyRoutes(mux)
	if s.cfg.BlogsEnabled {
		s.blogRoutes(mux)
		s.blogSettingsRoutes(mux)
		s.blogImportRoutes(mux)
	}
	s.pageRoutes(mux)
	s.rewriteRoutes(mux)
	s.composeRoutes(mux)
	s.xRoutes(mux)
	s.liveRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { s.notFound(w, r) })

	// The bearer API never reads cookies, so it sits outside CSRF.
	top := http.NewServeMux()
	top.Handle("/api/", s.apiRoutes())
	top.HandleFunc("GET /healthz", s.healthz) // no session, no CSRF cookie
	top.Handle("/", s.csrfProtect(mux))
	return s.securityHeaders(top)
}

// healthz is the deploy health check: no auth, no cookies, nothing secret.
// 200 with the build version when the database answers, 503 otherwise.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var one int
	if err := s.st.DB.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations LIMIT 1`).Scan(&one); err != nil {
		s.log.Printf("healthz: database check failed")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"ok":false}`)
		return
	}
	v, _ := json.Marshal(s.cfg.Version)
	fmt.Fprintf(w, `{"ok":true,"version":%s}`, v)
}

// ListenAndServe runs the server until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve runs the server on ln until ctx is done, then stops gracefully.
// Live streams end first (RegisterOnShutdown), so open tabs never hold up
// the stop.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second, // the live stream lifts it for itself
		WriteTimeout:      60 * time.Second, // likewise, per write
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	srv.RegisterOnShutdown(s.live.Close)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		s.live.Close()
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// Close ends the live streams (tests and shutdown).
func (s *Server) Close() { s.live.Close() }

// Body limits: forms are small; uploads carry images.
const (
	formBodyLimit   = 64 << 10
	uploadBodyLimit = 4*store.MaxImages*store.MaxMediaBytes + 1<<20 // an edit can add several images
	apiBodyLimit    = store.MaxMediaBytes + 1<<20
)

// CSP is the Content-Security-Policy on every response.
const CSP = "default-src 'none'; style-src 'self'; script-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		limit := int64(formBodyLimit)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			limit = apiBodyLimit
		case strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data"):
			limit = uploadBodyLimit
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// clientIP is the peer address, or the last X-Forwarded-For hop when the
// peer is a trusted local proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.cfg.TrustProxy {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
				parts := strings.Split(xff, ",")
				if cand := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(cand) != nil {
					return cand
				}
			}
		}
	}
	return host
}

func (s *Server) setCookie(w http.ResponseWriter, name, val string, maxAge time.Duration) {
	c := &http.Cookie{Name: name, Value: val, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, SameSite: http.SameSiteLaxMode}
	if maxAge < 0 {
		c.MaxAge = -1
	} else if maxAge > 0 {
		c.MaxAge = int(maxAge / time.Second)
	}
	http.SetCookie(w, c)
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Printf("internal error: %v", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// fail records a failed sign-in for the rate limiters and fail2ban.
func (s *Server) fail(r *http.Request, acctKey, reason string) {
	ip := s.clientIP(r)
	s.ipLim.Fail("ip:" + ip)
	if acctKey != "" {
		s.acctLim.Fail(acctKey)
	}
	auth.WriteFailLine(s.cfg.FailLog, ip, reason)
}

// limited answers 429 if the IP or account has too many recent failures.
func (s *Server) limited(w http.ResponseWriter, r *http.Request, acctKey string, render func(status int, msg string)) bool {
	ip := s.clientIP(r)
	b1, d1 := s.ipLim.Blocked("ip:" + ip)
	b2, d2 := false, time.Duration(0)
	if acctKey != "" {
		b2, d2 = s.acctLim.Blocked(acctKey)
	}
	if !b1 && !b2 {
		return false
	}
	wait := max(d1, d2)
	auth.WriteFailLine(s.cfg.FailLog, ip, "rate_limited")
	w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
	render(http.StatusTooManyRequests, fmt.Sprintf("Too many tries. Wait %d minutes and try again.", int(wait.Minutes())+1))
	return true
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if _, _, err := s.fullSession(r); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/queue", http.StatusSeeOther)
}
