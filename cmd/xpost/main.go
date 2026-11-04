// Command xpost is the X post scheduler: a single binary that serves the
// review UI and the agent draft API, and runs the admin commands. There is
// no public sign-up: the owner is created here and finishes setup (password
// and passkey) through a one-time link.
//
// Posting is off unless XPOST_SCHEDULER=on: then the dispatcher posts
// approved, due drafts to X through the connected accounts. The fake poster
// (-fake-poster) logs what it would post and runs only on a loopback base
// URL. Both refuse to post until the owner sets a monthly spend cap and the
// cost rates are configured.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	// Zone data in the binary: minimal servers and containers often ship
	// no /usr/share/zoneinfo, and XPOST_TZ or the owner's zone must still load.
	_ "time/tzdata"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/gander"
	"github.com/scott/xpost/internal/blog/hookpub"
	"github.com/scott/xpost/internal/db"
	"github.com/scott/xpost/internal/demo"
	"github.com/scott/xpost/internal/poster"
	"github.com/scott/xpost/internal/secret"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/sweep"
	"github.com/scott/xpost/internal/web"
	"github.com/scott/xpost/internal/xauth"
)

var version = "dev"

const usage = `usage: xpost <command> [flags]

commands:
  serve          run the web app and agent API
  create-owner   create the owner; prints a one-time setup link (password + passkey)
  setup-link     print a new one-time setup link for the owner (resets the password)
  seed-demo      add invented example data (never on a real install)
  backup         write a consistent, integrity-checked copy of the DB: backup -out FILE
  keygen         print a new random master key for XPOST_KEY
  version        print the version

common flags: -db (env XPOST_DB, default xpost.db), -base-url (env XPOST_BASE_URL)
The master key comes from XPOST_KEY or XPOST_KEY_FILE.
X connect (serve): XPOST_X_CLIENT_ID, XPOST_X_CLIENT_SECRET or XPOST_X_CLIENT_SECRET_FILE;
endpoint overrides XPOST_X_AUTHORIZE_URL, XPOST_X_TOKEN_URL, XPOST_X_REVOKE_URL, XPOST_X_ME_URL,
XPOST_X_POST_URL, XPOST_X_MEDIA_URL, XPOST_X_MEDIA_METADATA_URL, XPOST_X_WEB_URL.
Register <base URL>/auth/x/callback as the callback URL at X.
Posting (serve): XPOST_SCHEDULER=on starts the dispatcher (default off). Cost rates
XPOST_COST_POST_USD and XPOST_COST_LINK_POST_USD have no defaults; without them, or
without a monthly cap set in Settings, nothing posts. XPOST_COST_MEDIA_UPLOAD_USD (per
image upload) and XPOST_COST_MEDIA_METADATA_USD (per alt text call) have no defaults
either; without the first, drafts with images wait; without the second, drafts with alt
text wait. 0 is a valid rate.
`

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return "?"
}

// Scheduler modes.
const (
	schedOff  = ""
	schedOn   = "on"   // XPOST_SCHEDULER=on: the real X poster
	schedFake = "fake" // -fake-poster: local development only
)

// EnvScheduler turns posting on. Anything but "on" (or unset/"off") is an
// error, so a typo never silently means either state.
const EnvScheduler = "XPOST_SCHEDULER"

// EnvBlogsEnabled turns on the optional blog feature: blog drafts, the
// Blog queue kind, Settings > Blogs and its import, and the blog
// publisher. Off by default; only "1" turns it on and "" or "0" leave it
// off, anything else is an error. Off, blog rows already stored are kept
// but hidden, and XPOST_BLOG_LIVE or XPOST_BLOG_IMPORT set to 1 refuse to
// start (they would do nothing).
const EnvBlogsEnabled = "XPOST_BLOGS_ENABLED"

func blogsEnabledFromEnv(v string) (bool, error) {
	switch strings.TrimSpace(v) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("%s must be 1 or 0", EnvBlogsEnabled)
}

