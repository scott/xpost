package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/weight"
	"github.com/scott/xpost/internal/xauth"
)

// Replies in the review UI. Approve never posts a reply: it turns on the
// hand-off. Open on X to post records the time and sends the owner's
// browser to x.com's compose page with the reply filled in; the owner
// presses Post there and marks it posted here. xpost never sees X's side,
// never reads X to check a link, and never calls the X API for a reply.

func (s *Server) replyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /drafts/{id}/open-x", s.requireOwner(s.openOnX))
	mux.HandleFunc("POST /drafts/{id}/unapprove", s.requireOwner(s.unapprove))
	mux.HandleFunc("POST /drafts/{id}/mark-posted", s.requireOwner(s.markPosted))
	mux.HandleFunc("POST /drafts/{id}/retry-load", s.requireOwner(s.retryLoad))
}

// webCfg is the X web base for links (x.com unless XPOST_X_WEB_URL says
// otherwise; x.invalid in demo mode, so example data never opens X).
func (s *Server) webCfg() *xauth.Config {
	c := &xauth.Config{WebURL: xauth.DefaultWebURL}
	if s.xc != nil && s.xc.Cfg != nil && s.xc.Cfg.WebURL != "" {
		c.WebURL = s.xc.Cfg.WebURL
	}
	if s.cfg.Demo {
		c.WebURL = "https://x.invalid"
	}
	return c
}

// allowXForms lets this page's forms land on the X web origin. Open on X
// POSTs to /drafts/{id}/open-x, which 303s to the x.com intent URL, and
// browsers apply form-action to that redirect target. Only the queue and
// a reply's own page call this; every other page keeps form-action 'self'.
func (s *Server) allowXForms(w http.ResponseWriter) {
	if o := origin(s.webCfg().WebURL); o != "" {
		w.Header().Set("Content-Security-Policy", strings.Replace(CSP, "form-action 'self'", "form-action 'self' "+o, 1))
	}
}

// replyApproved is where Approve lands for a reply: the Approved tab under
// Replies, at that card, where Open on X to post and Copy text are.
func replyApproved(id int64) string {
	return "/queue?status=approved&kind=reply&done=replyapproved#d" + strconv.FormatInt(id, 10)
}

// replyView is what a reply card and the reply editor show.
type replyView struct {
	R        *store.Reply
	Name     string // the original's author
	Handle   string
	Initials string
	Text     string // the original's text
	Body     string // the reply's text
	Rel, Abs string // "2h ago", "12:14 PM MST"
	ViewURL  string // the original on X
	Example  bool
	Tried    string // when the original was last read
	// The reply text.
	Count     int
	Over      int
	Keep      string // the text up to the limit, and what runs past it
	Extra     string
	Lint      []blogtext.Finding
	Warns     []string
	IntentURL string
	Approved  string // "2:20 PM MST"
	Opened    string // "2:31 PM MST"
	OpenedDay string // "Nov 2"
	NotMarked bool
	Posted    string // posted time
	// Retry load (queue cards, pending, original unavailable).
	RetryForm bool
	RetryID   int64
	RetryV    int
	CSRF, Ret string
	// The Mark posted dialog.
	MarkOpen  bool
	MarkHref  string
	MarkClose string
	MarkErr   string
	MarkVal   string
}

func initials(name, handle string) string {
	var out []rune
	for _, f := range strings.Fields(name) {
		r := []rune(f)
		if len(r) > 0 && len(out) < 2 {
			out = append(out, r[0])
		}
	}
	if len(out) == 0 {
		r := []rune(handle)
		if len(r) > 0 {
			out = append(out, r[0])
		}
	}
	return strings.ToUpper(string(out))
}

