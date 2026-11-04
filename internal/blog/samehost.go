package blog

import (
	"net/url"
	"strings"
)

// SameHost compares two URLs' hosts conservatively: the host name without
// ASCII case, and the port, with https's default 443 when none is given.
// An empty host, a non-ASCII host (Unicode case folding would match
// different names, e.g. KELVIN SIGN and k) or a nil URL never matches.
// Both blog adapters use it before a live check.
func SameHost(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if strings.EqualFold(u.Scheme, "https") {
			return "443"
		}
		return ""
	}
	ha, hb := a.Hostname(), b.Hostname()
	return ha != "" && ascii(ha) && ascii(hb) && strings.EqualFold(ha, hb) && port(a) == port(b)
}

func ascii(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