// EnvBlogLive switches on live blog publishing: through a blog's
// Publisher API (gander: the authenticated Test read and real
// publishes) and through a webhook blog's agent (the signed blog.test and
// blog.publish wakes and the live page checks). Off by default; only "1"
// turns it on and "" or "0" leave it off, anything else is an error. With
// it off, no blog host or agent is ever contacted.
const EnvBlogLive = "XPOST_BLOG_LIVE"

func blogLiveFromEnv(v string) (bool, error) {
	switch strings.TrimSpace(v) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("%s must be 1 or 0", EnvBlogLive)
}

// EnvBlogImport switches on the read-only import of a blog's existing
// posts (Settings > Blogs > Import), separately from XPOST_BLOG_LIVE. Off
// by default; only "1" turns it on, unset or empty leave it off, and any
// other value (also "0") refuses to start. On, only the import's GETs can
// leave, through their own import-only client: nothing publishes, wakes
// an agent or checks a live page because of it.
const EnvBlogImport = "XPOST_BLOG_IMPORT"

func blogImportFromEnv(v string) (bool, error) {
	switch v {
	case "":
		return false, nil
	case "1":
		return true, nil
	}
	return false, fmt.Errorf("%s must be unset, empty or 1", EnvBlogImport)
}

// importAllowed is the import-only client's check: gander's published
// list (with the sealed token) or a webhook blog's post index or post
// page. Everything else is refused before it is sent.
func importAllowed(r *http.Request) bool { return gander.ImportRead(r) || hookpub.ImportRead(r) }

// blogImporter is the import's read of a blog's live posts, through the
// import's own adapter instances (and so the import-only client). It is
// nil, and the import off with no call possible, unless on.
func blogImporter(on bool, api, hook blog.Lister) func(context.Context, *store.Blog) ([]store.UpstreamPost, error) {
	if !on {
		return nil
	}
	return func(ctx context.Context, b *store.Blog) ([]store.UpstreamPost, error) {
		r := api
		if b.Method == store.MethodWebhook {
			r = hook
		}
		if r == nil {
			return nil, store.ErrNoPublisher
		}
		return r.List(ctx, b)
	}
}

// blogWiring is every blog adapter serve uses. The publishing ones
// (ganderPub, hookPub) get the guarded client and run only as
// XPOST_BLOG_LIVE allows; the import's own instances (ganderImport,
// hookImport) get only the import-only client, whatever
// XPOST_BLOG_LIVE says, and are used for nothing but the import's List.
type blogWiring struct {
	ganderPub, ganderImport *gander.Publisher
	hookPub, hookImport     *hookpub.Publisher
	pub                     blog.Publisher
	test                    func(context.Context, *store.Blog) (string, error)
	importer                func(context.Context, *store.Blog) ([]store.UpstreamPost, error)
}

func wireBlogs(st *store.Store, mode string, live, imp bool, version string, logw io.Writer) *blogWiring {
	now := func() time.Time { return st.Now() }
	token := func(ctx context.Context, id int64) (string, error) { return st.BlogSecret(ctx, id, store.BlogAPIToken) }
	w := &blogWiring{
		// The gander adapter gets the server's guarded client in attach,
		// before anything can publish or test.
		ganderPub: &gander.Publisher{Now: now, Token: token},
		// The webhook-blog adapter likewise: the signed wake and the live
		// page check, only through the guarded client.
		hookPub: &hookpub.Publisher{Now: now, Version: version, Secret: func(ctx context.Context, id int64, field string) (string, error) {
			return st.BlogSecret(ctx, id, field)
		}},
		ganderImport: &gander.Publisher{Now: now, Token: token},
		hookImport:   &hookpub.Publisher{Now: now, Version: version}, // no Secret: the import sends none
	}
	w.pub, w.test = blogPublishers(mode, live, w.ganderPub, w.hookPub, logw)
	w.importer = blogImporter(imp, w.ganderImport, w.hookImport)
	return w
}

// attach gives each adapter its client from the server: the guarded one
// for publishing, the import-only one for the import.
func (w *blogWiring) attach(srv *web.Server) {
	w.ganderPub.HTTP = srv.BlogHTTP
	w.hookPub.HTTP = srv.BlogHTTP
	w.ganderImport.HTTP = srv.BlogImportHTTP
	w.hookImport.HTTP = srv.BlogImportHTTP
}