func relAgo(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// clock is a time with its zone, in the owner's zone.
func clock(t time.Time, loc *time.Location) string { return t.In(loc).Format("3:04 PM MST") }

// splitAt cuts text where its X weight passes the limit.
func splitAt(text string, limit int) (string, string) {
	if weight.Count(text).Weighted <= limit {
		return text, ""
	}
	rs := []rune(text)
	lo, hi := 0, len(rs)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if weight.Count(string(rs[:mid])).Weighted <= limit {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(rs[:lo]), string(rs[lo:])
}

var limitedWho = map[string]string{"mentionedUsers": "accounts they mention", "following": "accounts they follow", "subscribers": "their subscribers", "verified": "verified accounts"}

// replyWarnings are the reply's warnings: they never block Approve.
func (s *Server) replyWarnings(r *http.Request, d *store.Draft) []string {
	rp := d.Reply
	out := []string{}
	if rp.Limited() {
		who := limitedWho[rp.ReplySettings]
		author := "The author"
		if rp.AuthorHandle != "" {
			author = "@" + rp.AuthorHandle
		}
		out = append(out, fmt.Sprintf("%s limits replies to %s. X may not let @%s post this reply. You can still approve and try on X.", author, who, d.Account))
	}
	if n := s.st.ReplyDuplicates(r.Context(), d); n > 0 {
		other := "another reply"
		if n > 1 {
			other = strconv.Itoa(n) + " other replies"
		}
		out = append(out, fmt.Sprintf("@%s already has %s to this post in xpost. You can still approve this one.", d.Account, other))
	}
	return out
}

func (s *Server) replyView(r *http.Request, d *store.Draft, loc *time.Location) *replyView {
	rp := d.Reply
	now := s.st.Now()
	text := ""
	if len(d.Parts) > 0 {
		text = d.Parts[0].Text
	}
	c := weight.Count(text)
	v := &replyView{R: rp, Name: rp.AuthorName, Handle: rp.AuthorHandle, Initials: initials(rp.AuthorName, rp.AuthorHandle), Text: rp.Text, Example: s.cfg.Demo,
		Tried: clock(rp.TriedAt, loc), Count: c.Weighted, Over: c.Over(), Lint: s.st.LintReply(r.Context(), d.Account, text), Warns: s.replyWarnings(r, d)}
	if v.Name == "" {
		v.Name = "@" + rp.AuthorHandle
	}
	v.Body = text
	v.Keep, v.Extra = splitAt(text, weight.Limit)
	if rp.CreatedAt != nil {
		v.Rel, v.Abs = relAgo(*rp.CreatedAt, now), clock(*rp.CreatedAt, loc)
	}
	if rp.Available {
		v.ViewURL = s.xURL(rp.AuthorHandle, rp.InReplyTo, s.cfg.Demo)
	}
	if d.Status == store.StatusApproved {
		v.IntentURL = s.webCfg().IntentURL(text, rp.InReplyTo)
		if d.ApprovedAt != nil {
			v.Approved = clock(*d.ApprovedAt, loc)
		}
		if rp.OpenedAt != nil {
			v.Opened = clock(*rp.OpenedAt, loc)
			v.OpenedDay = rp.OpenedAt.In(loc).Format("Jan 2")
			v.NotMarked = rp.NotMarked(now)
		}
	}
	if d.Status == store.StatusPosted {
		v.Posted = d.UpdatedAt.In(loc).Format("Jan 2, 3:04 PM MST")
	}
	return v
}

// replyCard fills a reply's queue card.
func (s *Server) replyCard(r *http.Request, c *cardView, loc *time.Location) {
	d := c.D
	c.Reply = s.replyView(r, d, loc)
	c.Conflicts = nil
	c.Cost = "$0" // never sent through the API
	if d.Status == store.StatusPending {
		c.Blocked = s.st.ReplyBlock(r.Context(), d)
	} else {
		c.Blocked = ""
	}
	if d.Status == store.StatusPosted {
		c.Posted = nil
	}
}

// markState carries a refused Mark posted back into the queue's dialog.
type markState struct {
	ID       int64
	Err, Val string
}

type markKey struct{}

// replyQueue sets the Mark posted dialog on the queue's reply cards.
func (s *Server) replyQueue(r *http.Request, c *cardView) {
	if c.Reply == nil {
		return
	}
	id := strconv.FormatInt(c.D.ID, 10)
	c.Reply.RetryForm = c.D.Status == store.StatusPending && !c.Reply.R.Available
	c.Reply.RetryID, c.Reply.RetryV, c.Reply.CSRF, c.Reply.Ret = c.D.ID, c.D.Version, c.CSRF, c.Ret
	c.Reply.MarkHref = withQuery(withQuery(r.URL.RequestURI(), "done", ""), "mark", id) + "#d" + id
	c.Reply.MarkClose = withQuery(withQuery(r.URL.RequestURI(), "done", ""), "mark", "") + "#d" + id
	if r.URL.Query().Get("mark") == id && c.D.Status == store.StatusApproved && c.Reply.R.OpenedAt != nil {
		c.Reply.MarkOpen = true
		c.Ret = c.Reply.MarkClose
		if m, ok := r.Context().Value(markKey{}).(*markState); ok && m.ID == c.D.ID {
			c.Reply.MarkErr, c.Reply.MarkVal = m.Err, m.Val
		}
	}
}

func (s *Server) openOnX(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	// xpost.js opens the x.com tab itself and records the open with
	// fetch (X-Xpost-Open: 1); it gets JSON back and re-renders the card.
	// Without JS the form posts here and follows the redirect.
	js := r.Header.Get(openXHeader) == "1"
	if d, err := s.st.Draft(ctx, id); err != nil || !d.IsReply() {
		if js {
			jsonError(w, http.StatusNotFound, "not found")
			return
		}
		s.notFound(w, r)
		return
	}
	if err := s.st.OpenedOnX(ctx, u, id, formVersion(r)); err != nil {
		if js {
			code, msg := reviewError(err)
			if errors.Is(err, store.ErrNotFound) {
				code, msg = http.StatusNotFound, "not found"
			} else if code == 0 {
				s.log.Printf("open-x: %v", err)
				code, msg = http.StatusInternalServerError, "Something went wrong. Reload and try again."
			}
			jsonError(w, code, msg)
			return
		}
		s.afterAction(w, r, u, err, "")
		return
	}
	d, err := s.st.Draft(ctx, id)
	if err != nil || !d.IsReply() || len(d.Parts) == 0 || d.Reply.OpenedAt == nil {
		s.notFound(w, r)
		return
	}
	intent := s.webCfg().IntentURL(d.Parts[0].Text, d.Reply.InReplyTo)
	if js {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": d.ID, "version": d.Version,
			"opened_at": d.Reply.OpenedAt.UTC().Format(time.RFC3339), "intent_url": intent})
		return
	}
	// The owner's browser goes to x.com; xpost sends nothing to X.
	http.Redirect(w, r, intent, http.StatusSeeOther)
}

