// Package hookpub reaches a webhook blog's agent and its live site: the
// thin signed wake (blog.publish, blog.test), the live page check, and the
// import's read of the site's post index and pages (GET only).
//
// It builds every request in one place (do) and sends it only through the
// client it is given: web.Server.BlogHTTP, the guarded client rewrite
// webhooks use (checked dialer, no proxy, no redirects, a timeout). It is
// wired in for publishing only when XPOST_BLOG_LIVE=1; the import's own
// instance (XPOST_BLOG_IMPORT=1) gets only the import-only client. Errors name only a status number
// or a transport word, never a URL, a reply body or a secret.
package hookpub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/store"
)

// Limits.
const (
	maxReply = 64 << 10 // read at most this much of the agent's answer
	maxPage  = 1 << 20  // and of the live page
	maxIndex = 1 << 20  // and of the site's post index
	// TestWindow is how long a blog.test wake stays valid.
	TestWindow = 5 * time.Minute
)

// Publisher implements blog.Waker.
type Publisher struct {
	HTTP func() *http.Client
	// Secret opens one of the blog's sealed fields (store.BlogSecret).
	Secret  func(ctx context.Context, blogID int64, field string) (string, error)
	Now     func() time.Time
	NewID   func() string
	Version string
}

var _ blog.Waker = (*Publisher)(nil)

