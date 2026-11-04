package auth

import (
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
)

// FailLinePrefix starts every failed sign-in or bad-token log line, so a
// fail2ban filter can match it (the deploy slice adds one):
//
//	failregex = xpost auth: login failed ip=<HOST> reason=\S+
const FailLinePrefix = "xpost auth: login failed"

var reasonRE = regexp.MustCompile(`^[a-z_]+$`)

// FailLine formats a failed sign-in line. It never includes the email or the
// password, only the client IP and a fixed reason word.
func FailLine(ip, reason string) string {
	if net.ParseIP(ip) == nil {
		ip = "invalid"
	}
	if !reasonRE.MatchString(reason) {
		reason = "other"
	}
	return fmt.Sprintf("%s ip=%s reason=%s", FailLinePrefix, ip, reason)
}

// WriteFailLine writes FailLine plus a newline to w.
func WriteFailLine(w io.Writer, ip, reason string) {
	fmt.Fprintln(w, FailLine(ip, reason))
}

// Fail2banRegexp converts a fail2ban failregex (with <HOST>) into a Go regexp,
// so tests can prove the shipped filter matches the shipped log line.
func Fail2banRegexp(failregex string) (*regexp.Regexp, error) {
	host := `(?P<host>[0-9A-Fa-f:.]+)`
	return regexp.Compile(strings.ReplaceAll(failregex, "<HOST>", host))
}
