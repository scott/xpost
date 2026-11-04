package blogtext

import (
	"strings"
	"testing"
)

const (
	em = "\u2014"
	en = "\u2013"
)

func TestLintRules(t *testing.T) {
	md := "Opening paragraph.\n\nSample sentence about the review flow " + em + " and a second clause.\n\nYears 2019" + en + "2024 in a list.\n\nWe went skiing near the Example Ridge."
	r := Rules{EnDash: Warn, Banned: Block, LeadingH1: Warn, Topics: []string{"ski", "skiing", "Example  Ridge", " "}}
	fs := Lint("A title", "One line.", []string{"docs"}, md, r)
	errs, warns := Counts(fs)
	if errs != 3 || warns != 1 {
		t.Fatalf("errors %d warnings %d: %+v", errs, warns, fs)
	}
	if fs[0].Rule != RuleEmDash || fs[0].Where != "paragraph 2" || fs[0].Hit != em || !strings.HasSuffix(fs[0].Before, "review flow ") || !strings.HasPrefix(fs[0].After, " and a second") {
		t.Fatalf("em dash finding: %+v", fs[0])
	}
	if fs[1].Rule != RuleEnDash || fs[1].Level != Warn || fs[1].Where != "paragraph 3" || !strings.Contains(fs[1].Msg, "Warning only, nothing to confirm.") {
		t.Fatalf("en dash finding: %+v", fs[1])
	}
	// "ski" is not matched inside "skiing"; "skiing" is; a multi-word topic
	// matches across any spacing and case.
	if fs[2].Rule != RuleBanned || fs[2].Hit != "skiing" || fs[3].Hit != "Example Ridge" {
		t.Fatalf("banned: %+v %+v", fs[2], fs[3])
	}
	// No findings on clean text, and an en dash can be set to block.
	if fs := Lint("Clean", "Clean.", nil, "Just text - with a hyphen.", r); len(fs) != 0 {
		t.Fatalf("clean text: %+v", fs)
	}
	r.EnDash = Block
	if e, _ := Counts(Lint("t", "d", nil, "a"+en+"b", r)); e != 1 {
		t.Fatal("en dash set to block did not block")
	}
	// An em dash in the title or description blocks too.
	if e, _ := Counts(Lint("Title "+em+" x", "d", nil, "body", DefaultRules())); e != 1 {
		t.Fatal("em dash in the title passed")
	}
}

func TestLintLeadingTitle(t *testing.T) {
	r := DefaultRules()
	fs := Lint("Example Post: notes!", "d", nil, "\n#  Example post notes  #\n\nBody.", r)
	if len(fs) != 1 || fs[0].Rule != RuleLeadingH1 || fs[0].Level != Warn {
		t.Fatalf("leading title: %+v", fs)
	}
	if fs := Lint("Example post", "d", nil, "# Another heading\n\nBody.", r); len(fs) != 0 {
		t.Fatalf("a different heading was flagged: %+v", fs)
	}
	if fs := Lint("Example post", "d", nil, "## Example post\n\nBody.", r); len(fs) != 0 {
		t.Fatalf("an h2 was flagged: %+v", fs)
	}
	r.LeadingH1 = Block
	if e, _ := Counts(Lint("T", "d", nil, "# T", r)); e != 1 {
		t.Fatal("leading title set to block did not block")
	}
}

func TestQuoteTrims(t *testing.T) {
	long := strings.Repeat("word ", 30)
	f := quote(long+em+" "+long, len(long), len(em))
	if !strings.HasPrefix(f.Before, "...") || !strings.HasSuffix(f.After, "...") || len([]rune(f.Before)) > 44 || len([]rune(f.After)) > 44 {
		t.Fatalf("quote: %q %q", f.Before, f.After)
	}
}

