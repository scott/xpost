package web

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	formRE     = regexp.MustCompile(`(?s)<form\b.*?</form>`)
	textareaRE = regexp.MustCompile(`<textarea\b[^>]*>`)
)

// Every form with an editable textarea names one primary submit button,
// so Cmd+Enter (Ctrl+Enter) has exactly one thing to press.
func checkPrimaries(t *testing.T, name, page string) int {
	t.Helper()
	n := 0
	for _, f := range formRE.FindAllString(page, -1) {
		editable := false
		for _, ta := range textareaRE.FindAllString(f, -1) {
			if !strings.Contains(ta, " readonly") && !strings.Contains(ta, " disabled") {
				editable = true
			}
		}
		if !editable {
			continue
		}
		n++
		if c := strings.Count(f, "data-primary-submit"); c != 1 {
			t.Errorf("%s: a form with a textarea has %d primary submit buttons:\n%.300s", name, c, f)
		}
	}
	return n
}

func TestCmdEnterPrimarySubmit(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	_, sec := e.token("sample-agent")
	e.readyBlog("example.test")
	xid := e.propose(sec, sm, testNow.Add(2*time.Hour), "Example post", "Example part two")
	rid := e.proposeReply(sec, replyBody(sm, "Example reply: small releases.", nil)).ID
	r := e.api("POST", "/api/v1/drafts", sec, blogBody("example.test", "sample-kbd", "Example opening."))
	if r.status != http.StatusCreated {
		t.Fatalf("blog: %d %s", r.status, r.body)
	}
	bid := decodeBlog(t, r).ID
	pages := []struct {
		path string
		hint string // the hint the page shows next to its primary button
		min  int    // forms with a textarea at least
	}{
		{"/compose", `data-kbd-hint="to save as draft"`, 1},
		{"/compose?format=thread", `data-kbd-hint="to save as draft"`, 1},
		{"/drafts/" + strconvI(xid), `data-kbd-hint="to save"`, 1},
		{"/drafts/" + strconvI(rid), `data-kbd-hint="to save"`, 1},
		{path(bid, "/edit"), `data-kbd-hint="to save"`, 1},
		{"/queue", "", 3}, // the reject reasons of the X post, reply and blog post
		{"/queue?kind=blog", "", 1},
	}
	for _, p := range pages {
		pg := c.get(p.path)
		if pg.status != http.StatusOK {
			t.Fatalf("%s: %d", p.path, pg.status)
		}
		if n := checkPrimaries(t, p.path, pg.body); n < p.min {
			t.Errorf("%s: %d forms with a textarea, want at least %d", p.path, n, p.min)
		}
		if p.hint != "" {
			mustHave(t, pg.body, p.hint, ` hidden>Ctrl+Enter `)
		}
	}
	// The primary is the non-committal choice where there are several.
	mustHave(t, c.get("/compose").body, `data-primary-submit name="op" value="draft"`)
	mustHave(t, c.get("/drafts/"+strconvI(xid)).body, `data-primary-submit name="op" value="save"`)
	mustLack(t, c.get("/drafts/"+strconvI(xid)).body, `data-primary-submit name="op" value="approve"`)
}

func TestCmdEnterRewriteNote(t *testing.T) {
	e := newRewriteEnv(t)
	id := e.propose(e.sec, sm, testNow.Add(2*time.Hour), "Sample first part.")
	open := e.c.get("/queue?rewrite=" + strconvI(id)).body
	if checkPrimaries(t, "rewrite note", open) < 2 {
		t.Error("the rewrite note and the reject reason should both count")
	}
	mustHave(t, open, `data-kbd-hint="to send"`, `<button class="btn pri" data-primary-submit>`)
}

// Every template with an editable textarea carries a primary submit, so a
// new textarea can't ship without one.
func TestCmdEnterEveryTextareaTemplate(t *testing.T) {
	files, _ := filepath.Glob("templates/*.html")
	seen := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, ta := range textareaRE.FindAllString(s, -1) {
			if strings.Contains(ta, `class="vh"`) { // the hidden Copy text source
				continue
			}
			seen++
			if !strings.Contains(s, "data-primary-submit") {
				t.Errorf("%s has a textarea but no data-primary-submit", f)
			}
		}
	}
	if seen < 9 {
		t.Errorf("only %d textareas found", seen)
	}
}

// The handler lives in the static script, served under the strict CSP.
func TestCmdEnterScriptUnderCSP(t *testing.T) {
	e := newEnv(t, nil)
	c, _ := e.setupOwner()
	r := c.get("/static/xpost.js")
	if r.status != http.StatusOK || r.h.Get("Content-Security-Policy") != CSP {
		t.Fatalf("xpost.js: %d CSP %q", r.status, r.h.Get("Content-Security-Policy"))
	}
	mustHave(t, r.body, "[data-primary-submit]", "e.isComposing", "e.keyCode === 229", "requestSubmit(b)", "data-kbd-sent", "e.metaKey : e.ctrlKey", "[data-kbd-hint]")
	pg := c.get("/compose").body
	mustLack(t, pg, "<script>", "onkeydown", "onclick=", ` style="`)
	mustHave(t, pg, `<script src="/static/xpost.js`)
}