// blogPublishers picks the blog publishers for a scheduler mode and the
// live switch. pubs is what the blog dispatcher uses (nil: no blog
// dispatcher); test is Settings' Test. With live off, Test makes no call
// (store.ErrLiveOff; in fake mode a webhook blog's Test is the fake's dry
// run) and posts publish only through the fake (fake mode) or not at all.
// With live on, Publisher API blogs use api and webhook blogs wake their
// agent through hook.
func blogPublishers(mode string, live bool, api blog.Publisher, hook blog.Waker, logw io.Writer) (pubs blog.Publisher, test func(context.Context, *store.Blog) (string, error)) {
	fake := &blog.FakePublisher{Log: logw}
	var m blog.Methods
	switch {
	case mode == schedFake && live, mode == schedOn && live:
		m = blog.Methods{API: api, Hook: hook}
	case mode == schedFake:
		m = blog.Methods{API: fake, Webhook: fake}
	}
	if m.API != nil || m.Webhook != nil || m.Hook != nil {
		pubs = m
	}
	testers := blog.Methods{API: api, Hook: hook}
	test = func(ctx context.Context, b *store.Blog) (string, error) {
		if !live {
			if b.Method == store.MethodWebhook && mode == schedFake {
				return (&blog.FakePublisher{Log: io.Discard}).Test(ctx, b) // a dry run: nothing is sent
			}
			return "", store.ErrLiveOff
		}
		return testers.Test(ctx, b)
	}
	return pubs, test
}

func schedulerFromEnv(v string) (bool, error) {
	switch strings.TrimSpace(v) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("%s must be on or off", EnvScheduler)
}

// startDispatcher is the only place a poster or dispatcher is started. It is
// a variable so tests can prove serve never calls it unless asked. The
// returned channel closes when the loop has stopped (after any X call in
// flight finishes).
//
// Blog posts publish through blogPub (see blogPublishers): the fake in
// fake mode, and the Publisher API adapter only when XPOST_BLOG_LIVE=1.
// blogWake, when not nil, wakes that loop.
var startDispatcher = func(ctx context.Context, st *store.Store, mode string, xm *xauth.Manager, loc *time.Location, logw io.Writer, blogWake chan struct{}, blogPub blog.Publisher) <-chan struct{} {
	var p poster.Poster = &poster.Fake{Log: logw}
	if mode == schedOn {
		p = &xauth.XPoster{M: xm}
	}
	d := &poster.Dispatcher{Store: st, Poster: p, Loc: loc, Log: log.New(logw, "xpost dispatch: ", log.LstdFlags)}
	done := make(chan struct{})
	var blogDone chan struct{}
	if blogPub != nil && blogWake != nil {
		bd := &blog.Dispatcher{Store: st, Publisher: blogPub, Log: log.New(logw, "xpost blog: ", log.LstdFlags), Wake: blogWake, Loc: loc}
		blogDone = make(chan struct{})
		go func() {
			defer close(blogDone)
			bd.Loop(ctx, 30*time.Second)
		}()
	}
	go func() {
		defer close(done)
		d.Loop(ctx, 30*time.Second)
		if blogDone != nil {
			<-blogDone
		}
	}()
	return done
}

// startSweeper starts the promo sweep (24h return to pending). It runs in
// every scheduler mode, posting off included, and never calls X or a blog
// host. A variable so tests can see serve starts it.
var startSweeper = func(ctx context.Context, st *store.Store, logw io.Writer) <-chan struct{} {
	return sweep.Promos(ctx, st, sweep.Every, log.New(logw, "xpost sweep: ", log.LstdFlags))
}

// loopbackURL reports whether raw is an http(s) URL on localhost or a
// loopback IP, i.e. a development install.
func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type common struct {
	dbPath  string
	baseURL string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dbPath, "db", envOr("XPOST_DB", "xpost.db"), "SQLite database path")
	fs.StringVar(&c.baseURL, "base-url", envOr("XPOST_BASE_URL", "http://localhost:8090"), "public base URL (passkeys need https, or http://localhost)")
}

