// Package gander publishes blog posts through gander's Publisher API
// (see gander's Publisher API docs). Every request goes through the
// HTTP client the caller injects: xpost's guarded webhook client (checked
// dialer, no proxy, no redirects). This package never builds a client or
// transport of its own and never falls back to Go's default client.
//
// Calls per publish, well under gander's 30 per hour: Stage does one GET
// and at most one PUT, Publish one POST, Live one API GET plus one
// unauthenticated GET of the public page. Test is one GET. Nothing polls
// and nothing retries: a 429 fails the publish with a note.
//
// Error text is built only from the HTTP status and gander's own short
// error codes (a fixed list). Response messages, URLs, decoder text and
// secrets never reach an error.
package gander

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/store"
)

// postsPath is appended to the blog's API base (the gander site root).
const postsPath = "/api/blog/posts"

// Limits gander enforces, checked here before anything is sent.
const (
	MaxCoverBytes    = 2 << 20   // 2 MiB
	MaxMarkdownBytes = 256 << 10 // gander's default MAX_BLOG_BYTES
	maxBody          = 4 << 20   // response bodies read at most this much
)

// coverTypes are the cover image types gander accepts.
var coverTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// SlugTaken is the start of the error when the slug is already used on
// gander by a post xpost did not create. The card shows it as blocked.
const SlugTaken = store.BlogSlugTaken

// Publisher is the gander Publisher API adapter.
type Publisher struct {
	// HTTP returns the guarded client. Nil, or a nil client, refuses
	// every call: there is no fallback client.
	HTTP func() *http.Client
	// Token opens the blog's sealed API token. It is only ever put in the
	// Authorization header, never logged or returned.
	Token func(ctx context.Context, blogID int64) (string, error)
	// Now is the clock for the UTC publish date and the live check.
	Now func() time.Time
}

var _ blog.Publisher = (*Publisher)(nil)

func (p *Publisher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// view is the post as gander's API returns it (only what xpost reads).
type view struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Date        string `json:"date"`
	Draft       bool   `json:"draft"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`
}

// upsert is the PUT body: exactly gander's fields (it refuses unknown
// ones), so no tags and no alt text.
type upsert struct {
	Title            string `json:"title"`
	Date             string `json:"date"`
	Description      string `json:"description"`
	Markdown         string `json:"markdown"`
	Draft            bool   `json:"draft"`
	CoverBase64      string `json:"cover_base64"`
	CoverContentType string `json:"cover_content_type"`
}

// site is the checked API base: an https site root.
func site(b *store.Blog) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(b.APIBase))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("The API base URL must be the gander site root over https. Fix it in Settings.")
	}
	u.Path = ""
	return u, nil
}

// ErrNoClient is returned when no guarded client was injected.
var ErrNoClient = errors.New("No guarded HTTP client is set up for blog publishing; nothing was sent.")

func (p *Publisher) client() (*http.Client, error) {
	if p.HTTP == nil {
		return nil, ErrNoClient
	}
	c := p.HTTP()
	if c == nil {
		return nil, ErrNoClient
	}
	return c, nil
}

// knownCodes are gander's error codes; only these are ever shown.
var knownCodes = map[string]bool{"missing_bearer": true, "invalid_token": true, "rate_limited": true, "invalid_slug": true,
	"invalid_json": true, "invalid_post": true, "not_found": true, "db": true}

// apiError is a non-2xx answer: what was being done, the status and
// gander's code (when it is a known one).
type apiError struct {
	What   string
	Status int
	Code   string
}

func (e *apiError) Error() string {
	s := fmt.Sprintf("%s returned %d %s", e.What, e.Status, http.StatusText(e.Status))
	if e.Code != "" {
		s += " (" + e.Code + ")"
	}
	s += "."
	switch {
	case e.Status == http.StatusUnauthorized:
		s += " Check the API token."
	case e.Status == http.StatusTooManyRequests:
		s += " gander rate limited xpost; not retried. Try again later."
	case e.Status >= 300 && e.Status < 400:
		s += " Redirects are not followed; check the API base URL."
	case e.Status == http.StatusNotFound && e.Code == "":
		s += " Check the API base URL."
	}
	return s
}

