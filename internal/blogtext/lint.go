// Package blogtext checks and renders blog post text: the content lint
// that runs on agent intake and on Approve, the sanitized Markdown preview,
// word counts and the card's opening lines. It has no I/O.
package blogtext

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Level is what a lint rule does when it finds something.
type Level string

// Levels. Block stops Approve and Publish now; Warn only shows.
const (
	Block Level = "block"
	Warn  Level = "warn"
)

// ValidLevel reports whether l is a level.
func ValidLevel(l string) bool { return l == string(Block) || l == string(Warn) }

// Rules are one blog's lint settings. The em dash rule is not here: an em
// dash always blocks.
type Rules struct {
	EnDash    Level
	Banned    Level
	LeadingH1 Level
	Topics    []string // banned topics, matched as whole words, any case
}

// DefaultRules are a new blog's settings.
func DefaultRules() Rules { return Rules{EnDash: Warn, Banned: Block, LeadingH1: Warn} }

// Rule names.
const (
	RuleEmDash    = "em_dash"
	RuleEnDash    = "en_dash"
	RuleBanned    = "banned_topic"
	RuleLeadingH1 = "leading_h1"
	RuleImage     = "inline_image"     // always a warning: images inside the post are not published
	RuleLongDesc  = "long_description" // always a warning: over LongDescription characters
)

// LongDescription is where the long-description warning starts: search
// results tend to cut a description off past about this many characters.
// It only warns; the hard limit is store.MaxDescription.
const LongDescription = 300

// LongDescriptionMsg is the long-description warning's text.
const LongDescriptionMsg = "Long description: search results may cut it off"

const (
	emDash = "\u2014"
	enDash = "\u2013"
)

// Finding is one lint result. Where names the place ("the title", "the
// description", "paragraph 3", "the tags"); Before, Hit and After quote the
// passage around the match (Hit is the matched text itself).
type Finding struct {
	Rule   string `json:"rule"`
	Level  Level  `json:"level"`
	Where  string `json:"where"`
	Msg    string `json:"message"`
	Before string `json:"-"`
	Hit    string `json:"-"`
	After  string `json:"-"`
}

// Blocks reports whether the finding stops Approve.
func (f Finding) Blocks() bool { return f.Level == Block }

// Counts tallies errors (blocking) and warnings.
func Counts(fs []Finding) (errs, warns int) {
	for _, f := range fs {
		if f.Blocks() {
			errs++
		} else {
			warns++
		}
	}
	return
}

// Paragraphs splits Markdown into its blank-line separated blocks, as the
// lint numbers them (1-based in messages).
func Paragraphs(md string) []string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	var out []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
			cur = nil
		}
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}

type place struct {
	where string
	text  string
}