func TestRenderSanitizes(t *testing.T) {
	src := "Hello <script>alert(1)</script> <b onclick=x>bold</b>\n\n" +
		"<iframe src=https://example.com></iframe>\n\n" +
		"[js](javascript:alert(1)) [ok](https://example.com/a) ![img](https://example.com/i.png)\n\n" +
		"<style>body{}</style>\n\n```\ncode <x>\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |"
	out := string(Render(src))
	for _, bad := range []string{"<script", "onclick", "<iframe", "javascript:", "<style", "<img", "<b "} {
		if strings.Contains(strings.ToLower(out), bad) {
			t.Errorf("render kept %q: %s", bad, out)
		}
	}
	for _, good := range []string{`href="https://example.com/a"`, `rel="nofollow noreferrer noopener"`, "<pre><code>code &lt;x&gt;", "<table>", "<td>1</td>"} {
		if !strings.Contains(out, good) {
			t.Errorf("render lost %q: %s", good, out)
		}
	}
}

func TestWordsOpeningSlug(t *testing.T) {
	src := "## Heading\n\nFirst *paragraph* with a [link](https://example.com/very/long).\n\nSecond one."
	if n := Words(src); n != 8 {
		t.Fatalf("words: %d", n)
	}
	if o := Opening(src); o != "First paragraph with a link." {
		t.Fatalf("opening: %q", o)
	}
	for n, want := range map[int]int{0: 1, 480: 2, 590: 3, 712: 3, 864: 4, 1042: 5} {
		if got := ReadMinutes(n); got != want {
			t.Errorf("read minutes %d: %d, want %d", n, got, want)
		}
	}
	for s, ok := range map[string]bool{"sample-build-week": true, "a": true, "a1-b2": true, "": false, "-a": false, "a--b": false, "A": false, "a_b": false, strings.Repeat("a", 81): false} {
		if ValidSlug(s) != ok {
			t.Errorf("slug %q: %v", s, !ok)
		}
	}
}

func TestLintInlineImage(t *testing.T) {
	for _, md := range []string{"Intro.\n\n![Example](https://example.test/a.png)", "Intro.\n\n<img src=\"x.png\">"} {
		var hit *Finding
		for _, f := range Lint("Example title", "Sample.", nil, md, Rules{}) {
			if f.Rule == RuleImage {
				f := f
				hit = &f
			}
		}
		if hit == nil || hit.Level != Warn || hit.Where != "paragraph 2" {
			t.Fatalf("%q: %+v", md, hit)
		}
	}
	for _, f := range Lint("Example title", "Sample.", nil, "A [link](https://example.test) and ! [not an image].", Rules{}) {
		if f.Rule == RuleImage {
			t.Fatal("false image warning")
		}
	}
}

// Over 300 characters (runes, not bytes) only warns, with the exact text.
func TestLintLongDescription(t *testing.T) {
	long := func(n int, r string) string { return strings.Repeat(r, n) }
	for _, d := range []string{long(300, "a"), long(300, "\u00e9"), ""} {
		if fs := Lint("Example", d, nil, "Body.", DefaultRules()); len(fs) != 0 {
			t.Fatalf("%d runes warned: %+v", len([]rune(d)), fs)
		}
	}
	for _, d := range []string{long(301, "a"), long(301, "\u00e9"), long(1000, "a")} {
		fs := Lint("Example", d, nil, "Body.", Rules{EnDash: Block, Banned: Block, LeadingH1: Block})
		if len(fs) != 1 || fs[0].Rule != RuleLongDesc || fs[0].Level != Warn || fs[0].Blocks() || fs[0].Where != "the description" ||
			fs[0].Msg != "Long description: search results may cut it off" {
			t.Fatalf("%d runes: %+v", len([]rune(d)), fs)
		}
		if errs, warns := Counts(fs); errs != 0 || warns != 1 {
			t.Fatalf("counts %d %d", errs, warns)
		}
	}
}

// The long-description quote marks the words from the one the cut falls in.
func TestLintLongDescriptionQuote(t *testing.T) {
	d := strings.Repeat("abcd ", 60) + "sample tail words here"
	fs := Lint("Example", d, nil, "Body.", DefaultRules())
	if len(fs) != 1 || fs[0].Hit != "sample tail words here" || !strings.HasSuffix(fs[0].Before, "abcd ") || fs[0].After != "" {
		t.Fatalf("%+v", fs)
	}
	long := strings.Repeat("x", 1000)
	if fs := Lint("Example", long, nil, "Body.", DefaultRules()); len(fs) != 1 || fs[0].Hit == "" || !strings.HasSuffix(fs[0].After, "...") {
		t.Fatalf("no spaces: %+v", fs)
	}
}
