package web

import (
	"regexp"
	"strings"
	"testing"
)

// phoneCSS returns the body of the phone media block in the shipped CSS.
func phoneCSS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/xpost.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	i := strings.Index(css, "@media (max-width:760px){")
	if i < 0 {
		t.Fatal("no phone media block")
	}
	return css[i:]
}

// lastRule returns the declarations of the last rule for selector in css.
func lastRule(css, selector string) string {
	re := regexp.MustCompile(`(?m)(^|\})\s*` + regexp.QuoteMeta(selector) + `\{([^}]*)\}`)
	m := re.FindAllStringSubmatch(css, -1)
	if m == nil {
		return ""
	}
	return m[len(m)-1][2]
}

// Layout rule: nothing scrolls sideways on a phone. The account chips wrap
// (they used to scroll), and no phone rule scrolls horizontally.
func TestPhoneChipsWrapAndNothingScrollsSideways(t *testing.T) {
	css := phoneCSS(t)
	chips := lastRule(css, ".achips")
	if !strings.Contains(chips, "flex-wrap:wrap") {
		t.Fatalf(".achips on phone does not wrap: %q", chips)
	}
	for _, bad := range []string{"overflow-x:auto", "overflow-x:scroll", "overflow:auto", "overflow:scroll", "nowrap"} {
		if strings.Contains(chips, bad) {
			t.Errorf(".achips on phone has %s", bad)
		}
	}
	if regexp.MustCompile(`overflow-x:(auto|scroll)`).MatchString(css) {
		t.Error("a phone rule scrolls sideways")
	}
	// Grid tracks that hold long text must be able to shrink.
	for _, sel := range []string{".pdays", ".ptlh,.ptb"} {
		if r := lastRule(css, sel); !strings.Contains(r, "minmax(0,1fr)") {
			t.Errorf("%s on phone can grow past the screen: %q", sel, r)
		}
	}
}

// The queue renders the account chips inside the container the CSS wraps.
func TestQueueChipsUseWrappingContainer(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	body := c.get("/queue").body
	if !regexp.MustCompile(`<nav class="[^"]*\bachips\b[^"]*" aria-label="Account">`).MatchString(body) {
		t.Fatal("account chips are not in the .achips container")
	}
}