// openXHeader marks xpost.js's Open on X fetch.
const openXHeader = "X-Xpost-Open"

func (s *Server) unapprove(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	s.afterAction(w, r, u, s.st.Unapprove(r.Context(), u, id, formVersion(r)), "unapproved")
}

func (s *Server) retryLoad(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	loaded, err := s.st.RetryReplyLoad(r.Context(), u, id, formVersion(r))
	flash := "notloaded"
	if loaded {
		flash = "loaded"
	}
	s.afterAction(w, r, u, err, flash)
}

// replyLink checks the optional link to a posted reply: an x.com post by
// the replying account. It checks the format only; xpost never reads X.
// It returns the link normalized, or a message for the owner.
func (s *Server) replyLink(raw, account string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	example := "Paste a link like x.com/" + account + "/status/1234567890, or clear the field."
	h, id, err := xauth.ParseStatusLink(raw)
	switch {
	case errors.Is(err, xauth.ErrProfileLink):
		return "", "This is a profile link, not a post. " + example
	case errors.Is(err, xauth.ErrNotXLink):
		return "", "This is not an x.com link. " + example
	case err != nil:
		return "", "This is not a link to a post. " + example
	case !strings.EqualFold(h, account):
		return "", "This post is from @" + h + ", not @" + account + ". Paste the link to your reply, or clear the field."
	}
	return s.webCfg().StatusURL(account, id), ""
}

func (s *Server) markPosted(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	d, err := s.st.Draft(ctx, id)
	if err != nil || !d.IsReply() {
		s.notFound(w, r)
		return
	}
	raw := r.PostFormValue("link")
	link, msg := s.replyLink(raw, d.Account)
	if msg != "" {
		to, _ := url.Parse(back(r))
		r2 := r.Clone(context.WithValue(ctx, markKey{}, &markState{ID: id, Err: msg, Val: raw}))
		q := to.Query()
		q.Set("mark", strconv.FormatInt(id, 10))
		r2.URL.RawQuery = q.Encode()
		s.showQueue(w, r2, u, http.StatusBadRequest, "", "")
		return
	}
	s.afterAction(w, r, u, s.st.MarkReplyPosted(ctx, u, id, formVersion(r), link), "replyposted")
}

// ---- the reply editor ----

type replyCheck struct {
	Level string // "ok", "warn" or "bad"
	Msg   string
}

