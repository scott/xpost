package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

func (s *Server) blogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /drafts/{id}/edit", s.requireOwner(s.blogEditPage))
	mux.HandleFunc("POST /drafts/{id}/edit", s.requireOwner(s.blogEditSubmit))
	mux.HandleFunc("POST /drafts/{id}/publish", s.requireOwner(s.blogPublish))
}

// canPublish reports whether this server has a blog publisher. Slice 1
// has only the fake one (local development); elsewhere Publish now is off.
func (s *Server) canPublish() bool { return s.cfg.BlogWake != nil }

const noPublisher = "Publishing blog posts is not available on this server yet."

type findingView struct {
	Sev, Label, Msg, Before, Hit, After string
}

// blogView is what blog cards, the preview and the editor show.
type blogView struct {
	B          *store.Blog
	P          *store.BlogPost
	Opening    string
	Words      string // "1,042 words"
	Mins       int
	Findings   []findingView
	NErr       int
	NWarn      int
	LintLabel  string
	LintClass  string
	LintHead   string
	Off        string // why Approve and Publish now are off
	Untested   bool
	PubOff     string // why Publish now is off when Approve is not
	PubOpen    bool   // the Publish now confirm is open
	PubHref    string
	PubClose   string
	Published  string // "7:00 AM"
	LiveAt     string
	FailedAt   string
	Blocked    bool // failed because the slug is taken by a post xpost did not create
	Live       bool // the post is published
	Imported   bool // imported from the live site: no body kept here
	Adopted    bool // a blocked draft the import matched to the live post
	ShowLint   bool // pending, approved or failed: the lint box can show
	CoverLabel string
	St         string // the status pill: posted reads "published", posting "publishing"
	Promos     []promoLine
	NApproved  int    // approved promos (held while the publish has failed)
	Hook       string // publishing through the blog's agent: where it is
}

// hookLine says where a publish through the blog's agent is.
func hookLine(p *store.BlogPublish, lease time.Time, loc *time.Location) string {
	clock := func(t time.Time) string { return t.In(loc).Format("3:04 PM") }
	pr := "Pull request #" + strconv.Itoa(p.PR)
	switch p.State {
	case store.PubWaking:
		return "Sending the post to the publishing agent."
	case store.PubSent:
		at := p.StartedAt
		if p.SentAt != nil {
			at = *p.SentAt
		}
		return "Sent to the publishing agent at " + clock(at) + ". Waiting for its report until " + clock(p.ExpiresAt) + "."
	case store.PubOpened:
		return pr + " is open. Waiting for the merge until " + clock(p.ExpiresAt) + "."
	case store.PubMerged:
		l := pr + " merged as " + p.MergeSHA[:min(7, len(p.MergeSHA))] + ". Checking the live page"
		if !lease.IsZero() {
			l += "; next check at " + clock(lease)
		}
		return l + "."
	}
	return ""
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func lintLabel(e, w int) (string, string) {
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + one + "s"
	}
	switch {
	case e > 0 && w > 0:
		return "Lint: " + plural(e, "error") + ", " + plural(w, "warning"), "bad2"
	case e > 0:
		return "Lint: " + plural(e, "error"), "bad2"
	case w > 0:
		return "Lint: " + plural(w, "warning"), "warn2"
	}
	return "Lint passed", "ok2"
}

