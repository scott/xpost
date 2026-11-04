package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"syscall"
)

// Rewrite webhook destinations are checked where the connection is made,
// not only when the URL is saved: the host is resolved here, every address
// must be allowed (one blocked record refuses the host, so a DNS answer
// that mixes a public and a private address can't slip through), the
// connection goes to that checked IP literal, and the dialer's Control
// hook checks the socket's actual address once more. A DNS-rebinding host
// therefore can't reach a private or reserved address. No proxy is ever
// used: a proxy would dial on our behalf, past these checks.

// errDestRefused is a destination in a blocked range. Its text names no
// address or host.
var errDestRefused = errors.New("destination not allowed: private or reserved address")

// blockedNets are refused for every webhook, whatever the base URL.
// Loopback is refused too unless xpost runs on a loopback base URL (dev).
var blockedNets = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",                                     // unspecified, "this network"
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // private (RFC 1918)
		"100.64.0.0/10",  // CGNAT
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local, including 169.254.169.254 (cloud metadata)
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved, including 255.255.255.255
		"::/96",          // unspecified, loopback and IPv4-compatible
		"fc00::/7",       // unique local (ULA)
		"fe80::/10",      // link-local
		"ff00::/8",       // multicast
		"2001::/32",      // Teredo (hides an IPv4 address)
		"64:ff9b:1::/48", // local-use NAT64
		"100::/64",       // discard-only
		"2001:db8::/32",  // documentation
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// destAllowed reports whether a webhook may connect to a.
func (s *Server) destAllowed(a netip.Addr) bool {
	a = a.WithZone("").Unmap() // IPv4-mapped IPv6 is checked as IPv4
	if !a.IsValid() {
		return false
	}
	b := a.As16()
	switch {
	case nat64.Contains(a): // well-known NAT64 embeds an IPv4 address
		return s.destAllowed(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case sixToFour.Contains(a): // 6to4 embeds one too
		return s.destAllowed(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	case a.IsLoopback():
		return s.loopbackBase()
	}
	for _, p := range blockedNets {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func (s *Server) hookLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if s.cfg.WebhookLookup != nil {
		return s.cfg.WebhookLookup(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// hookControl runs on the socket just before connect, with the address
// actually being dialed.
func (s *Server) hookControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !s.destAllowed(ap.Addr()) {
		return errDestRefused
	}
	return nil
}

// hookDial resolves, checks every address, then dials a checked IP.
func (s *Server) hookDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else if ips, err = s.hookLookup(ctx, host); err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("host not found")
	}
	for _, ip := range ips {
		if !s.destAllowed(ip) {
			return nil, errDestRefused
		}
	}
	d := &net.Dialer{Timeout: webhookTimeout, Control: s.hookControl}
	var last error
	for _, ip := range ips {
		c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.WithZone("").Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		last = err
	}
	return nil, last
}

// webhookClient is the only client rewrite webhooks use: guarded dialer,
// no proxy, no redirects, no keep-alive, an overall timeout. A test client
// (Config.WebhookHTTP) contributes only its TLS settings.
func (s *Server) webhookClient() *http.Client {
	var t *http.Transport
	if b := s.cfg.WebhookHTTP; b != nil {
		if bt, ok := b.Transport.(*http.Transport); ok {
			t = bt.Clone()
		}
	}
	if t == nil {
		t = &http.Transport{TLSHandshakeTimeout: webhookTimeout, ResponseHeaderTimeout: webhookTimeout}
	}
	t.Proxy = nil // never HTTP_PROXY / HTTPS_PROXY / ALL_PROXY
	t.OnProxyConnectResponse = nil
	t.DialContext = s.hookDial
	t.DialTLSContext = nil // TLS runs over the guarded DialContext
	t.Dial, t.DialTLS = nil, nil
	t.DisableKeepAlives = true
	return &http.Client{Transport: t, Timeout: webhookTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// BlogHTTP is the client blog publishers use: the same guarded client as
// rewrite webhooks (checked dialer, no proxy, no redirects, no keep-alive,
// a timeout). Each call returns a fresh client.
func (s *Server) BlogHTTP() *http.Client { return s.webhookClient() }

// errNotImportRead is a request the import-only client refused.
var errNotImportRead = errors.New("not an import read")

// importOnly lets through only the requests allow approves (the import's
// GETs), and refuses everything else before any connection is made.
type importOnly struct {
	next  http.RoundTripper
	allow func(*http.Request) bool
}

func (t importOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.allow == nil || !t.allow(r) {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, errNotImportRead
	}
	return t.next.RoundTrip(r)
}

// BlogImportHTTP is the client the import's readers use: the same guarded
// client as BlogHTTP, wrapped so that only Config.ImportAllow's requests
// (the import's GETs) can leave. With no ImportAllow, nothing can.
func (s *Server) BlogImportHTTP() *http.Client {
	c := s.webhookClient()
	c.Transport = importOnly{next: c.Transport, allow: s.cfg.ImportAllow}
	return c
}