func (s *Server) showReplyEdit(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft, text string, status int, msg string, stale bool) {
	s.allowXForms(w)
	seq := s.liveSeqNow(r)
	ctx := r.Context()
	loc := s.zone(r)
	view := *d
	view.Parts = []store.Part{{Text: text}}
	rv := s.replyView(r, &view, loc)
	var checks []replyCheck
	switch {
	case !d.Reply.Available:
		checks = append(checks, replyCheck{"bad", "The original post didn't load. Approve stays off until it does."})
	default:
		checks = append(checks, replyCheck{"ok", "Original post loaded."})
	}
	if rv.Over > 0 {
		checks = append(checks, replyCheck{"bad", fmt.Sprintf("%d over the 280 limit. Shorten by %d to approve.", rv.Over, rv.Over)})
	} else {
		checks = append(checks, replyCheck{"ok", fmt.Sprintf("%d / 280.", rv.Count)})
	}
	nLint := 0
	for _, f := range rv.Lint {
		lv := "warn"
		if f.Blocks() {
			lv = "bad"
		}
		checks = append(checks, replyCheck{lv, f.Msg})
		nLint++
	}
	if nLint == 0 {
		checks = append(checks, replyCheck{"ok", "No banned topics or dashes."})
	}
	for _, m := range rv.Warns {
		checks = append(checks, replyCheck{"warn", m})
	}
	events, _ := s.st.Events(ctx, d.ID)
	type evView struct{ Who, What, At string }
	var evs []evView
	for _, e := range events {
		who := e.Actor
		if a, ok := strings.CutPrefix(who, "agent:"); ok {
			who = a
		}
		what := strings.ToUpper(e.Kind[:1]) + e.Kind[1:]
		if e.Detail != "" && (e.Kind == "rejected" || e.Kind == "posted by you on X") {
			what += ": " + e.Detail
		}
		evs = append(evs, evView{Who: who, What: what, At: e.At.In(loc).Format("Jan 2, 3:04 PM")})
	}
	c := weight.Count(text)
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = "queue", "Edit reply", "/queue"
	p.Error = msg
	editable := d.Status == store.StatusPending || d.Status == store.StatusApproved
	p.Data = map[string]any{"D": d, "Acct": s.accountMap(r)[d.Account], "R": rv, "Text": text, "Count": c.Weighted, "Over": c.Over(), "URLs": c.URLs,
		"Ring": min(100, c.Weighted*100/weight.Limit), "Limit": weight.Limit, "Checks": checks, "Events": evs, "Editable": editable,
		"Approved": d.Status == store.StatusApproved, "Deletable": d.Status != store.StatusPosting && d.Status != store.StatusPosted, "V": d.Version}
	if v := formVersion(r); r.Method == http.MethodPost && v > 0 && !stale {
		p.Data.(map[string]any)["V"] = v
	}
	setLive(r, p, status, "draft", seq)
	p.LiveDraft, p.LiveV = d.ID, d.Version
	s.render(w, status, "replyedit", "app", p)
}

func (s *Server) replyEditSubmit(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft) {
	ctx := r.Context()
	text := strings.ReplaceAll(r.PostFormValue("text_0"), "\r\n", "\n")
	if d.Status != store.StatusPending && d.Status != store.StatusApproved {
		s.showReplyEdit(w, r, u, d, d.Parts[0].Text, http.StatusConflict, "This reply can't be edited in its current state.", false)
		return
	}
	if r.PostFormValue("op") != "save" {
		s.showReplyEdit(w, r, u, d, text, http.StatusOK, "", false) // recount
		return
	}
	err := s.st.Save(ctx, u, d.ID, formVersion(r), store.Edit{Parts: []store.PartIn{{Text: text}}, When: d.ScheduledAt}, false)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		code, m := reviewError(err)
		if code == 0 {
			s.internalError(w, err)
			return
		}
		stale := false
		if fresh, e2 := s.st.Draft(ctx, d.ID); e2 == nil && errors.Is(err, store.ErrStale) {
			d, text, stale = fresh, fresh.Parts[0].Text, true
			m = "This reply changed since you opened it. This is the current version; make your edit again."
		}
		s.showReplyEdit(w, r, u, d, text, code, m, stale)
		return
	}
	flash := "saved"
	if d.Status == store.StatusApproved {
		flash = "pending"
	}
	http.Redirect(w, r, withFlash("/queue", flash), http.StatusSeeOther)
}