func (s *Server) blogView(r *http.Request, d *store.Draft, loc *time.Location) *blogView {
	p := d.Post
	words := blogtext.Words(p.Markdown)
	v := &blogView{P: p, Opening: blogtext.Opening(p.Markdown), Mins: blogtext.ReadMinutes(words), Words: thousands(words) + " words"}
	if words == 1 {
		v.Words = "1 word"
	}
	b, err := s.st.BlogForAccount(r.Context(), d.Account)
	if err != nil {
		b = &store.Blog{Name: "Removed blog", Color: "bo"}
	}
	v.B = b
	v.St = d.Status
	v.Promos = s.promoLines(r, d, loc)
	for _, l := range v.Promos {
		if l.Approved {
			v.NApproved++
		}
	}
	switch d.Status {
	case store.StatusPosted:
		v.St = "published"
	case store.StatusPosting:
		v.St = "publishing"
	}
	fs := store.LintPost(b, p)
	for _, f := range fs {
		fv := findingView{Msg: f.Msg, Before: f.Before, Hit: f.Hit, After: f.After, Sev: "w", Label: "WARN"}
		if f.Blocks() {
			fv.Sev, fv.Label = "e", "ERROR"
		}
		v.Findings = append(v.Findings, fv)
	}
	v.NErr, v.NWarn = blogtext.Counts(fs)
	v.LintLabel, v.LintClass = lintLabel(v.NErr, v.NWarn)
	switch {
	case v.NErr == 1:
		v.LintHead = "Lint found 1 error. Approve and Publish now are off until it is fixed."
	case v.NErr > 1:
		v.LintHead = "Lint found " + strconv.Itoa(v.NErr) + " errors. Approve and Publish now are off until they are fixed."
	case v.NWarn == 1:
		v.LintHead = "Lint found 1 warning. Nothing is blocked."
	case v.NWarn > 1:
		v.LintHead = "Lint found " + strconv.Itoa(v.NWarn) + " warnings. Nothing is blocked."
	}
	v.ShowLint = d.Status == store.StatusPending || d.Status == store.StatusApproved || d.Status == store.StatusFailed
	switch {
	case v.NErr > 0:
		v.Off = (&store.LintError{Findings: fs}).Error()
	case !b.Ready():
		v.Off, v.Untested = b.Name+" cannot publish until its last test passes. Drafts can still be reviewed.", true
	case b.CoverAltRequired && (p.Cover == nil || strings.TrimSpace(p.Cover.Alt) == ""):
		v.Off = b.Name + " needs cover alt text. Add it in Edit."
	}
	if v.Off == "" && !s.canPublish() {
		v.PubOff = noPublisher
	}
	if p.Cover != nil {
		v.CoverLabel = p.Cover.Alt
		if v.CoverLabel == "" {
			v.CoverLabel = "Cover image"
		}
	}
	if d.Status == store.StatusPosted && p.ImportedAt != nil {
		// The site gives a day, not a time: say the day, in the blog's zone.
		v.Live, v.Imported = true, true
		v.Published = d.ScheduledAt.In(b.Location(loc)).Format("Jan 2, 2006")
	} else if d.Status == store.StatusPosted {
		v.Live, v.Adopted = true, p.AdoptedAt != nil
		v.Published = d.ScheduledAt.In(loc).Format("3:04 PM")
		if p.LiveCheckedAt != nil {
			v.LiveAt = p.LiveCheckedAt.In(loc).Format("3:04 PM")
		}
	}
	if d.Status == store.StatusFailed {
		v.FailedAt = d.UpdatedAt.In(loc).Format("3:04 PM")
		v.Blocked = strings.HasPrefix(d.Error, store.BlogSlugTakenPrefix) || strings.Contains(d.Error, store.BlogSlugTaken)
	}
	if d.Status == store.StatusPosting && b.Method == store.MethodWebhook {
		if pub, err := s.st.LatestBlogPublish(r.Context(), d.ID); err == nil && pub.InFlight() {
			v.Hook = hookLine(pub, d.LeaseUntil, loc)
		}
	}
	return v
}

// withQuery sets one query value on a same-site URL.
func withQuery(raw, k, val string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if val == "" {
		q.Del(k)
	} else {
		q.Set(k, val)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ---- preview ----

type eventView struct {
	When, Text string
	Cur        bool
}

var eventText = map[string]string{"proposed": "submitted", "approved": "approved", "publish now": "publish now", "retry publish": "retry publish",
	"posting": "publishing", "published": "published", "publish failed": "publish failed", "rejected": "rejected"}

func (s *Server) blogPreview(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft) {
	seq := s.liveSeqNow(r)
	loc := s.zone(r)
	c := s.card(r, d, s.accountMap(r), loc)
	c.CSRF, c.Ret = csrfToken(r), "/drafts/"+strconv.FormatInt(d.ID, 10)
	pq := "/queue?kind=blog&"
	if d.Status == store.StatusApproved {
		pq += "status=approved&"
	}
	c.Blog.PubHref = pq + "publish=" + strconv.FormatInt(d.ID, 10) + "#d" + strconv.FormatInt(d.ID, 10)
	evs, _ := s.st.Events(r.Context(), d.ID)
	var hist []eventView
	for _, e := range evs {
		who := e.Actor
		switch {
		case who == "owner":
			who = "you"
		case strings.HasPrefix(who, "agent:"):
			who = strings.TrimPrefix(who, "agent:")
		}
		txt := eventText[e.Kind]
		if txt == "" {
			txt = e.Kind
		}
		txt += " by " + who
		if e.Kind == "agent report" {
			// xpost writes the detail itself from checked fields (a PR
			// number and a short hex SHA), never agent text.
			txt = "Publishing agent: " + e.Detail
		}
		hist = append(hist, eventView{When: s.received(e.At, loc), Text: txt})
	}
	if len(hist) > 0 {
		hist[len(hist)-1].Cur = true
	}
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = "queue", "Preview", "/queue?kind=blog"
	p.Flash = queueFlash[r.URL.Query().Get("done")]
	p.Data = map[string]any{"Card": c, "Body": blogtext.Render(d.Post.Markdown), "History": hist, "Date": d.ScheduledAt.In(loc).Format("January 2, 2006")}
	setLive(r, p, http.StatusOK, "draft", seq)
	p.LiveDraft, p.LiveV = d.ID, d.Version
	s.render(w, http.StatusOK, "blog", "app", p)
}

// ---- editor ----

func (s *Server) blogEditPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, ok := s.blogDraft(w, r)
	if !ok {
		return
	}
	loc := s.zone(r)
	e := store.BlogEdit{Title: d.Post.Title, Description: d.Post.Description, Slug: d.Post.Slug, Tags: d.Post.Tags, Markdown: d.Post.Markdown, When: d.ScheduledAt}
	if d.Post.Cover != nil {
		e.CoverAlt = d.Post.Cover.Alt
	}
	s.showBlogEdit(w, r, u, d, e, d.Version, loc, http.StatusOK, "", r.URL.Query().Get("tab") == "preview")
}

func (s *Server) blogDraft(w http.ResponseWriter, r *http.Request) (*store.Draft, bool) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return nil, false
	}
	d, err := s.st.Draft(r.Context(), id)
	if err != nil || !d.IsBlog() {
		s.notFound(w, r)
		return nil, false
	}
	return d, true
}