func (c *common) open(ctx context.Context, needKey bool) (*store.Store, func(), error) {
	var key *secret.Key
	k, err := secret.FromEnv()
	if err != nil && needKey {
		return nil, nil, err
	}
	if err == nil {
		key = k
	}
	d, err := db.Open(ctx, c.dbPath)
	if err != nil {
		return nil, nil, err
	}
	return store.New(d, key), func() { d.Close() }, nil
}

func setupURL(base, tok string) string { return strings.TrimRight(base, "/") + "/setup/" + tok }

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("xpost "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var c common
	fail := func(err error) int {
		fmt.Fprintf(stderr, "xpost %s: %v\n", cmd, err)
		return 1
	}
	switch cmd {
	case "version":
		fmt.Fprintln(stdout, "xpost", version)
		return 0

	case "backup":
		c.register(fs)
		out := fs.String("out", "", "destination file (must not exist); written 0600")
		if fs.Parse(args) != nil {
			return 2
		}
		if *out == "" {
			return fail(fmt.Errorf("-out is required"))
		}
		if err := db.Backup(ctx, c.dbPath, *out); err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "backup ok:", *out)
		return 0

	case "keygen":
		k, err := secret.NewKey()
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, k.Encode())
		return 0

	case "create-owner":
		c.register(fs)
		email := fs.String("email", "", "owner email")
		name := fs.String("name", "", "owner display name")
		tz := fs.String("tz", envOr("XPOST_TZ", ""), "IANA time zone for displayed times (optional; changeable in Settings)")
		if fs.Parse(args) != nil {
			return 2
		}
		st, closeFn, err := c.open(ctx, false)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		if *tz != "" {
			if _, err := time.LoadLocation(*tz); err != nil {
				return fail(err)
			}
		}
		u, tok, err := st.CreateOwner(ctx, *email, *name)
		if err != nil {
			return fail(err)
		}
		if *tz != "" {
			st.SetSetting(ctx, store.SettingTZ, *tz)
		}
		fmt.Fprintf(stdout, "Created owner %s.\nOpen this one-time setup link within %d hours to set a password and add a passkey:\n%s\n", u.Email, int(store.SetupTTL.Hours()), setupURL(c.baseURL, tok))
		return 0

	case "setup-link":
		c.register(fs)
		if fs.Parse(args) != nil {
			return 2
		}
		st, closeFn, err := c.open(ctx, false)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		u, err := st.Owner(ctx)
		if err != nil {
			return fail(fmt.Errorf("no owner yet: run xpost create-owner"))
		}
		tok, err := st.NewSetupLink(ctx, u.ID)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "One-time setup link for %s (expires in %d hours; earlier links are revoked). Using it sets a new password, signs out everywhere and adds a passkey:\n%s\n", u.Email, int(store.SetupTTL.Hours()), setupURL(c.baseURL, tok))
		return 0

	case "seed-demo":
		c.register(fs)
		if fs.Parse(args) != nil {
			return 2
		}
		st, closeFn, err := c.open(ctx, false)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		lines, err := demo.Seed(ctx, st)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, "Seeded invented example data:")
		for _, l := range lines {
			fmt.Fprintln(stdout, "  "+l)
		}
		return 0

	case "serve":
		c.register(fs)
		addr := fs.String("addr", envOr("XPOST_ADDR", "127.0.0.1:8090"), "listen address (keep it on 127.0.0.1 behind Caddy)")
		insecure := fs.Bool("insecure-cookies", false, "allow cookies over plain HTTP (local development only)")
		trustProxy := fs.Bool("trust-proxy", false, "use X-Forwarded-For from a proxy on 127.0.0.1 for the client IP")
		tz := fs.String("tz", envOr("XPOST_TZ", ""), "default IANA display zone until the owner picks one (default: the server's local zone)")
		demoBadge := fs.Bool("demo", false, "show the Example data badge")
		fake := fs.Bool("fake-poster", false, "run the dispatcher with the fake poster (logs, never calls X)")
		rpID := fs.String("rp-id", envOr("XPOST_RP_ID", ""), "WebAuthn relying party ID (default: the host of -base-url)")
		rpOrigin := fs.String("rp-origin", envOr("XPOST_RP_ORIGIN", ""), "origin browsers report for passkeys (default: from -base-url)")
		if fs.Parse(args) != nil {
			return 2
		}
		if *fake && !loopbackURL(c.baseURL) {
			return fail(fmt.Errorf("-fake-poster is for local development only (base URL %s is not loopback)", c.baseURL))
		}
		schedOnEnv, err := schedulerFromEnv(os.Getenv(EnvScheduler))
		if err != nil {
			return fail(err)
		}
		if schedOnEnv && *fake {
			return fail(fmt.Errorf("use %s=on or -fake-poster, not both", EnvScheduler))
		}
		mode := schedOff
		if schedOnEnv {
			mode = schedOn
		} else if *fake {
			mode = schedFake
		}
		rates, err := store.ParseRates(os.Getenv("XPOST_COST_POST_USD"), os.Getenv("XPOST_COST_LINK_POST_USD"))
		if err != nil {
			return fail(err)
		}
		if rates, err = rates.WithMediaRates(os.Getenv(store.EnvCostMediaUpload), os.Getenv(store.EnvCostMediaMetadata)); err != nil {
			return fail(err)
		}
		xstats, err := web.ParseXstatsURL(os.Getenv(web.EnvXstatsURL))
		if err != nil {
			return fail(err)
		}
		loc := time.Local
		if *tz != "" {
			l, err := time.LoadLocation(*tz)
			if err != nil {
				return fail(err)
			}
			loc = l
		}
		st, closeFn, err := c.open(ctx, true)
		if err != nil {
			return fail(err)
		}
		defer closeFn()
		st.Rates = rates
		xcfg, err := xauth.FromEnv(os.Getenv, c.baseURL)
		if err != nil {
			return fail(err)
		}
		if mode == schedOn && !xcfg.Enabled() {
			return fail(fmt.Errorf("%s=on needs X connect configured (%s)", EnvScheduler, xauth.EnvClientID))
		}
		blogLive, err := blogLiveFromEnv(os.Getenv(EnvBlogLive))
		if err != nil {
			return fail(err)
		}
		blogImport, err := blogImportFromEnv(os.Getenv(EnvBlogImport))
		if err != nil {
			return fail(err)
		}
		blogsOn, err := blogsEnabledFromEnv(os.Getenv(EnvBlogsEnabled))
		if err != nil {
			return fail(err)
		}
		if !blogsOn && (blogLive || blogImport) {
			return fail(fmt.Errorf("%s=1 or %s=1 needs %s=1 (blogs are an optional feature, off by default)", EnvBlogLive, EnvBlogImport, EnvBlogsEnabled))
		}
		bw := wireBlogs(st, mode, blogLive, blogImport, version, stderr)
		if !blogsOn {
			bw.pub, bw.test, bw.importer = nil, nil, nil
		}
		blogPub := bw.pub
		var blogWake chan struct{}
		var wake func()
		if blogPub != nil {
			blogWake = make(chan struct{}, 1)
			wake = func() {
				select {
				case blogWake <- struct{}{}:
				default:
				}
			}
		}
		srv, err := web.New(st, web.Config{BaseURL: c.baseURL, RPID: *rpID, RPOrigin: *rpOrigin, SecureCookies: !*insecure, TrustProxy: *trustProxy, Location: loc, Demo: *demoBadge, BlogsEnabled: blogsOn, X: xcfg, Version: version, Scheduler: mode, XstatsURL: xstats, BlogWake: wake, BlogTest: bw.test, BlogImport: bw.importer, ImportAllow: importAllowed})
		if err != nil {
			return fail(err)
		}
		bw.attach(srv)
		if blogsOn {
			fmt.Fprintf(stderr, "xpost: blogs on (%s=1)\n", EnvBlogsEnabled)
		} else {
			fmt.Fprintf(stderr, "xpost: blogs off (%s not 1): blog pages, the Blog queue kind and blog proposals are hidden\n", EnvBlogsEnabled)
		}
		if blogImport {
			fmt.Fprintf(stderr, "xpost: blog import on (%s=1): Settings > Blogs can read existing posts; only the import's GETs can leave\n", EnvBlogImport)
		} else {
			fmt.Fprintf(stderr, "xpost: blog import off (%s not 1)\n", EnvBlogImport)
		}
		sweepDone := startSweeper(ctx, st, stderr)
		var dispatchDone <-chan struct{}
		switch mode {
		case schedFake:
			dispatchDone = startDispatcher(ctx, st, mode, srv.XManager(), loc, stderr, blogWake, blogPub)
			fmt.Fprintln(stderr, "xpost: fake poster on: approved, due drafts are marked posted with fake ids; nothing is sent to X")
			if blogsOn {
				if blogLive {
					fmt.Fprintf(stderr, "xpost: live blog publishing on (%s=1): Publisher API blogs publish through their API; webhook blogs wake their agent\n", EnvBlogLive)
				} else {
					fmt.Fprintln(stderr, "xpost: fake blog publisher on: approved, due blog posts are marked published; nothing is sent to any blog")
				}
			}

		case schedOn:
			dispatchDone = startDispatcher(ctx, st, mode, srv.XManager(), loc, stderr, blogWake, blogPub)
			fmt.Fprintf(stderr, "xpost: scheduler ON: approved, due drafts post to X (create-post host %s, media host %s)\n", hostOf(xcfg.PostURL), hostOf(xcfg.MediaURL))
		}
		if !rates.Known {
			fmt.Fprintln(stderr, "xpost: cost rates unverified (XPOST_COST_POST_USD, XPOST_COST_LINK_POST_USD not set): the spend meter shows unverified and nothing posts")
		}
		if !rates.UploadKnown {
			fmt.Fprintln(stderr, "xpost: image upload rate unverified (XPOST_COST_MEDIA_UPLOAD_USD not set): drafts with images wait")
		}
		if !rates.AltKnown {
			fmt.Fprintln(stderr, "xpost: alt text rate unverified (XPOST_COST_MEDIA_METADATA_USD not set): drafts with alt text wait")
		}
		if xcfg.Enabled() {
			fmt.Fprintf(stderr, "xpost: X connect on (token endpoint host %s); callback %s\n", hostOf(xcfg.TokenURL), xcfg.RedirectURL)
		} else {
			fmt.Fprintf(stderr, "xpost: X connect off (%s not set)\n", xauth.EnvClientID)
		}
		if xstats != "" {
			fmt.Fprintf(stderr, "xpost: xstats links on (%s)\n", hostOf(xstats))
		} else {
			fmt.Fprintf(stderr, "xpost: xstats links off (%s not set)\n", web.EnvXstatsURL)
		}
		if mode == schedOff {
			fmt.Fprintf(stderr, "xpost: posting off: no dispatcher or poster is running (%s is not on)\n", EnvScheduler)
		}
		switch {
		case blogLive && mode == schedOff:
			fmt.Fprintf(stderr, "xpost: live blog publishing: Test reads Publisher API blogs and wakes webhook agents with blog.test (%s=1); nothing publishes while posting is off\n", EnvBlogLive)
		case blogLive && mode == schedOn:
			fmt.Fprintf(stderr, "xpost: live blog publishing on (%s=1): Publisher API blogs publish through their API; webhook blogs wake their agent\n", EnvBlogLive)
		case !blogLive:
			fmt.Fprintf(stderr, "xpost: live blog publishing off (%s not 1): no blog host is contacted\n", EnvBlogLive)
		}
		fmt.Fprintf(stderr, "xpost %s listening on %s (%s)\n", version, *addr, c.baseURL)
		serr := srv.ListenAndServe(ctx, *addr)
		if dispatchDone != nil && ctx.Err() != nil {
			<-dispatchDone // let an X call in flight finish and be recorded
		}
		if sweepDone != nil && ctx.Err() != nil {
			<-sweepDone
		}
		if serr != nil && serr.Error() != "http: Server closed" {
			return fail(serr)
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}