// Lint checks a post against the rules. Findings come in a stable order:
// by rule (em dash, en dash, banned topics, inline image, leading title,
// long description), then by place.
func Lint(title, description string, tags []string, md string, r Rules) []Finding {
	if r.EnDash == "" {
		r.EnDash = Warn
	}
	if r.Banned == "" {
		r.Banned = Block
	}
	if r.LeadingH1 == "" {
		r.LeadingH1 = Warn
	}
	places := []place{{"the title", title}, {"the description", description}}
	for i, p := range Paragraphs(md) {
		places = append(places, place{fmt.Sprintf("paragraph %d", i+1), p})
	}
	if len(tags) > 0 {
		places = append(places, place{"the tags", strings.Join(tags, ", ")})
	}
	var out []Finding
	for _, p := range places {
		if i := strings.Index(p.text, emDash); i >= 0 {
			f := quote(p.text, i, len(emDash))
			f.Rule, f.Level, f.Where = RuleEmDash, Block, p.where
			f.Msg = "Em dash in " + p.where + ". Use a comma, colon or period."
			out = append(out, f)
		}
	}
	for _, p := range places {
		if i := strings.Index(p.text, enDash); i >= 0 {
			f := quote(p.text, i, len(enDash))
			f.Rule, f.Level, f.Where = RuleEnDash, r.EnDash, p.where
			if r.EnDash == Warn {
				f.Msg = "En dash in " + p.where + ". Warning only, nothing to confirm. A hyphen or \"to\" reads better."
			} else {
				f.Msg = "En dash in " + p.where + ". A hyphen or \"to\" reads better."
			}
			out = append(out, f)
		}
	}
	for _, topic := range r.Topics {
		re := topicRE(topic)
		if re == nil {
			continue
		}
		for _, p := range places {
			loc := findTopic(re, p.text)
			if loc == nil {
				continue
			}
			f := quote(p.text, loc[0], loc[1]-loc[0])
			f.Rule, f.Level, f.Where = RuleBanned, r.Banned, p.where
			f.Msg = fmt.Sprintf("Banned topic %q in %s.", strings.TrimSpace(topic), p.where)
			if r.Banned == Block {
				f.Msg += " Remove it to approve or publish."
			} else {
				f.Msg += " Warning only, nothing to confirm."
			}
			out = append(out, f)
		}
	}
	for i, p := range Paragraphs(md) {
		if m := imageRE.FindStringIndex(p); m != nil {
			f := quote(p, m[0], m[1]-m[0])
			f.Rule, f.Level, f.Where = RuleImage, Warn, fmt.Sprintf("paragraph %d", i+1)
			f.Msg = "Image in " + f.Where + ". Images inside the post are not published. Use the cover instead. Warning only, nothing to confirm."
			out = append(out, f)
			break
		}
	}
	if h, ok := leadingH1(md); ok && norm(h) != "" && norm(h) == norm(title) {
		f := Finding{Rule: RuleLeadingH1, Level: r.LeadingH1, Where: "paragraph 1", Hit: "# " + h}
		f.Msg = "The post starts with a # title that repeats the title. Remove it; the title field is used."
		out = append(out, f)
	}
	if r := []rune(description); len(r) > LongDescription {
		// The quote marks the text past the cut, from the start of the
		// word the cut falls in, so it reads as what may be lost.
		i := len(string(r[:LongDescription]))
		if j := strings.LastIndexByte(description[:i], ' '); j >= 0 && i-j <= 40 {
			i = j + 1
		}
		n := len(description) - i
		if rest := []rune(description[i:]); len(rest) > 40 {
			n = len(string(rest[:40]))
			if j := strings.LastIndexByte(description[i:i+n], ' '); j > 0 {
				n = j
			}
		}
		f := quote(description, i, n)
		f.Rule, f.Level, f.Where, f.Msg = RuleLongDesc, Warn, "the description", LongDescriptionMsg
		out = append(out, f)
	}
	return out
}

// imageRE finds a Markdown image or an <img> tag.
var imageRE = regexp.MustCompile(`!\[[^\]\n]*\]\(|(?i)<img\b`)

func topicRE(topic string) *regexp.Regexp {
	t := strings.Join(strings.Fields(topic), " ")
	if t == "" {
		return nil
	}
	parts := strings.Split(regexp.QuoteMeta(t), " ")
	re, err := regexp.Compile(`(?i)(?:^|[^\pL\pN])(` + strings.Join(parts, `\s+`) + `)(?:$|[^\pL\pN])`)
	if err != nil {
		return nil
	}
	return re
}

// findTopic returns the byte range of the topic itself (not the boundary
// characters around it), or nil.
func findTopic(re *regexp.Regexp, s string) []int {
	m := re.FindStringSubmatchIndex(s)
	if m == nil {
		return nil
	}
	return m[2:4]
}

var spaces = regexp.MustCompile(`\s+`)

// quote cuts the passage around text[i:i+n] for the lint box: up to 40
// characters each side (whitespace collapsed), cut at a word where it can,
// with "..." where it was cut.
func quote(text string, i, n int) Finding {
	before := spaces.ReplaceAllString(text[:i], " ")
	after := spaces.ReplaceAllString(text[i+n:], " ")
	const keep = 40
	if r := []rune(before); len(r) > keep {
		b := string(r[len(r)-keep:])
		if j := strings.IndexByte(b, ' '); j >= 0 && j < keep/2 {
			b = b[j+1:]
		}
		before = "..." + b
	} else {
		before = strings.TrimLeft(before, " ")
	}
	if r := []rune(after); len(r) > keep {
		a := string(r[:keep])
		if j := strings.LastIndexByte(a, ' '); j > keep/2 {
			a = a[:j]
		}
		after = a + "..."
	} else {
		after = strings.TrimRight(after, " ")
	}
	return Finding{Before: before, Hit: text[i : i+n], After: after}
}

var h1RE = regexp.MustCompile(`^#[ \t]+(.+?)[ \t]*#*[ \t]*$`)

// leadingH1 returns the text of a "# Heading" on the first non-blank line.
func leadingH1(md string) (string, bool) {
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := h1RE.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			return "", false
		}
		return m[1], true
	}
	return "", false
}

func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