func splitTags(raw string) []string {
	var out []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func (s *Server) showBlogEdit(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft, e store.BlogEdit, version int, loc *time.Location, status int, msg string, preview bool) {
	// Lint and words come from what is in the form, not what was saved.
	shown := *d
	post := *d.Post
	post.Title, post.Description, post.Tags, post.Markdown = e.Title, e.Description, e.Tags, e.Markdown
	shown.Post = &post
	bv := s.blogView(r, &shown, loc)
	editable := d.Status == store.StatusPending || d.Status == store.StatusApproved || d.Status == store.StatusFailed
	when := s.when(e.When, loc)
	src := "written by you"
	if d.TokenID != 0 {
		src = "from " + d.Source
	}
	var utcNote string
	if bv.B.Method == store.MethodAPI {
		at := e.When.In(loc)
		utcNote = loc.String() + " (" + at.Format("MST") + "). " + bv.B.Name + " date: " + e.When.UTC().Format("2006-01-02") + " (UTC day)."
		if at.Format("2006-01-02") != e.When.UTC().Format("2006-01-02") {
			utcNote += " That is the next UTC day."
		}
	}
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = "queue", "Edit blog post", "/drafts/"+strconv.FormatInt(d.ID, 10)
	p.Error = msg
	p.Data = map[string]any{"D": d, "BV": bv, "E": e, "Tags": strings.Join(e.Tags, ", "), "V": version, "When": when, "Source": src, "Editable": editable,
		"Preview": preview, "Body": blogtext.Render(e.Markdown), "UTCNote": utcNote, "ZoneName": loc.String()}
	s.render(w, status, "blogedit", "app", p)
}

func (s *Server) blogEditSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, ok := s.blogDraft(w, r)
	if !ok {
		return
	}
	loc := s.zone(r)
	// Title and description are one line; their boxes wrap, so line breaks
	// become spaces.
	line := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace
	e := store.BlogEdit{Title: line(r.PostFormValue("title")), Description: line(r.PostFormValue("description")), Slug: strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("slug")), "/"),
		Tags: splitTags(r.PostFormValue("tags")), Markdown: strings.ReplaceAll(r.PostFormValue("markdown"), "\r\n", "\n"), CoverAlt: r.PostFormValue("cover_alt")}
	when, werr := parseWhen(r.PostFormValue("date"), r.PostFormValue("time"), loc)
	e.When = when
	if werr != nil {
		e.When = d.ScheduledAt
	}
	v := formVersion(r)
	switch r.PostFormValue("op") {
	case "preview", "write":
		s.showBlogEdit(w, r, u, d, e, v, loc, http.StatusOK, "", r.PostFormValue("op") == "preview")
		return
	}
	if werr != nil {
		s.showBlogEdit(w, r, u, d, e, v, loc, http.StatusBadRequest, "Pick a date and a time.", false)
		return
	}
	err := s.st.SaveBlogPost(r.Context(), u, d.ID, v, e)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		code, msg := reviewError(err)
		if code == 0 {
			s.internalError(w, err)
			return
		}
		s.showBlogEdit(w, r, u, d, e, v, loc, code, msg, false)
		return
	}
	http.Redirect(w, r, "/queue?kind=blog&done=pending#d"+strconv.FormatInt(d.ID, 10), http.StatusSeeOther)
}

// ---- publish now ----

func (s *Server) blogPublish(w http.ResponseWriter, r *http.Request, u *store.User) {
	d, ok := s.blogDraft(w, r)
	if !ok {
		return
	}
	if !s.canPublish() {
		s.afterAction(w, r, u, &store.ConflictError{Msg: noPublisher}, "")
		return
	}
	err := s.st.PublishNow(r.Context(), u, d.ID, formVersion(r))
	if err == nil {
		s.cfg.BlogWake()
	}
	s.afterAction(w, r, u, err, "publishing")
}