// ErrNoClient is a Publisher with no client: it never builds its own.
var ErrNoClient = errors.New("The blog publisher has no network client on this server. Nothing was sent.")

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Sign is the X-Xpost-Signature value for body at ts, the same scheme as
// the Rewrite webhook: sha256=<hex HMAC-SHA256(key, ts + "." + body)>.
func Sign(key string, ts int64, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(strconv.FormatInt(ts, 10)))
	m.Write([]byte("."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// wakeBody is the whole wake: five fields, no content.
type wakeBody struct {
	Event      string `json:"event"`
	PublishID  int64  `json:"publish_id"`
	Version    int    `json:"version"`
	DeliveryID string `json:"delivery_id"`
	ExpiresAt  string `json:"expires_at"`
}

// Test sends a signed blog.test wake. Any 2xx passes.
func (p *Publisher) Test(ctx context.Context, b *store.Blog) (string, error) {
	id := "d_test"
	if p.NewID != nil {
		id = p.NewID()
	}
	if err := p.Wake(ctx, b, blog.Wake{Event: blog.EventTest, DeliveryID: id, ExpiresAt: p.now().Add(TestWindow)}); err != nil {
		return "", err
	}
	return "The agent answered blog.test with a 2xx. Nothing was published.", nil
}

// Wake POSTs the signed wake to the blog's webhook.
func (p *Publisher) Wake(ctx context.Context, b *store.Blog, w blog.Wake) error {
	if p.Secret == nil {
		return errors.New("The publish webhook is not set. Add it in Settings.")
	}
	hookURL, err := p.Secret(ctx, b.ID, store.BlogHookURL)
	if err != nil || hookURL == "" {
		return errors.New("The publish webhook URL is not set. Add it in Settings.")
	}
	key, err := p.Secret(ctx, b.ID, store.BlogHookKey)
	if err != nil || key == "" {
		return errors.New("The sender key is not set. Add it in Settings.")
	}
	auth, err := p.Secret(ctx, b.ID, store.BlogHookAuth)
	if err != nil {
		return errors.New("The Authorization value could not be opened. Save it again in Settings.")
	}
	body, err := json.Marshal(wakeBody{Event: w.Event, PublishID: w.PublishID, Version: w.Version, DeliveryID: w.DeliveryID, ExpiresAt: w.ExpiresAt.UTC().Format(time.RFC3339)})
	if err != nil {
		return errors.New("The wake could not be built. Nothing was sent.")
	}
	ts := p.now().Unix()
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Xpost-Event", w.Event)
	h.Set("X-Xpost-Timestamp", strconv.FormatInt(ts, 10))
	h.Set("X-Xpost-Signature", Sign(key, ts, body))
	h.Set("X-Xpost-Delivery", w.DeliveryID)
	if auth != "" {
		h.Set("Authorization", auth) // as saved; never logged
	}
	status, _, _, err := p.do(ctx, http.MethodPost, hookURL, h, body, maxReply)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("HTTP %d. Check the Authorization header. Nothing more was sent.", status)
	case status >= 300 && status < 400:
		return fmt.Errorf("HTTP %d: the webhook redirects, and xpost does not follow redirects. Nothing more was sent.", status)
	}
	return fmt.Errorf("HTTP %d. Nothing more was sent.", status)
}

var (
	errNotLivePage = errors.New("The live address is not an https address ending in a slash.")
	errOtherHost   = errors.New("The live address is not on the blog's site, so it was not checked. Nothing is marked live.")
	errNoLiveSetup = errors.New("The live check has no blog or publisher set up, so nothing was checked. Nothing is marked live.")
)

// Check GETs liveURL exactly, with no token and no redirects. It is
// confirmed only by a 200 text/html page that contains marker. An address
// with a user, query or fragment, or on a host other than the blog's site
// (blog.SameHost), is refused before any call.
func (p *Publisher) Check(ctx context.Context, b *store.Blog, liveURL, marker string) (bool, string, error) {
	if p == nil || b == nil {
		return false, "", errNoLiveSetup
	}
	// A bare "?" or "#" parses to an empty query or fragment, so the raw
	// address is checked too: the page must be exactly scheme, host, path.
	u, err := url.Parse(liveURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(liveURL, "?#") || !strings.HasSuffix(u.Path, "/") {
		return false, "", errNotLivePage
	}
	// The page must be on the blog's own site: no call to any other host.
	if s, err := url.Parse(strings.TrimSpace(b.SiteURL)); err != nil || !blog.SameHost(u, s) {
		return false, "", errOtherHost
	}
	h := http.Header{}
	h.Set("Accept", "text/html")
	status, ctype, page, err := p.do(ctx, http.MethodGet, liveURL, h, nil, maxPage)
	if err != nil {
		return false, "", err
	}
	switch {
	case status >= 300 && status < 400:
		return false, "the live URL redirects.", nil
	case status != http.StatusOK:
		return false, fmt.Sprintf("the page returned %d.", status), nil
	case !strings.HasPrefix(strings.ToLower(ctype), "text/html"):
		return false, "the page is not HTML.", nil
	case !bytes.Contains(page, []byte(marker)):
		return false, "the page does not show this version yet.", nil
	}
	return true, "", nil
}

// do is the only place a request is built and sent.
// IndexPath is where a webhook blog's site lists its posts, under the
// site root: [{slug, title, date, description}], newest first.
const IndexPath = "/blog/posts.json"

var slugOK = func(s string) bool {
	if s == "" || len(s) > 80 || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// ImportRead reports whether r is one of the import's site reads: a GET
// of the post index or of one post page (/blog/<slug>/), with no query
// (not even a bare "?"), no fragment, no escaped path, no credentials and
// no body. The import-only client (XPOST_BLOG_IMPORT=1) lets only such
// reads through, so no wake or other request can leave. A bare "#" is
// dropped when Go parses a URL, so List also refuses one in the addresses
// it builds (importURL) before any request exists.
func ImportRead(r *http.Request) bool {
	u := r.URL
	if r.Method != http.MethodGet || u == nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || r.Body != nil || r.Header.Get("Authorization") != "" {
		return false
	}
	if r.URL.Path == IndexPath {
		return true
	}
	slug, ok := strings.CutPrefix(r.URL.Path, "/blog/")
	if !ok {
		return false
	}
	slug, ok = strings.CutSuffix(slug, "/")
	return ok && slugOK(slug)
}

// importURL reports whether a URL string List built is a plain address:
// no "?" or "#" at all, so a bare one can't slip past ImportRead.
func importURL(s string) bool { return !strings.ContainsAny(s, "?#") }

// List is the import's read of the site: one GET of the post index, then
// one GET of each listed post's live page (exactly, no token, no
// redirects), all on the blog's own site. A post counts as live only when
// its page answers 200 with HTML. It never writes and sends no secret.
func (p *Publisher) List(ctx context.Context, b *store.Blog) ([]store.UpstreamPost, error) {
	s, err := url.Parse(strings.TrimSpace(b.SiteURL))
	if err != nil || s.Scheme != "https" || s.Host == "" || s.User != nil || s.RawQuery != "" || s.ForceQuery || s.Fragment != "" ||
		strings.ContainsAny(b.SiteURL, "?#") || strings.Trim(s.Path, "/") != "" {
		return nil, errors.New("The site URL is not an https site root, so nothing was read.")
	}
	idx := *s
	idx.Path = IndexPath
	h := http.Header{}
	h.Set("Accept", "application/json")
	if !importURL(idx.String()) {
		return nil, errors.New("The site URL is not an https site root, so nothing was read.")
	}
	status, ctype, data, err := p.do(ctx, http.MethodGet, idx.String(), h, nil, maxIndex)
	if err != nil {
		return nil, err
	}
	switch {
	case status >= 300 && status < 400:
		return nil, errors.New("The post index redirects, so it was not read. Nothing was imported.")
	case status != http.StatusOK:
		return nil, fmt.Errorf("Reading the post index returned %d. Nothing was imported.", status)
	case !strings.Contains(strings.ToLower(ctype), "json"):
		return nil, errors.New("The post index is not JSON. Nothing was imported.")
	}
	var list []struct {
		Slug        string `json:"slug"`
		Title       string `json:"title"`
		Date        string `json:"date"`
		Description string `json:"description"`
	}
	if json.Unmarshal(data, &list) != nil {
		return nil, errors.New("The post index is not a list of posts. Nothing was imported.")
	}
	if len(list) > store.MaxImport {
		return nil, store.ErrImportTooMany
	}
	var ups []store.UpstreamPost
	for _, e := range list {
		up := store.UpstreamPost{Slug: e.Slug, Title: e.Title, Description: e.Description, Date: e.Date}
		if !slugOK(e.Slug) {
			ups = append(ups, up) // the plan reports it; no page is read
			continue
		}
		live := b.LiveURL(e.Slug)
		u, err := url.Parse(live)
		if err != nil || !blog.SameHost(u, s) || !importURL(live) || u.RawPath != "" {
			up.Why = "its live address is not on the blog's site."
			ups = append(ups, up)
			continue
		}
		ph := http.Header{}
		ph.Set("Accept", "text/html")
		st, pct, _, err := p.do(ctx, http.MethodGet, live, ph, nil, maxPage)
		if err != nil {
			return nil, err
		}
		switch {
		case st >= 300 && st < 400:
			up.Why = "its page redirects."
		case st != http.StatusOK:
			up.Why = fmt.Sprintf("its page returned %d.", st)
		case !strings.HasPrefix(strings.ToLower(pct), "text/html"):
			up.Why = "its page is not HTML."
		default:
			up.Live, up.URL = true, live
		}
		ups = append(ups, up)
	}
	return ups, nil
}

func (p *Publisher) do(ctx context.Context, method, target string, h http.Header, body []byte, limit int64) (int, string, []byte, error) {
	if p.HTTP == nil {
		return 0, "", nil, ErrNoClient
	}
	c := p.HTTP()
	if c == nil {
		return 0, "", nil, ErrNoClient
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, "", nil, errors.New("The request could not be built. Nothing was sent.")
	}
	req.Header = h
	ua := "xpost"
	if p.Version != "" {
		ua += "/" + p.Version
	}
	req.Header.Set("User-Agent", ua)
	res, err := c.Do(req)
	if err != nil {
		return 0, "", nil, netError(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, limit))
	if err != nil {
		return res.StatusCode, "", nil, netError(err)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), data, nil
}

func netError(err error) error {
	msg := "no connection"
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		msg = "timeout"
	case errors.Is(err, context.Canceled):
		msg = "cancelled"
	case strings.Contains(err.Error(), "destination not allowed"):
		msg = "refused: the address is private or reserved"
	case strings.Contains(err.Error(), "not an import read"):
		msg = "refused: this server only allows the import's reads"
	case strings.Contains(err.Error(), "no such host"):
		msg = "host not found"
	case strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "tls:"):
		msg = "the TLS certificate was not accepted"
	}
	return fmt.Errorf("%s. Nothing more was sent.", msg)
}