func codeOf(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && knownCodes[e.Error] {
		return e.Error
	}
	return ""
}

// netError turns a transport error into a short line with no URL, host or
// address in it.
func netError(what string, err error) error {
	msg := "could not connect"
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		msg = "timed out"
	case errors.Is(err, context.Canceled):
		msg = "was cancelled"
	case strings.Contains(err.Error(), "destination not allowed"):
		msg = "was refused: the address is private or reserved"
	case strings.Contains(err.Error(), "not an import read"):
		msg = "was refused: this server only allows the import's reads"
	case strings.Contains(err.Error(), "x509") || strings.Contains(err.Error(), "tls:"):
		msg = "failed: the TLS certificate was not accepted"
	}
	return fmt.Errorf("%s %s. Nothing more was sent.", what, msg)
}

// do sends one request and reads at most maxBody of the answer. auth
// adds the Bearer token; the public live check sends none.
func (p *Publisher) do(ctx context.Context, b *store.Blog, what, method, target string, body []byte, auth bool) (int, []byte, error) {
	c, err := p.client()
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, fmt.Errorf("%s could not be built.", what)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "xpost")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		if p.Token == nil {
			return 0, nil, errors.New("No API token is set. Add it in Settings.")
		}
		tok, err := p.Token(ctx, b.ID)
		if err != nil || tok == "" {
			return 0, nil, errors.New("No API token is set. Add it in Settings.")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	res, err := c.Do(req)
	if err != nil {
		return 0, nil, netError(what, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return res.StatusCode, nil, netError(what, err)
	}
	return res.StatusCode, data, nil
}

func postURL(base *url.URL, slug, suffix string) string {
	u := *base
	u.Path = postsPath + "/" + slug + suffix
	return u.String()
}

// Test is one authenticated read (list published posts). It never writes.
func (p *Publisher) Test(ctx context.Context, b *store.Blog) (string, error) {
	base, err := site(b)
	if err != nil {
		return "", err
	}
	u := *base
	u.Path, u.RawQuery = postsPath, "published=1"
	st, data, err := p.do(ctx, b, "Listing posts", http.MethodGet, u.String(), nil, true)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", &apiError{"Listing posts", st, codeOf(data)}
	}
	var out struct {
		Posts []view `json:"posts"`
	}
	if json.Unmarshal(data, &out) != nil {
		return "", errors.New("Listing posts returned 200 OK, but not gander's answer. Check the API base URL.")
	}
	return fmt.Sprintf("Listed posts, 200 OK (%d published). Nothing was changed.", len(out.Posts)), nil
}

// ImportRead reports whether r is the import's one gander request: an
// authenticated GET of the published posts list, nothing else. The query
// must be exactly "published=1" (no other order, case, encoding or extra
// parameter), with no fragment, escaped path or credentials in the URL.
// The import-only client (XPOST_BLOG_IMPORT=1) lets only such reads through.
func ImportRead(r *http.Request) bool {
	u := r.URL
	return r.Method == http.MethodGet && u != nil && u.Opaque == "" && u.Path == postsPath && u.RawPath == "" &&
		u.RawQuery == "published=1" && u.Fragment == "" && u.RawFragment == "" && u.User == nil &&
		strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") && r.Body == nil
}

