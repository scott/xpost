package web

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/scott/xpost/internal/store"
)

// fakeDNS answers from a fixed table and counts lookups. Nothing here
// touches real DNS or dials anything outside the test.
type fakeDNS struct {
	mu   sync.Mutex
	recs map[string][]string
	hits map[string]int
}

func (f *fakeDNS) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[host]++
	var out []netip.Addr
	for _, s := range f.recs[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if out == nil {
		return nil, errors.New("no such host in the test table")
	}
	return out, nil
}

func (f *fakeDNS) count(host string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits[host] }

// Hostnames that resolve to blocked ranges are refused when dialing, even
// on a loopback base URL where the URL check alone would pass them.
func TestWebhookDestinationRefusedAtDial(t *testing.T) {
	dns := &fakeDNS{hits: map[string]int{}, recs: map[string][]string{
		"private.example.test":    {"10.20.30.40"},
		"rfc1918b.example.test":   {"172.20.1.1"},
		"metadata.example.test":   {"169.254.169.254"},
		"cgnat.example.test":      {"100.64.5.5"},
		"ula.example.test":        {"fd12:3456::1"},
		"linklocal6.example.test": {"fe80::1"},
		"mapped.example.test":     {"::ffff:10.0.0.9"},
		"multicast.example.test":  {"239.1.2.3"},
		"zero.example.test":       {"0.0.0.0"},
		"mixed.example.test":      {"203.0.113.7", "10.0.0.1"}, // one bad record refuses the host
	}}
	h := newHookRecv(t)
	e := newEnv(t, func(c *Config) { loopbackHooks(h)(c); c.WebhookLookup = dns.lookup })
	c, _ := e.setupOwner()
	tok, sec := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeDraftsRevise)
	form := "/settings/webhooks/" + strconvI(tok.ID)
	for host := range dns.recs {
		u := "https://" + host + "/hook"
		if r := c.post("/settings", form, url.Values{"op": {"save"}, "url": {u}, "key": {hookKey}}); r.status != http.StatusSeeOther {
			t.Fatalf("%s: save %d", host, r.status)
		}
		r := c.post("/settings", form, url.Values{"op": {"test"}})
		if !strings.Contains(r.loc, "done=hookrefused") {
			t.Errorf("%s: test not refused: %s", host, r.loc)
		}
		if dns.count(host) == 0 {
			t.Errorf("%s: the injected resolver was not used", host)
		}
		w, _ := e.st.AgentWebhook(context.Background(), tok.ID)
		if w.TestOK || w.TestDetail != "destination not allowed: private or reserved address" {
			t.Errorf("%s: recorded %v %q", host, w.TestOK, w.TestDetail)
		}
	}
	mustHaveRW(t, c.get("/settings?done=hookrefused").body, "Not sent: that webhook host resolves to a private or reserved address.", "destination not allowed: private or reserved address")
	// A delivery is refused the same way and leaves the draft unblocked.
	id := e.propose(sec, sm, testNow.Add(time.Hour), "Sample text.")
	r := c.post("/queue", path(id, "/rewrite"), e.ownerForm(id, url.Values{"return": {"/queue"}}))
	if r.status != http.StatusConflict || !strings.Contains(r.body, "(destination not allowed: private or reserved address). Nothing changed") {
		t.Fatalf("delivery: %d", r.status)
	}
	if r := c.post("/queue", path(id, "/approve"), e.ownerForm(id, url.Values{"return": {"/queue"}})); r.status != http.StatusSeeOther {
		t.Fatalf("approve after refused delivery: %d", r.status)
	}
	if n := len(h.hits()); n != 0 {
		t.Fatalf("receiver got %d requests", n)
	}
	logs := e.logs.String()
	for host := range dns.recs {
		if strings.Contains(logs, host) {
			t.Fatalf("logs name %s", host)
		}
	}
	for _, ip := range []string{"10.20.30.40", "169.254.169.254", "fd12:3456::1"} {
		if strings.Contains(logs, ip) {
			t.Fatalf("logs name %s", ip)
		}
	}
}

