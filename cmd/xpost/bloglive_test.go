package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/blog/gander"
	"github.com/scott/xpost/internal/blog/gander/gfake"
	"github.com/scott/xpost/internal/blog/hookpub"
	"github.com/scott/xpost/internal/store"
)

// XPOST_BLOG_LIVE is off unless exactly "1"; a typo refuses to start.
func TestBlogLiveSwitchFromEnv(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "1": true, " 1 ": true} {
		if got, err := blogLiveFromEnv(v); err != nil || got != want {
			t.Errorf("%q: %v %v", v, got, err)
		}
	}
	for _, v := range []string{"on", "true", "yes", "2"} {
		if _, err := blogLiveFromEnv(v); err == nil {
			t.Errorf("%q accepted", v)
		}
	}
	dbp := testEnv(t)
	recordDispatch(t)
	t.Setenv(EnvBlogsEnabled, "1") // blogs are optional; these switches need them on
	t.Setenv(EnvBlogLive, "on")
	if code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies", "-fake-poster"); code == 0 || !strings.Contains(logs, "XPOST_BLOG_LIVE must be 1 or 0") {
		t.Fatalf("typo: exit %d %s", code, logs)
	}
	t.Setenv(EnvBlogLive, "")
	code, logs := serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies", "-fake-poster")
	if code != 0 || !strings.Contains(logs, "live blog publishing off") {
		t.Fatalf("off: exit %d %s", code, logs)
	}
	if m, ok := lastBlogPub.(blog.Methods); !ok || !isFake(m.API) || !isFake(m.Webhook) {
		t.Fatalf("off, fake mode: blog publisher %#v", lastBlogPub)
	}
	t.Setenv(EnvBlogLive, "1")
	code, logs = serveFor(t, "-db", dbp, "-base-url", "http://localhost:8090", "-insecure-cookies", "-fake-poster")
	if code != 0 || !strings.Contains(logs, "live blog publishing on (XPOST_BLOG_LIVE=1)") {
		t.Fatalf("on: exit %d %s", code, logs)
	}
	m, ok := lastBlogPub.(blog.Methods)
	gp, isGander := m.API.(*gander.Publisher)
	hp, isHook := m.Hook.(*hookpub.Publisher)
	if !ok || !isGander || gp.HTTP == nil || m.Webhook != nil || !isHook || hp.HTTP == nil {
		t.Fatalf("on, fake mode: blog publisher %#v", lastBlogPub)
	}
	// The webhook adapter gets the same guarded client.
	if hc := hp.HTTP(); hc.CheckRedirect == nil || hc.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the webhook client is not the guarded one")
	}
	// The adapter's client is the server's guarded one: no proxy, no redirects.
	c := gp.HTTP()
	if c.CheckRedirect == nil || c.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the gander client is not the guarded one")
	}
}

func isFake(p blog.Publisher) bool { _, ok := p.(*blog.FakePublisher); return ok }

// With the switch off, Test on a Publisher API blog makes no call and
// says so, and nothing real publishes in any mode.
func TestBlogLiveOffMakesNoCall(t *testing.T) {
	fake := &gfake.Server{Token: "example-gander-token-0000000000000000", Now: time.Now}
	srv := httptest.NewTLSServer(fake.Handler())
	defer srv.Close()
	fake.Base = srv.URL
	gp := &gander.Publisher{HTTP: srv.Client, Token: func(context.Context, int64) (string, error) { return fake.Token, nil }}
	api := &store.Blog{ID: 1, Name: "guide.example.test", Method: store.MethodAPI, SiteURL: srv.URL, APIBase: srv.URL, HasAPIToken: true}
	for _, mode := range []string{schedOff, schedFake, schedOn} {
		pubs, test := blogPublishers(mode, false, gp, nil, io.Discard)
		if _, err := test(context.Background(), api); !errors.Is(err, store.ErrLiveOff) || !strings.Contains(err.Error(), "Live publishing is disabled on this server") {
			t.Errorf("%q: test %v", mode, err)
		}
		if m, ok := pubs.(blog.Methods); ok {
			if _, real := m.API.(*gander.Publisher); real {
				t.Errorf("%q: live off but the API publisher is real", mode)
			}
		} else if pubs != nil {
			t.Errorf("%q: publisher %#v", mode, pubs)
		}
		if mode != schedFake && pubs != nil {
			t.Errorf("%q: a blog dispatcher runs with live off", mode)
		}
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("live off made %d calls", n)
	}
	// On: Test is one authenticated read; posting off still runs no dispatcher.
	pubs, test := blogPublishers(schedOff, true, gp, nil, io.Discard)
	if d, err := test(context.Background(), api); err != nil || !strings.HasPrefix(d, "Listed posts, 200 OK") || pubs != nil {
		t.Fatalf("on: %q %v %v", d, err, pubs)
	}
	if c := fake.Calls(); len(c) != 1 || c[0] != "GET /api/blog/posts auth" {
		t.Fatalf("on: calls %v", c)
	}
	// Without a webhook adapter a webhook blog has no publisher: Test says so.
	hook := &store.Blog{ID: 2, Name: "notes.example.test", Method: store.MethodWebhook, HasHookURL: true, HasHookKey: true}
	_, test = blogPublishers(schedOn, true, gp, nil, io.Discard)
	if _, err := test(context.Background(), hook); !errors.Is(err, store.ErrNoPublisher) {
		t.Fatalf("webhook on: %v", err)
	}
}

// stubWaker counts what the webhook adapter would send.
type stubWaker struct{ tests, wakes, checks int }

func (w *stubWaker) Test(context.Context, *store.Blog) (string, error)  { w.tests++; return "ok", nil }
func (w *stubWaker) Wake(context.Context, *store.Blog, blog.Wake) error { w.wakes++; return nil }
func (w *stubWaker) Check(context.Context, *store.Blog, string, string) (bool, string, error) {
	w.checks++
	return false, "", nil
}

// Webhook blogs: the agent is woken only with the switch on. Off, Test
// makes no call (the fake's dry run in fake mode) and no mode wires the
// adapter into the dispatcher.
func TestBlogLiveWebhookSwitch(t *testing.T) {
	hook := &store.Blog{ID: 2, Name: "notes.example.test", Method: store.MethodWebhook, HasHookURL: true, HasHookKey: true}
	w := &stubWaker{}
	for _, mode := range []string{schedOff, schedFake, schedOn} {
		pubs, test := blogPublishers(mode, false, nil, w, io.Discard)
		_, err := test(context.Background(), hook)
		if mode == schedFake {
			if err != nil {
				t.Errorf("fake: dry run %v", err)
			}
		} else if !errors.Is(err, store.ErrLiveOff) {
			t.Errorf("%q: test %v", mode, err)
		}
		if m, ok := pubs.(blog.Methods); ok && m.Hook != nil {
			t.Errorf("%q: live off but the webhook adapter is wired", mode)
		}
	}
	if w.tests+w.wakes+w.checks != 0 {
		t.Fatalf("live off reached the agent: %+v", w)
	}
	for _, mode := range []string{schedFake, schedOn} {
		pubs, test := blogPublishers(mode, true, nil, w, io.Discard)
		if m, ok := pubs.(blog.Methods); !ok || m.Hook != w || m.Webhook != nil {
			t.Errorf("%q: live on: %#v", mode, pubs)
		}
		if _, err := test(context.Background(), hook); err != nil {
			t.Errorf("%q: test %v", mode, err)
		}
	}
	if w.tests != 2 || w.wakes != 0 {
		t.Fatalf("live on: %+v", w)
	}
}
