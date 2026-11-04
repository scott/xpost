package store

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Test results are shown in Settings and kept in the database, so they
// never carry a URL or a secret, even when a publisher's error echoes one.

var (
	scrubURL    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.\-]*://\S+`)
	scrubPath   = regexp.MustCompile(`(?i)\b[a-z0-9\-]+(?:\.[a-z0-9\-]+)+(?::\d+)?/\S*`)
	scrubBearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+\S+`)
	scrubKV     = regexp.MustCompile(`(?i)\b(api[_\-]?key|key|token|secret|password|passwd|pwd|auth|authorization|signature|sig|access[_\-]?token)(\s*[:=]\s*)(?:(?:bearer|basic)\s+)?(?:"[^"]*"|'[^']*'|\S+)`)
	scrubLong   = regexp.MustCompile(`[A-Za-z0-9_\-+/=.]{24,}`)
	scrubSpace  = regexp.MustCompile(`\s+`)
)

// ScrubDetail removes URLs, credentials and token-like strings from a
// test result, plus every value in secrets, and clips it to 200 bytes.
func ScrubDetail(detail string, secrets ...string) string {
	detail = scrubURL.ReplaceAllString(detail, "[link hidden]")
	detail = scrubPath.ReplaceAllString(detail, "[link hidden]")
	for _, v := range secrets {
		if v = strings.TrimSpace(v); len(v) >= 4 {
			detail = strings.ReplaceAll(detail, v, "[hidden]")
		}
	}
	detail = scrubKV.ReplaceAllString(detail, "$1$2[hidden]")
	detail = scrubBearer.ReplaceAllString(detail, "$1 [hidden]")
	detail = scrubLong.ReplaceAllString(detail, "[hidden]")
	detail = strings.TrimSpace(scrubSpace.ReplaceAllString(detail, " "))
	if len(detail) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(detail[cut]) {
			cut--
		}
		detail = detail[:cut]
	}
	return detail
}

// CleanBlogTestDetail scrubs a blog's test result, including the blog's
// own sealed values.
func (s *Store) CleanBlogTestDetail(ctx context.Context, id int64, detail string) string {
	var secrets []string
	for f := range blogSecretCols {
		if v, err := s.BlogSecret(ctx, id, f); err == nil && v != "" {
			secrets = append(secrets, v)
			if _, after, ok := strings.Cut(v, " "); ok { // "Bearer <token>"
				secrets = append(secrets, after)
			}
		}
	}
	return ScrubDetail(detail, secrets...)
}
