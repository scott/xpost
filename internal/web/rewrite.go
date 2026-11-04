package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Rewrite (owner side): send a pending agent draft back to the agent that
// wrote it, cancel the request, and the per-agent webhook settings.

func (s *Server) rewriteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /drafts/{id}/rewrite", s.requireOwner(s.rewriteSend))
	mux.HandleFunc("POST /drafts/{id}/rewrite/cancel", s.requireOwner(s.rewriteCancel))
	mux.HandleFunc("POST /settings/webhooks/{id}", s.requireOwner(s.webhookSubmit))
}

// ---- agents ----

// agentView is the agent that wrote a draft: its configured token name, an
// initial and a color class (rounded squares, so they never read as X
// accounts).
type agentView struct {
	ID       int64
	Name     string
	Initial  string
	Color    string
	Hook     *store.Webhook // nil: no webhook saved
	Revise   bool           // token has drafts:revise
	Live     bool
	Settings string // link to this agent's row in Settings
}

func agentInitial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return "?"
}

func (s *Server) agentViews(r *http.Request) map[int64]*agentView {
	ctx := r.Context()
	hooks, _ := s.st.AgentWebhooks(ctx)
	rows, err := s.st.DB.QueryContext(ctx, `SELECT id FROM agent_tokens`)
	if err != nil {
		return map[int64]*agentView{}
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := map[int64]*agentView{}
	for _, id := range ids {
		t, err := s.st.AgentTokenByID(ctx, id)
		if err != nil {
			continue
		}
		out[id] = &agentView{ID: id, Name: t.Name, Initial: agentInitial(t.Name), Color: "ag" + strconv.FormatInt((id-1)%5+1, 10),
			Hook: hooks[id], Revise: t.Has(store.ScopeDraftsRevise), Live: t.RevokedAt == nil, Settings: "/settings#agent-" + strconv.FormatInt(id, 10)}
	}
	return out
}

// ---- card ----

// rwView is what a pending agent card shows about its latest request.
type rwView struct {
	State     string // waiting, late (no rewrite yet), revised, unchanged
	Note      string
	Sent      string // "9:14 AM"
	Mins      int    // minutes since sent
	RevisedAt string
	Prev      *prevView
}

type prevView struct {
	From     string // "sample-agent-1, 8:37 AM"
	PrevTime string
	RevTime  string
	Prev     []fullPart
	Rev      []fullPart
}

// rewriteCard adds the Rewrite button state and the latest request to a
// pending agent card.
func (s *Server) rewriteCard(r *http.Request, c *cardView, agents map[int64]*agentView, loc *time.Location) {
	d := c.D
	if d.TokenID == 0 {
		return
	}
	a := agents[d.TokenID]
	if a == nil {
		return
	}
	c.Agent = a
	if d.Status != store.StatusPending {
		return
	}
	c.CanRewrite = true
	switch {
	case !a.Live:
		c.RwOff = "The token that wrote this draft is revoked"
	case !a.Revise:
		c.RwOff = a.Name + "'s token lacks drafts:revise"
		c.RwHint = "Rewrite is off for " + a.Name + ": its token lacks the drafts:revise scope. Make a new token with drafts:revise on the Log page."
	case a.Hook == nil:
		c.RwOff = "No rewrite webhook for " + a.Name
		c.RwHint, c.RwLink = "Rewrite is off for "+a.Name+".", "Set up a rewrite webhook for "+a.Name+" in Settings"
	case a.Hook.TestedAt != nil && !a.Hook.TestOK:
		c.RwWarn = a.Name + "'s rewrite webhook failed its last test (" + a.Hook.TestDetail + "). A rewrite may not arrive."
	}
	q, err := s.st.LatestRewrite(r.Context(), d.ID)
	if err == nil && !q.Dismissed {
		now := s.st.Now()
		v := &rwView{Note: q.Note, Sent: q.SentAt.In(loc).Format("3:04 PM"), Mins: int(now.Sub(q.SentAt).Minutes())}
		switch {
		case q.Waiting(now):
			v.State = "waiting"
			c.Blocked = "Waiting on " + a.Name + " for a rewrite."
			c.RwOff = "Waiting on " + a.Name
		case q.TimedOut(now):
			v.State = "late"
		case q.State == store.RewriteRevised && q.RevisedVersion == d.Version:
			v.State = "revised"
			if q.AnsweredAt != nil {
				v.RevisedAt = q.AnsweredAt.In(loc).Format("3:04 PM")
			}
			v.Prev = s.prevVersion(r, c, loc)
		case q.State == store.RewriteUnchanged:
			v.State = "unchanged"
		}
		if v.State != "" {
			c.RW = v
		}
	}
	if c.RwOff == "" && r.URL.Query().Get("rewrite") == strconv.FormatInt(d.ID, 10) {
		c.RwOpen = true
	}
	qv := r.URL.Query()
	qv.Del("done")
	qv.Set("rewrite", strconv.FormatInt(d.ID, 10))
	c.RwHref = "/queue?" + qv.Encode() + "#d" + strconv.FormatInt(d.ID, 10)
	qv.Del("rewrite")
	c.RwClose = "/queue"
	if e := qv.Encode(); e != "" {
		c.RwClose += "?" + e
	}
	c.RwClose += "#d" + strconv.FormatInt(d.ID, 10)
}

// prevVersion diffs the latest replaced version against the draft and
// marks additions in the card's own text.
func (s *Server) prevVersion(r *http.Request, c *cardView, loc *time.Location) *prevView {
	revs, err := s.st.Revisions(r.Context(), c.D.ID)
	if err != nil || len(revs) == 0 {
		return nil
	}
	old := revs[0]
	from := old.Author
	pv := &prevView{From: from + ", " + old.AuthoredAt.In(loc).Format("3:04 PM"), PrevTime: old.AuthoredAt.In(loc).Format("3:04 PM"), RevTime: old.CreatedAt.In(loc).Format("3:04 PM")}
	if c.D.IsBlog() {
		if old.Post == nil || c.D.Post == nil {
			return nil
		}
		blogPrev(pv, old.Post, c.D.Post)
		return pv
	}
	n, m := len(old.Parts), len(c.D.Parts)
	for i := 0; i < n || i < m; i++ {
		var a, b string
		if i < n {
			a = old.Parts[i].Text
		}
		if i < m {
			b = c.D.Parts[i].Text
		}
		da, db := wordDiff(a, b)
		if i < n {
			pv.Prev = append(pv.Prev, fullPart{Label: partLabel(i, n), Text: a, Segs: da})
		}
		if i < m {
			pv.Rev = append(pv.Rev, fullPart{Label: partLabel(i, m), Text: b, Segs: db})
			if i < len(c.Full) {
				c.Full[i].Segs = db
			}
		}
	}
	return pv
}

// blogPrev lists the blog fields the rewrite changed, each marked word by
// word: title, description, tags, then the markdown body.
func blogPrev(pv *prevView, old *store.RevisionPost, cur *store.BlogPost) {
	for _, f := range [][3]string{
		{"Title", old.Title, cur.Title},
		{"Description", old.Description, cur.Description},
		{"Tags", strings.Join(old.Tags, ", "), strings.Join(cur.Tags, ", ")},
		{"Body", old.Markdown, cur.Markdown},
	} {
		if f[1] == f[2] {
			continue
		}
		da, db := wordDiff(f[1], f[2])
		pv.Prev = append(pv.Prev, fullPart{Label: f[0], Text: f[1], Segs: da})
		pv.Rev = append(pv.Rev, fullPart{Label: f[0], Text: f[2], Segs: db})
	}
}

func partLabel(i, n int) string {
	if n < 2 {
		return ""
	}
	return strconv.Itoa(i+1) + "/" + strconv.Itoa(n)
}

// ---- word diff ----

// seg is a run of text: K is "" (same), "add" or "del".
type seg struct{ T, K string }

var tokRe = regexp.MustCompile(`\s+|[^\s]+`)

// wordDiff marks the words of a that b removed and the words of b that
// a lacked (LCS over words and whitespace runs).
func wordDiff(a, b string) (as, bs []seg) {
	ta, tb := tokRe.FindAllString(a, -1), tokRe.FindAllString(b, -1)
	if len(ta)*len(tb) > 1_000_000 { // far beyond 280 weighted chars; whole-part fallback
		return []seg{{a, "del"}}, []seg{{b, "add"}}
	}
	n, m := len(ta), len(tb)
	l := make([][]int, n+1)
	for i := range l {
		l[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if ta[i] == tb[j] {
				l[i][j] = l[i+1][j+1] + 1
			} else if l[i+1][j] >= l[i][j+1] {
				l[i][j] = l[i+1][j]
			} else {
				l[i][j] = l[i][j+1]
			}
		}
	}
	type tk struct {
		t    string
		kind string
	}
	var ka, kb []tk
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case ta[i] == tb[j]:
			ka, kb = append(ka, tk{ta[i], ""}), append(kb, tk{tb[j], ""})
			i, j = i+1, j+1
		case l[i+1][j] >= l[i][j+1]:
			ka = append(ka, tk{ta[i], "del"})
			i++
		default:
			kb = append(kb, tk{tb[j], "add"})
			j++
		}
	}
	for ; i < n; i++ {
		ka = append(ka, tk{ta[i], "del"})
	}
	for ; j < m; j++ {
		kb = append(kb, tk{tb[j], "add"})
	}
	merge := func(ts []tk, kind string) []seg {
		// A same-whitespace run between two changed words joins them.
		for x := 1; x+1 < len(ts); x++ {
			if ts[x].kind == "" && strings.TrimSpace(ts[x].t) == "" && !strings.Contains(ts[x].t, "\n") && ts[x-1].kind == kind && ts[x+1].kind == kind {
				ts[x].kind = kind
			}
		}
		var out []seg
		for _, t := range ts {
			if len(out) > 0 && out[len(out)-1].K == t.kind {
				out[len(out)-1].T += t.t
				continue
			}
			out = append(out, seg{t.t, t.kind})
		}
		return out
	}
	return merge(ka, "del"), merge(kb, "add")
}