// List is the import's read: one authenticated GET of the published
// posts (gander leaves out hidden drafts and future dates). It never
// writes. Each post carries xpost's reference for it, so a later publish
// on that slug knows the post. A post gander lists that is still a draft
// or dated after today is reported as not live, never imported.
func (p *Publisher) List(ctx context.Context, b *store.Blog) ([]store.UpstreamPost, error) {
	base, err := site(b)
	if err != nil {
		return nil, err
	}
	u := *base
	u.Path, u.RawQuery = postsPath, "published=1"
	st, data, err := p.do(ctx, b, "Listing posts", http.MethodGet, u.String(), nil, true)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, &apiError{"Listing posts", st, codeOf(data)}
	}
	var out struct {
		Posts []view `json:"posts"`
	}
	if json.Unmarshal(data, &out) != nil {
		return nil, errors.New("Listing posts returned 200 OK, but not gander's answer. Check the API base URL.")
	}
	if len(out.Posts) > store.MaxImport {
		return nil, store.ErrImportTooMany
	}
	today := p.now().UTC().Format("2006-01-02")
	var ups []store.UpstreamPost
	for _, v := range out.Posts {
		up := store.UpstreamPost{Slug: v.Slug, Title: v.Title, Description: v.Description, Date: v.Date, Live: true}
		switch {
		case v.Draft:
			up.Live, up.Why = false, "it is a hidden draft on gander."
		case v.Date > today:
			up.Live, up.Why = false, "it is dated after today."
		case v.CreatedAt == "":
			up.Live, up.Why = false, "gander gave no created_at for it."
		}
		if up.Live {
			up.URL, up.Ref = liveURL(b, v), RefFor(v)
		}
		ups = append(ups, up)
	}
	return ups, nil
}

// RefFor is xpost's reference for a post it created: the slug and
// gander's created_at, so a post someone deleted and re-created under the
// same slug is not taken for ours.
func RefFor(v view) string { return "gander:" + v.Slug + "@" + v.CreatedAt }

// Check validates a post locally against gander's rules, before any call.
func Check(p blog.Post) error {
	switch {
	case !slugRe.MatchString(p.Slug):
		return errors.New("The slug must be lowercase words joined by hyphens for gander.")
	case strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Description) == "" || strings.TrimSpace(p.Markdown) == "":
		return errors.New("gander needs a title, a description and a body.")
	case len(p.Markdown) > MaxMarkdownBytes:
		return errors.New("The body is over gander's 256 KiB limit.")
	case !utf8.ValidString(p.Markdown):
		return errors.New("The body is not valid UTF-8.")
	case len(p.Cover) == 0:
		return errors.New("gander needs a cover image.")
	case !coverTypes[strings.ToLower(p.CoverMIME)]:
		return errors.New("gander takes a PNG, JPEG, WebP or GIF cover only.")
	case len(p.Cover) > MaxCoverBytes:
		return errors.New("The cover is over gander's 2 MiB limit.")
	}
	return nil
}

// Stage creates the post as a hidden draft, or updates it when xpost's
// own reference for it is recorded. A post on the slug that xpost did not
// create is never overwritten. A post of ours that is already live (a
// retry after the live check failed) is left as it is: a PUT with draft
// true would take it down.
func (p *Publisher) Stage(ctx context.Context, b *store.Blog, post blog.Post) (string, error) {
	base, err := site(b)
	if err != nil {
		return "", err
	}
	if err := Check(post); err != nil {
		return "", err
	}
	target := postURL(base, post.Slug, "")
	st, data, err := p.do(ctx, b, "Reading the post", http.MethodGet, target, nil, true)
	if err != nil {
		return "", err
	}
	switch st {
	case http.StatusNotFound:
		if codeOf(data) != "not_found" {
			return "", &apiError{"Reading the post", st, ""}
		}
	case http.StatusOK:
		var v view
		if json.Unmarshal(data, &v) != nil || v.Slug != post.Slug {
			return "", errors.New("Reading the post returned 200 OK, but not gander's answer.")
		}
		if post.Ref == "" || post.Ref != RefFor(v) {
			return "", errors.New(SlugTaken + " xpost will not overwrite a post it did not create.")
		}
		if !v.Draft {
			return post.Ref, nil
		}
	default:
		return "", &apiError{"Reading the post", st, codeOf(data)}
	}
	body, _ := json.Marshal(upsert{Title: post.Title, Date: p.now().UTC().Format("2006-01-02"), Description: post.Description, Markdown: post.Markdown,
		Draft: true, CoverBase64: base64.StdEncoding.EncodeToString(post.Cover), CoverContentType: strings.ToLower(post.CoverMIME)})
	st, data, err = p.do(ctx, b, "Staging the post", http.MethodPut, target, body, true)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", &apiError{"Staging the post", st, codeOf(data)}
	}
	var v view
	if json.Unmarshal(data, &v) != nil || v.Slug != post.Slug || !v.Draft || v.CreatedAt == "" {
		return "", errors.New("Staging returned 200 OK, but not a hidden draft of this post.")
	}
	return RefFor(v), nil
}

