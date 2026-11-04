// Package weight counts post length the way X does (twitter-text v3
// weighting), erring on the side of counting too much so a draft that
// passes here is never too long for X.
//
// Rules: code points in the Latin and general punctuation ranges weigh 1;
// everything else (CJK, emoji, most other scripts) weighs 2. Joiners and
// variation selectors weigh 0. A URL with a scheme and a dotted host counts
// as 23 (X's t.co length). A bare domain-looking word counts as the larger
// of its own length and 23, since X may or may not turn it into a link.
package weight

import (
	"regexp"
	"strings"
)

// Limit is X's weighted length limit for one post.
const Limit = 280

// URLLength is what X counts for any link it shortens.
const URLLength = 23

var (
	schemeURL = regexp.MustCompile(`(?i)\bhttps?://[^\s]+`)
	bareURL   = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?:/[^\s]*)?`)
	dottedTLD = regexp.MustCompile(`(?i)^https?://(?:[^/\s@]+@)?(?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?(?:[/?#]|$)`)
)

// Result is a weighted count.
type Result struct {
	Weighted int // weighted length, compare with Limit
	URLs     int // links counted as 23
}

// Over reports how far over the limit the text is (0 if within).
func (r Result) Over() int {
	if r.Weighted > Limit {
		return r.Weighted - Limit
	}
	return 0
}

// BlogURL is xpost's placeholder for a promoted blog post's link. It
// counts as a link (URLLength); the real URL replaces it when the post goes
// out.
const BlogURL = "{{blog_url}}"

// Count returns the weighted length of text.
func Count(text string) Result {
	var res Result
	if n := strings.Count(text, BlogURL); n > 0 {
		res.Weighted += n * URLLength // each leaves a space, taken off below
		res.URLs += n
		text = strings.ReplaceAll(text, BlogURL, " ")
	}
	rest := schemeURL.ReplaceAllStringFunc(text, func(u string) string {
		trimmed := strings.TrimRight(u, ".,;:!?)]}'\"")
		res.Weighted += chars(u[len(trimmed):]) // trailing punctuation is text
		u = trimmed
		if dottedTLD.MatchString(u) {
			res.Weighted += URLLength
		} else {
			res.Weighted += max(chars(u), URLLength)
		}
		res.URLs++
		return " "
	})
	rest = bareURL.ReplaceAllStringFunc(rest, func(u string) string {
		res.Weighted += max(chars(u), URLLength)
		res.URLs++
		return " "
	})
	// Each replaced URL left one space behind; it is not part of the text.
	res.Weighted += chars(rest) - res.URLs
	return res
}

// HasURL reports whether text contains a link-like word.
func HasURL(text string) bool { return Count(text).URLs > 0 }

func chars(s string) int {
	n := 0
	for _, c := range s {
		n += runeWeight(c)
	}
	return n
}

func runeWeight(c rune) int {
	switch {
	case c <= 4351, c >= 8192 && c <= 8205 && c != 0x200D, c >= 8208 && c <= 8223, c >= 8242 && c <= 8247:
		return 1
	case c == 0x200D, c >= 0xFE00 && c <= 0xFE0F:
		return 0
	}
	return 2
}
