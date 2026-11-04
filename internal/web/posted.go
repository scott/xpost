package web

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/xauth"
)

// EnvXstatsURL is the optional base of the owner's xstats dashboard. When it
// is set, posted drafts link to <base>/#post/<x_post_id>. No default.
const EnvXstatsURL = "XPOST_XSTATS_URL"

// ParseXstatsURL checks XPOST_XSTATS_URL: empty (links off), or an https
// URL (http only on a loopback host, for local tests) with a host, no
// credentials, query or fragment. Trailing slashes are trimmed. A bad value
// is an error, so serve refuses to start rather than run with a link the
// owner thinks is on.
func ParseXstatsURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%s: %s (want e.g. https://stats.example.com)", EnvXstatsURL, why)
	}
	if strings.ContainsAny(v, " \t\r\n\"'<>\\`") || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return bad("contains spaces, quotes or control characters")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return bad("not an absolute URL")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return bad("must be https (http only on localhost)")
	}
	if u.User != nil {
		return bad("must not contain a user name or password")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(v, "#") {
		return bad("must not have a query or fragment")
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// X post ids are decimal; handles are 1 to 15 letters, digits or _.
var handleRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

func validPostID(id string) bool {
	return id != "" && len(id) <= 30 && strings.Trim(id, "0123456789") == ""
}

// postedLink is one part of a posted draft on the Posted tab.
type postedLink struct {
	Label    string // "Part 2"; "" for a single post
	XURL     string // the live post; "" when no usable id was recorded
	StatsURL string // xstats deep link; "" when XPOST_XSTATS_URL is unset
}

// xURL builds a part's public link from the stored post id and handle.
// Demo rows link to x.invalid like their stored URLs. A handle X would not
// issue falls back to the handle-free /i/status/<id> form.
func (s *Server) xURL(handle, id string, example bool) string {
	if !validPostID(id) {
		return ""
	}
	c := &xauth.Config{WebURL: xauth.DefaultWebURL}
	if s.xc != nil && s.xc.Cfg != nil && s.xc.Cfg.WebURL != "" {
		c.WebURL = s.xc.Cfg.WebURL
	}
	if example {
		c.WebURL = "https://x.invalid"
	}
	if !handleRE.MatchString(handle) {
		handle = "i"
	}
	return c.StatusURL(handle, id)
}

// statsURL is the xstats deep link for a post id (its route only accepts
// digits), or "" when the base is not configured.
func (s *Server) statsURL(id string) string {
	if s.cfg.XstatsURL == "" || !validPostID(id) {
		return ""
	}
	return s.cfg.XstatsURL + "/#post/" + url.PathEscape(id)
}

// postedLinks lists every part of a posted draft with its links, and when
// the draft finished posting.
func (s *Server) postedLinks(ctx context.Context, d *store.Draft) ([]postedLink, time.Time) {
	rows, at, err := s.st.PostedParts(ctx, d.ID)
	if err != nil || at.IsZero() {
		at = d.UpdatedAt
	}
	byPart := map[int]*store.LogEntry{}
	for _, e := range rows {
		if byPart[e.Part] == nil {
			byPart[e.Part] = e
		}
	}
	n := len(d.Parts)
	if n == 0 {
		n = len(byPart)
	}
	var out []postedLink
	for i := 1; i <= n; i++ {
		l := postedLink{}
		if n > 1 {
			l.Label = fmt.Sprintf("Part %d", i)
		}
		if e := byPart[i]; e != nil {
			l.XURL = s.xURL(d.Account, e.PostID, e.Example)
			if l.XURL != "" {
				l.StatsURL = s.statsURL(e.PostID)
			}
		}
		out = append(out, l)
	}
	return out, at
}
