// Package blog publishes approved blog posts through a Publisher.
// FakePublisher records what it would do and never touches the network
// (demo and tests). The gander Publisher API adapter lives in
// internal/blog/gander and is the only code under internal/blog that
// makes HTTP calls, through the guarded client it is given (see
// guard_test.go). Webhook blogs publish through their agent (hook.go,
// with the signed wake and the live check in internal/blog/hookpub).
package blog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

// Post is what a publisher gets: the post and where it goes.
type Post struct {
	DraftID     int64
	Slug        string
	Title       string
	Description string
	Tags        []string
	Markdown    string
	CoverAlt    string // stays in xpost: gander has no alt field
	HasCover    bool
	Cover       []byte // the cover image, for publishers that upload it
	CoverMIME   string
	PublishAt   time.Time
	Ref         string // set once staged
}

// Publisher is one way to put posts on a blog.
type Publisher interface {
	// Test checks the connection without publishing anything. It returns a
	// short owner-facing detail.
	Test(ctx context.Context, b *store.Blog) (string, error)
	// Stage creates or updates the post on the blog without making it
	// public, and returns the blog's reference for it. Staging the same
	// slug again returns the same reference.
	Stage(ctx context.Context, b *store.Blog, p Post) (string, error)
	// Publish makes a staged post public and returns its URL.
	Publish(ctx context.Context, b *store.Blog, p Post) (string, error)
	// Live reports whether the published URL serves the post.
	Live(ctx context.Context, b *store.Blog, url string) (bool, error)
}

// FakePublisher is the only Publisher in slice 1: it keeps posts in
// memory, makes no network calls, and logs only titles and slugs.
type FakePublisher struct {
	Log io.Writer
	// Fail, when set, makes Publish fail for that slug with the message.
	Fail map[string]string

	mu     sync.Mutex
	staged map[string]string // blog:slug -> ref
	live   map[string]bool
	calls  []string
}

func (f *FakePublisher) logf(format string, a ...any) {
	if f.Log != nil {
		fmt.Fprintf(f.Log, "xpost fake blog: "+format+"\n", a...)
	}
}

func (f *FakePublisher) record(c string) {
	f.calls = append(f.calls, c)
}

// Calls lists what the fake was asked to do, e.g. "stage blog/x".
func (f *FakePublisher) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Test is a dry run: nothing is sent or published.
func (f *FakePublisher) Test(_ context.Context, b *store.Blog) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("test " + b.Name)
	if !b.HasConnection() {
		return "", errors.New("Add the connection details first.")
	}
	return "Test passed. Nothing was published.", nil
}

// Stage records the post.
func (f *FakePublisher) Stage(_ context.Context, b *store.Blog, p Post) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.staged == nil {
		f.staged, f.live = map[string]string{}, map[string]bool{}
	}
	k := b.Name + "/" + p.Slug
	f.record("stage " + k)
	if ref, ok := f.staged[k]; ok {
		return ref, nil
	}
	ref := fmt.Sprintf("fake_%d", p.DraftID)
	f.staged[k] = ref
	f.logf("staged %q on %s as /%s", p.Title, b.Name, p.Slug)
	return ref, nil
}

// Publish marks the staged post live.
func (f *FakePublisher) Publish(_ context.Context, b *store.Blog, p Post) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := b.Name + "/" + p.Slug
	f.record("publish " + k)
	if msg := f.Fail[p.Slug]; msg != "" {
		return "", errors.New(msg)
	}
	if _, ok := f.staged[k]; !ok {
		return "", errors.New("the post was not staged")
	}
	u := b.LiveURL(p.Slug)
	f.live[u] = true
	f.logf("published %q at %s (fake: nothing was sent)", p.Title, u)
	return u, nil
}

// Live reports what Publish recorded.
func (f *FakePublisher) Live(_ context.Context, b *store.Blog, url string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("live " + url)
	return f.live[url], nil
}

// Methods picks the publisher for a blog's publish method: API for the
// Publisher API, Webhook for the webhook to an agent. A nil entry refuses.
type Methods struct {
	API     Publisher
	Webhook Publisher
	// Hook, when set, publishes webhook blogs through their agent (a
	// thin wake, then live checks) instead of Webhook. Only with
	// XPOST_BLOG_LIVE=1.
	Hook Waker
}

// HookFor is the Waker for b, or nil when b publishes another way.
func (m Methods) HookFor(b *store.Blog) Waker {
	if b.Method == store.MethodWebhook {
		return m.Hook
	}
	return nil
}

// ErrNoMethod is a blog whose method has no publisher on this server.
var ErrNoMethod = store.ErrNoPublisher

func (m Methods) pick(b *store.Blog) (Publisher, error) {
	p := m.Webhook
	if b.Method == store.MethodAPI {
		p = m.API
	}
	if p == nil {
		return nil, ErrNoMethod
	}
	return p, nil
}

// Lister reads the posts already live on a blog, for the import. It only
// ever GETs, and never writes to the blog.
type Lister interface {
	List(ctx context.Context, b *store.Blog) ([]store.UpstreamPost, error)
}