// ---- handlers ----

// rewriteReturn is the queue URL to go back to, without ?rewrite= (the
// note box closes once sent).
func rewriteReturn(r *http.Request) string {
	u, err := url.Parse(back(r))
	if err != nil {
		return "/queue"
	}
	q := u.Query()
	q.Del("rewrite")
	q.Del("done")
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) rewriteSend(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	note := r.PostFormValue("note")
	if r.PostFormValue("retry") != "" {
		if q, err := s.st.LatestRewrite(ctx, id); err == nil {
			note = q.Note // Retry re-sends the same note
		}
	}
	req, err := s.st.StartRewrite(ctx, u, id, formVersion(r), note)
	if err != nil {
		s.rewriteFail(w, r, u, err)
		return
	}
	d, err := s.st.Draft(ctx, id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	res := s.deliver(ctx, req.TokenID, hookRewrite, s.rewritePayload(ctx, d, req))
	if !res.OK {
		s.st.FailRewrite(ctx, req.ID, res.Detail)
		name := "the agent"
		if t, err := s.st.AgentTokenByID(ctx, req.TokenID); err == nil {
			name = t.Name
		}
		s.rewriteFail(w, r, u, &store.ConflictError{Msg: "Couldn't deliver the request to " + name + "'s rewrite webhook (" + res.Detail + "). Nothing changed; the draft is still Pending."})
		return
	}
	http.Redirect(w, r, withFlash(rewriteReturn(r), "rewrite"), http.StatusSeeOther)
}

func (s *Server) rewriteFail(w http.ResponseWriter, r *http.Request, u *store.User, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	code, msg := reviewError(err)
	if code == 0 {
		s.internalError(w, err)
		return
	}
	to, _ := url.Parse(rewriteReturn(r))
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = to.RawQuery
	s.showQueue(w, r2, u, code, msg, "")
}

func (s *Server) rewriteCancel(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.st.CancelRewrite(r.Context(), u, id); err != nil {
		s.rewriteFail(w, r, u, err)
		return
	}
	http.Redirect(w, r, withFlash(rewriteReturn(r), "rwcancel"), http.StatusSeeOther)
}

// ---- settings ----

// hookRow is one agent's Rewrite webhooks row in Settings. The key is
// never part of it: only its last 4 characters.
type hookRow struct {
	A      *agentView
	Scopes string
	Status string // none, set, ok, bad
	Chip   string
	Note   string
	URL    string // what to show in the URL field
	Err    string
}

func (s *Server) hookRows(r *http.Request, loc *time.Location, errFor int64, errMsg string) []hookRow {
	toks, _ := s.st.AgentTokens(r.Context())
	agents := s.agentViews(r)
	var out []hookRow
	for i := len(toks) - 1; i >= 0; i-- { // oldest first
		t := toks[i]
		a := agents[t.ID]
		if a == nil {
			continue
		}
		h := hookRow{A: a, Scopes: strings.Join(t.Scopes, ", "), Status: "none", Chip: "Not set"}
		if a.Hook != nil {
			h.URL = a.Hook.URL
			switch {
			case a.Hook.TestedAt == nil:
				h.Status, h.Chip, h.Note = "set", "Set · not tested", "Saved. Send a test to check that "+a.Name+" answers."
			case a.Hook.TestOK:
				h.Status, h.Chip = "ok", "Test passed "+a.Hook.TestedAt.In(loc).Format("3:04 PM")
				h.Note = fmt.Sprintf("%s answered the test in %.1f s.", a.Name, float64(a.Hook.TestMs)/1000)
			default:
				h.Status, h.Chip = "bad", "Test failed"
				if a.Hook.TestDetail != "" {
					h.Chip += " · " + a.Hook.TestDetail
				}
				h.Note = testFailNote(a.Name, a.Hook.TestDetail)
			}
		}
		if errFor == t.ID {
			h.Err = errMsg
			if r.Method == http.MethodPost {
				h.URL = r.PostFormValue("url")
			}
		}
		out = append(out, h)
	}
	return out
}

// testFailNote explains a failed test, with the HTTP status when there was
// one; 401 and 403 point at the Authorization header.
func testFailNote(name, detail string) string {
	n := name + " did not accept the test (" + detail + "). Rewrite stays on, but requests may not arrive."
	if detail == "HTTP 401" || detail == "HTTP 403" {
		n += " The receiver refused it: check the Authorization header."
	}
	return n
}

func (s *Server) webhookSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	t, err := s.st.AgentTokenByID(ctx, id)
	if err != nil || t.RevokedAt != nil {
		s.notFound(w, r)
		return
	}
	fail := func(msg string) { s.showSettingsHook(w, r, u, http.StatusBadRequest, id, msg) }
	switch r.PostFormValue("op") {
	case "remove":
		if err := s.st.RemoveAgentWebhook(ctx, u, id); err != nil {
			s.internalError(w, err)
			return
		}
		http.Redirect(w, r, "/settings?done=hookremoved#agent-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	case "authclear":
		if err := s.st.ClearAgentWebhookAuth(ctx, u, id); err != nil {
			s.internalError(w, err)
			return
		}
		http.Redirect(w, r, "/settings?done=hookauthcleared#agent-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	case "test":
		if _, err := s.st.AgentWebhook(ctx, id); err != nil {
			fail("Save a URL and key first.")
			return
		}
		res := s.deliver(ctx, id, hookTest, webhookTestBody{Type: hookTest, At: rfc(s.st.Now())})
		if err := s.st.RecordWebhookTest(ctx, u, id, res.OK, res.Detail, int(res.Took.Milliseconds())); err != nil {
			s.internalError(w, err)
			return
		}
		done := "hooktested"
		switch {
		case res.Refused:
			done = "hookrefused"
		case !res.OK:
			done = "hookfailed&agent=" + strconv.FormatInt(id, 10) // a failed test only warns
		}
		http.Redirect(w, r, "/settings?done="+done+"#agent-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	inputErr := func(err error) {
		var ie *store.InputError
		if errors.As(err, &ie) {
			fail(strings.ToUpper(ie.Msg[:1]) + ie.Msg[1:] + ".")
			return
		}
		s.internalError(w, err)
	}
	raw := strings.TrimSpace(r.PostFormValue("url"))
	if err := s.checkWebhookURL(raw); err != nil {
		fail(err.Error())
		return
	}
	// Check the Authorization value before saving anything; empty keeps
	// the saved one (Clear removes it).
	auth, err := store.CleanWebhookAuth(r.PostFormValue("auth"))
	if err != nil {
		inputErr(err)
		return
	}
	if err := s.st.SetAgentWebhook(ctx, u, id, raw, r.PostFormValue("key")); err != nil {
		inputErr(err)
		return
	}
	if auth != "" {
		if err := s.st.SetAgentWebhookAuth(ctx, u, id, auth); err != nil {
			inputErr(err)
			return
		}
	}
	http.Redirect(w, r, "/settings?done=hooksaved#agent-"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