// Publish flips the staged post to public and returns its URL as gander
// gives it (no trailing slash), checked to be on the blog's site.
func (p *Publisher) Publish(ctx context.Context, b *store.Blog, post blog.Post) (string, error) {
	base, err := site(b)
	if err != nil {
		return "", err
	}
	if post.Ref == "" || !strings.HasPrefix(post.Ref, "gander:"+post.Slug+"@") {
		return "", errors.New("The post was not staged by xpost; nothing was published.")
	}
	st, data, err := p.do(ctx, b, "Publishing", http.MethodPost, postURL(base, post.Slug, "/publish"), nil, true)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", &apiError{"Publishing", st, codeOf(data)}
	}
	var v view
	if json.Unmarshal(data, &v) != nil || v.Slug != post.Slug {
		return "", errors.New("Publishing returned 200 OK, but not gander's answer.")
	}
	return liveURL(b, v), nil
}

// liveURL is the API's url when it is the post's page on the blog's own
// site (gander gives it with no trailing slash); otherwise the page gander
// serves at <site>/blog/<slug>.
func liveURL(b *store.Blog, v view) string {
	if u, err := url.Parse(v.URL); err == nil && u.Scheme == "https" && u.RawQuery == "" && u.Fragment == "" && u.User == nil {
		if s, err := url.Parse(b.SiteURL); err == nil && strings.EqualFold(u.Host, s.Host) && u.Path == "/blog/"+v.Slug {
			return u.String()
		}
	}
	return b.LiveURL(v.Slug)
}

var (
	errNotPostPage = errors.New("The live address is not a gander post page.")
	errOtherHost   = errors.New("The live address is not on the blog's site, so it was not checked. Nothing is marked live.")
	errNoLiveSetup = errors.New("The live check has no blog or publisher set up, so nothing was checked. Nothing is marked live.")
)

// Live confirms the post is public: the API shows it published with a
// date on or before today (UTC), and the public page answers 200 to a
// request that carries no token. Redirects count as not live.
func (p *Publisher) Live(ctx context.Context, b *store.Blog, live string) (bool, error) {
	if p == nil || b == nil {
		return false, errNoLiveSetup
	}
	base, err := site(b)
	if err != nil {
		return false, err
	}
	u, err := url.Parse(live)
	if err != nil {
		return false, errNotPostPage
	}
	slug := strings.TrimPrefix(u.Path, "/blog/")
	if u.Scheme != "https" || u.User != nil || !slugRe.MatchString(slug) {
		return false, errNotPostPage
	}
	// The public page must be on the blog's own site: never check (or
	// mark live) a page on another host.
	if s, err := url.Parse(strings.TrimSpace(b.SiteURL)); err != nil || !blog.SameHost(u, s) {
		return false, errOtherHost
	}
	st, data, err := p.do(ctx, b, "Checking the post", http.MethodGet, postURL(base, slug, ""), nil, true)
	if err != nil {
		return false, err
	}
	if st != http.StatusOK {
		return false, &apiError{"Checking the post", st, codeOf(data)}
	}
	var v view
	if json.Unmarshal(data, &v) != nil || v.Slug != slug {
		return false, errors.New("Checking the post returned 200 OK, but not gander's answer.")
	}
	day, err := time.Parse("2006-01-02", v.Date)
	if v.Draft || err != nil || day.After(p.now().UTC()) {
		return false, nil
	}
	st, _, err = p.do(ctx, b, "Opening the public page", http.MethodGet, live, nil, false)
	if err != nil {
		return false, err
	}
	return st == http.StatusOK, nil
}