// Test runs the blog's publisher's dry run.
func (m Methods) Test(ctx context.Context, b *store.Blog) (string, error) {
	if w := m.HookFor(b); w != nil {
		return w.Test(ctx, b)
	}
	p, err := m.pick(b)
	if err != nil {
		return "", err
	}
	return p.Test(ctx, b)
}

// Stage stages through the blog's publisher.
func (m Methods) Stage(ctx context.Context, b *store.Blog, post Post) (string, error) {
	p, err := m.pick(b)
	if err != nil {
		return "", err
	}
	return p.Stage(ctx, b, post)
}

// Publish publishes through the blog's publisher.
func (m Methods) Publish(ctx context.Context, b *store.Blog, post Post) (string, error) {
	p, err := m.pick(b)
	if err != nil {
		return "", err
	}
	return p.Publish(ctx, b, post)
}

// Live checks through the blog's publisher.
func (m Methods) Live(ctx context.Context, b *store.Blog, url string) (bool, error) {
	p, err := m.pick(b)
	if err != nil {
		return false, err
	}
	return p.Live(ctx, b, url)
}

// Dispatcher publishes approved, due blog posts.
type Dispatcher struct {
	Store     *store.Store
	Publisher Publisher
	Log       *log.Logger
	Lease     time.Duration  // default 5 minutes
	Wake      chan struct{}  // optional: a send runs a pass now
	Loc       *time.Location // xpost's default zone (the owner's setting wins); UTC if nil
	NewID     func() string  // delivery ids for wakes; random if nil
}

func (d *Dispatcher) logf(format string, a ...any) {
	if d.Log != nil {
		d.Log.Printf(format, a...)
	}
}

// RunDue publishes every due post once. It returns how many it published.
func (d *Dispatcher) RunDue(ctx context.Context, now time.Time) (int, error) {
	due, err := d.Store.DueBlogs(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, du := range due {
		if ctx.Err() != nil {
			break
		}
		ok, err := d.one(ctx, now, du)
		if err != nil {
			d.logf("draft %d: %v", du.ID, err)
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func (d *Dispatcher) lease() time.Duration {
	if d.Lease > 0 {
		return d.Lease
	}
	return 5 * time.Minute
}

func (d *Dispatcher) one(ctx context.Context, now time.Time, du store.Due) (bool, error) {
	ok, err := d.Store.Claim(ctx, du, now.Add(d.lease()))
	if err != nil || !ok {
		return false, err
	}
	dr, err := d.Store.Draft(ctx, du.ID)
	if err != nil {
		return false, err
	}
	fail := func(why string) (bool, error) {
		return false, d.Store.FinishBlog(ctx, dr.ID, false, "", "", why)
	}
	b, err := d.Store.BlogForAccount(ctx, dr.Account)
	if err != nil {
		return fail("The blog for this post is gone.")
	}
	w := d.waker(b)
	if w != nil {
		// A publish already with the agent only needs its live check:
		// settings changed since then don't take it back.
		if pub, err := d.Store.LatestBlogPublish(ctx, dr.ID); err == nil && pub.InFlight() {
			return d.hookCheck(ctx, now, dr, b, pub, w, fail)
		}
	}
	if !b.Ready() {
		return fail(b.Name + " cannot publish until its last test passes.")
	}
	if e, _ := blogtext.Counts(store.LintPost(b, dr.Post)); e > 0 {
		return fail("Lint found an error. Fix it and approve again.")
	}
	p := Post{DraftID: dr.ID, Slug: dr.Post.Slug, Title: dr.Post.Title, Description: dr.Post.Description, Tags: dr.Post.Tags, Markdown: dr.Post.Markdown,
		HasCover: dr.Post.Cover != nil, PublishAt: dr.ScheduledAt, Ref: dr.Post.Ref}
	if dr.Post.Cover != nil {
		p.CoverAlt = dr.Post.Cover.Alt
		mime, data, err := d.Store.Media(ctx, dr.Post.Cover.Ref)
		if err != nil {
			return fail("The cover image is missing. Add it again and approve.")
		}
		p.Cover, p.CoverMIME = data, mime
	}
	if w != nil {
		return d.hookStart(ctx, now, dr, b, p, w, fail)
	}
	ref, err := d.Publisher.Stage(ctx, b, p)
	if err != nil {
		if strings.HasPrefix(err.Error(), store.BlogSlugTaken) {
			return fail(short(err)) // the card shows this as blocked
		}
		return fail("Staging failed: " + short(err))
	}
	if err := d.Store.MarkStaged(ctx, dr.ID, ref); err != nil {
		return false, err
	}
	p.Ref = ref
	u, err := d.Publisher.Publish(ctx, b, p)
	if err != nil {
		return fail(short(err))
	}
	live, err := d.Publisher.Live(ctx, b, u)
	if err != nil {
		return fail("Published, but the live check failed: " + short(err))
	}
	if !live {
		return fail("Published, but the live check did not find the post yet. Retry publish to check again.")
	}
	return true, d.Store.FinishBlog(ctx, dr.ID, true, u, ref, "")
}

// short keeps an error to one owner-facing line.
func short(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "..."
	}
	return s
}

// Loop runs RunDue every interval (or when woken) until ctx is done.
func (d *Dispatcher) Loop(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := d.RunDue(ctx, d.Store.Now()); err != nil {
			d.logf("blog dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.Wake:
		}
	}
}