// Loopback is reachable only while xpost itself runs on a loopback base
// URL. On a public base URL a loopback webhook is refused at save and,
// if stored anyway, at dial time; so is a name that resolves to loopback.
func TestWebhookLoopbackOnlyForLoopbackBase(t *testing.T) {
	h := newHookRecv(t)
	dns := &fakeDNS{hits: map[string]int{}, recs: map[string][]string{"loop.example.test": {"127.0.0.1"}}}
	e := newEnv(t, func(c *Config) { c.WebhookHTTP = h.srv.Client(); c.WebhookLookup = dns.lookup })
	c, _ := e.setupOwner()
	tok, _ := e.token("sample-agent", store.ScopeDraftsWrite, store.ScopeDraftsRevise)
	form := "/settings/webhooks/" + strconvI(tok.ID)
	for _, u := range []string{h.srv.URL + "/hook", "https://localhost/hook", "https://[::1]/hook"} {
		r := c.post("/settings", form, url.Values{"op": {"save"}, "url": {u}, "key": {hookKey}})
		if r.status != http.StatusBadRequest || !strings.Contains(r.body, "That address is private or reserved.") {
			t.Errorf("%s: save %d", u, r.status)
		}
	}
	for _, u := range []string{h.srv.URL + "/hook", "https://loop.example.test/hook"} {
		if err := e.st.SetAgentWebhook(context.Background(), e.owner, tok.ID, u, hookKey); err != nil {
			t.Fatal(err)
		}
		if r := c.post("/settings", form, url.Values{"op": {"test"}}); !strings.Contains(r.loc, "done=hookrefused") {
			t.Errorf("%s: test %s", u, r.loc)
		}
	}
	if n := len(h.hits()); n != 0 {
		t.Fatalf("receiver got %d requests on a public base URL", n)
	}
}

func TestDestAllowed(t *testing.T) {
	pub := &Server{cfg: Config{BaseURL: "https://xpost.example.test"}}
	dev := &Server{cfg: Config{BaseURL: "http://localhost:8090"}}
	for _, c := range []struct {
		ip       string
		pub, dev bool
	}{
		{"10.0.0.1", false, false}, {"172.16.0.1", false, false}, {"172.31.255.255", false, false}, {"192.168.1.1", false, false},
		{"100.64.0.1", false, false}, {"100.127.255.254", false, false},
		{"169.254.169.254", false, false}, {"169.254.1.1", false, false}, {"fe80::1", false, false}, {"fe80::1%eth0", false, false},
		{"fc00::1", false, false}, {"fd00::1", false, false},
		{"0.0.0.0", false, false}, {"::", false, false},
		{"224.0.0.1", false, false}, {"239.255.255.250", false, false}, {"ff02::1", false, false}, {"255.255.255.255", false, false},
		{"::ffff:10.0.0.1", false, false}, {"::ffff:169.254.169.254", false, false}, {"::ffff:100.64.0.1", false, false},
		{"64:ff9b::a00:1", false, false}, {"2002:a00:1::1", false, false}, // NAT64 and 6to4 of 10.0.0.1
		{"127.0.0.1", false, true}, {"127.1.2.3", false, true}, {"::1", false, true}, {"::ffff:127.0.0.1", false, true},
		{"8.8.8.8", true, true}, {"172.32.0.1", true, true}, {"100.128.0.1", true, true}, {"2606:4700::1", true, true}, {"::ffff:8.8.8.8", true, true},
	} {
		a := netip.MustParseAddr(c.ip)
		if got := pub.destAllowed(a); got != c.pub {
			t.Errorf("public base: %s allowed=%v, want %v", c.ip, got, c.pub)
		}
		if got := dev.destAllowed(a); got != c.dev {
			t.Errorf("loopback base: %s allowed=%v, want %v", c.ip, got, c.dev)
		}
	}
	// The socket-level check sees the address actually being dialed.
	for _, addr := range []string{"10.0.0.1:443", "[::ffff:169.254.169.254]:80", "[fd00::1]:443", "127.0.0.1:443", "garbage"} {
		if err := pub.hookControl("tcp", addr, nil); !errors.Is(err, errDestRefused) {
			t.Errorf("control %s: %v", addr, err)
		}
	}
	if err := pub.hookControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("control public: %v", err)
	}
}

// No proxy setting, from the environment or a supplied client, can route
// a webhook around the dial check; redirects are never followed.
func TestWebhookClientNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example.test:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.example.test:3128")
	t.Setenv("ALL_PROXY", "socks5://proxy.example.test:1080")
	t.Setenv("NO_PROXY", "")
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	for _, base := range []*http.Client{nil, {}, proxied} {
		s := &Server{cfg: Config{BaseURL: "https://xpost.example.test", WebhookHTTP: base}}
		c := s.webhookClient()
		tr, ok := c.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil || tr.DialContext == nil || tr.DialTLSContext != nil || !tr.DisableKeepAlives || c.CheckRedirect == nil || c.Timeout != webhookTimeout {
			t.Fatalf("client not locked down: %+v", c)
		}
		if base != nil && base.Transport != nil && base.Transport.(*http.Transport).Proxy == nil {
			t.Fatal("the caller's transport was changed")
		}
	}
	// And a real delivery to the local receiver still goes straight there.
	e := newRewriteEnv(t)
	if r := e.c.post("/settings", "/settings/webhooks/"+strconvI(e.tok.ID), url.Values{"op": {"test"}}); !strings.Contains(r.loc, "done=hooktested") {
		t.Fatalf("test with proxy env set: %s", r.loc)
	}
}
