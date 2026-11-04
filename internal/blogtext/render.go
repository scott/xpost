package blogtext

import (
	"bytes"
	"html"
	"html/template"
	"math"
	"regexp"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Markdown is untrusted agent input. goldmark runs with raw HTML off (it
// writes "<!-- raw HTML omitted -->" instead) and drops dangerous link
// schemes; bluemonday then allows only plain article elements, so nothing
// the post says can add a script, a style, a form, an event handler or a
// frame. Inline images are not rendered in this slice.
var (
	md = goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
	)
	policy = func() *bluemonday.Policy {
		p := bluemonday.NewPolicy()
		p.AllowElements("p", "br", "hr", "h2", "h3", "h4", "h5", "h6", "blockquote", "ul", "ol", "li", "pre", "code", "em", "strong", "del", "sub", "sup",
			"table", "thead", "tbody", "tr", "th", "td")
		p.AllowAttrs("start").Matching(regexp.MustCompile(`^[0-9]{1,6}$`)).OnElements("ol")
		p.AllowAttrs("href").OnElements("a")
		p.AllowURLSchemes("http", "https", "mailto")
		p.RequireParseableURLs(true)
		p.RequireNoFollowOnLinks(true)
		p.RequireNoReferrerOnLinks(true)
		p.AddTargetBlankToFullyQualifiedLinks(true)
		// A leading "# Title" stays visible (the lint flags it).
		p.AllowElements("h1")
		return p
	}()
)

// Render turns Markdown into sanitized HTML for the preview.
func Render(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	out := policy.SanitizeBytes(buf.Bytes())
	return template.HTML(out) // sanitized above
}

var (
	blockRE = regexp.MustCompile(`(?i)</?(p|h[1-6]|li|ul|ol|blockquote|pre|tr|td|th|table|br|hr)\b[^>]*>`)
	tagRE   = regexp.MustCompile(`<[^>]*>`)
	firstP  = regexp.MustCompile(`(?s)<p>(.*?)</p>`)
	blankRE = regexp.MustCompile(`\s+`)
)

func plain(h string) string {
	return strings.TrimSpace(blankRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(blockRE.ReplaceAllString(h, " "), "")), " "))
}

// Words counts the words a reader sees (Markdown syntax and URLs in link
// targets are not words).
func Words(src string) int {
	var buf bytes.Buffer
	if md.Convert([]byte(src), &buf) != nil {
		return len(strings.Fields(src))
	}
	return len(strings.Fields(plain(buf.String())))
}

// ReadMinutes is the read time shown on cards: words / 240, rounded up,
// at least 1.
func ReadMinutes(words int) int {
	return max(1, int(math.Ceil(float64(words)/240)))
}

// Opening is the first paragraph as plain text, for the card.
func Opening(src string) string {
	var buf bytes.Buffer
	if md.Convert([]byte(src), &buf) != nil {
		return ""
	}
	if m := firstP.FindStringSubmatch(buf.String()); m != nil {
		return plain(m[1])
	}
	return ""
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// MaxSlug bounds a slug.
const MaxSlug = 80

// ValidSlug reports whether s is a slug: lowercase letters, digits and
// single hyphens, at most MaxSlug characters.
func ValidSlug(s string) bool { return len(s) <= MaxSlug && slugRE.MatchString(s) }
