package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
)

// Settings > Blogs: add, edit, test and remove blogs. Sealed values are
// never rendered: only whether each is set.

func (s *Server) blogSettingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/blogs", s.requireOwner(s.blogSettingsPage))
	mux.HandleFunc("GET /settings/blogs/new", s.requireOwner(s.blogNewPage))
	mux.HandleFunc("POST /settings/blogs", s.requireOwner(s.blogCreate))
	mux.HandleFunc("POST /settings/blogs/{id}", s.requireOwner(s.blogSubmit))
}

// blogSet is one blog in Settings > Blogs.
type blogSet struct {
	B        *store.Blog
	Open     bool
	Dot      string // ok, bad, untested
	DotLabel string
	Sub      string
	Test     string // the test result line
	TestCls  string // ok, bad, neutral
	Err      string
	Note     string               // a neutral notice: Test made no call (live publishing off)
	Busy     *store.BlogBusyError // the Remove blocked dialog is open
	F        blogForm
}

type blogForm struct {
	ID      int64
	New     bool
	Method  string
	Name    string
	SiteURL string
	APIBase string
	Clock   string
	Promo   string
	Topics  []string
	EnDash  string
	Banned  string
	H1      string
	CoverAl bool
	Sealed  []sealedField
	XAccts  []store.Account
	Zone    string // xpost's zone, for the hint
	BZone   string // the blog's own zone, "" = xpost's
	Zones   []string
	Tok     int64      // the Publishing token (webhook blogs)
	Tokens  []tokenOpt // active tokens with blogs:publish
}

// tokenOpt is one Publishing token choice: name and masked secret only.
type tokenOpt struct {
	ID    int64
	Label string
}

// sealedField is one sealed connection value: set or not, never shown.
type sealedField struct {
	Field, Label, Method, Help, Placeholder string
	Set, Optional                           bool
}

func sealedFields(b *store.Blog) []sealedField {
	set := map[string]bool{}
	if b != nil {
		set = map[string]bool{store.BlogAPIToken: b.HasAPIToken, store.BlogHookURL: b.HasHookURL, store.BlogHookKey: b.HasHookKey, store.BlogHookAuth: b.HasHookAuth}
	}
	return []sealedField{
		{Field: store.BlogAPIToken, Label: "API token", Method: store.MethodAPI, Help: "Sealed. The value is never shown after saving.", Placeholder: "Paste the API token", Set: set[store.BlogAPIToken]},
		{Field: store.BlogHookURL, Label: "Publish webhook URL", Method: store.MethodWebhook, Help: "Sealed. The value is never shown after saving.", Placeholder: "https://", Set: set[store.BlogHookURL]},
		{Field: store.BlogHookKey, Label: "Sender key", Method: store.MethodWebhook, Help: "Sealed. Signs every publish call. Never shown after saving.", Placeholder: "16 or more characters", Set: set[store.BlogHookKey]},
		{Field: store.BlogHookAuth, Label: "Authorization header", Method: store.MethodWebhook, Help: "Sealed. Sent as is with every publish call, for receivers that require one.", Placeholder: "Optional, e.g. Bearer ...", Set: set[store.BlogHookAuth], Optional: true},
	}
}

// formFor fills the form from a saved blog, or from what was typed.
func formFor(b *store.Blog, in *store.BlogIn, x []store.Account, zone string, toks []tokenOpt) blogForm {
	f := blogForm{Method: store.MethodWebhook, Clock: store.DefaultPubClock, EnDash: string(blogtext.Warn), Banned: string(blogtext.Block), H1: string(blogtext.Warn), XAccts: x, Zone: zone, Sealed: sealedFields(b),
		Zones: commonZones, Tokens: toks}
	if b != nil {
		f.ID, f.Method, f.Name, f.SiteURL, f.APIBase, f.Clock, f.Promo = b.ID, b.Method, b.Name, b.SiteURL, b.APIBase, b.PublishClock, b.PromoAccount
		f.BZone, f.Tok = b.Zone, b.PublishTokenID
		f.Topics, f.EnDash, f.Banned, f.H1, f.CoverAl = b.Rules.Topics, string(b.Rules.EnDash), string(b.Rules.Banned), string(b.Rules.LeadingH1), b.CoverAltRequired
	} else {
		f.New = true
	}
	if in != nil {
		f.Name, f.SiteURL, f.APIBase, f.Promo, f.Topics, f.CoverAl = in.Name, in.SiteURL, in.APIBase, in.PromoAccount, in.Topics, in.CoverAltRequired
		f.BZone, f.Tok = in.Zone, in.PublishTokenID
		if in.Method != "" {
			f.Method = in.Method
		}
		if in.PublishClock != "" {
			f.Clock = in.PublishClock
		}
		for _, l := range []struct {
			dst *string
			v   blogtext.Level
		}{{&f.EnDash, in.EnDash}, {&f.Banned, in.Banned}, {&f.H1, in.LeadingH1}} {
			if l.v != "" {
				*l.dst = string(l.v)
			}
		}
	}
	return f
}

func methodLabel(m string) string {
	if m == store.MethodAPI {
		return "Publisher API"
	}
	return "Webhook to agent"
}

var blogFlash = map[string]string{"added": "Blog added. It is Untested: test it before it can publish.", "saved": "Blog saved.", "topic": "Topic added. Blog saved.", "tested": "Test passed. Nothing was published.",
	"testfailed": "Test failed. Nothing was published.", "removed": "Blog removed. Its sealed credentials are deleted; published history stays.", "cleared": "Cleared. The blog is back to Untested."}

func (s *Server) blogSettingsPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	open, _ := strconv.ParseInt(r.URL.Query().Get("open"), 10, 64)
	s.showBlogSettings(w, r, u, http.StatusOK, open, "", nil, nil, blogFlash[r.URL.Query().Get("done")])
}

func (s *Server) blogSets(r *http.Request, loc *time.Location, open int64, in *store.BlogIn) []blogSet {
	bs, _ := s.st.Blogs(r.Context())
	x, _ := s.st.Accounts(r.Context())
	toks := s.publishTokens(r)
	var out []blogSet
	for _, b := range bs {
		v := blogSet{B: b, Open: b.ID == open || len(bs) == 1, Sub: methodLabel(b.Method)}
		if b.ID == open {
			v.F = formFor(b, in, x, loc.String(), toks)
		} else {
			v.F = formFor(b, nil, x, loc.String(), toks)
		}
		if b.PromoAccount != "" {
			v.Sub += " · promos from @" + b.PromoAccount
		}
		switch b.TestState {
		case store.TestPassed:
			v.Dot, v.DotLabel, v.TestCls = "ok", "Ready", "ok"
			v.Test = "Test passed"
		case store.TestFailed:
			v.Dot, v.DotLabel, v.TestCls = "bad", "Test failed", "bad"
			v.Test = "Test failed"
		default:
			v.Dot, v.DotLabel, v.TestCls = "untested", "Untested", "neutral"
			v.Test = "Not tested yet."
		}
		if b.TestedAt != nil && b.TestState != store.TestUntested {
			v.Test += " · " + b.TestedAt.In(loc).Format("Jan 2, 3:04 PM")
			if b.TestMs > 0 {
				v.Test += fmt.Sprintf(" (%.1f s)", float64(b.TestMs)/1000)
			}
			if b.TestDetail != "" {
				v.Test += ". " + strings.TrimSuffix(b.TestDetail, ".") + "."
			}
			if b.TestState == store.TestFailed {
				v.Sub += " · test failed " + b.TestedAt.In(loc).Format("3:04 PM")
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) showBlogSettings(w http.ResponseWriter, r *http.Request, u *store.User, status int, open int64, errMsg string, busy *store.BlogBusyError, in *store.BlogIn, flash string, note ...string) {
	loc := s.zone(r)
	sets := s.blogSets(r, loc, open, in)
	for i := range sets {
		if sets[i].B.ID == open {
			sets[i].Err, sets[i].Busy = errMsg, busy
			if len(note) > 0 {
				sets[i].Note = note[0]
			}
		}
	}
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Flash = "settings", "Blogs", flash
	p.Data = map[string]any{"Sets": sets, "TestOn": s.cfg.BlogTest != nil}
	s.render(w, status, "blogsettings", "app", p)
}

func (s *Server) blogNewPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	m := r.URL.Query().Get("method")
	s.showBlogNew(w, r, u, http.StatusOK, m, "", nil)
}

func (s *Server) showBlogNew(w http.ResponseWriter, r *http.Request, u *store.User, status int, method, errMsg string, in *store.BlogIn) {
	if method != store.MethodAPI && method != store.MethodWebhook {
		method = ""
	}
	x, _ := s.st.Accounts(r.Context())
	f := formFor(nil, in, x, s.zone(r).String(), s.publishTokens(r))
	if method != "" {
		f.Method = method
	}
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Error, p.Back = "settings", "Add blog", errMsg, "/settings/blogs"
	p.Data = map[string]any{"F": f, "Step": map[bool]int{true: 1, false: 2}[method == ""]}
	s.render(w, status, "blognew", "app", p)
}

// publishTokens lists the active tokens that may be a Publishing token.
func (s *Server) publishTokens(r *http.Request) []tokenOpt {
	ts, _ := s.st.AgentTokens(r.Context())
	var out []tokenOpt
	for _, t := range ts {
		if t.RevokedAt == nil && t.Has(store.ScopeBlogsPublish) {
			out = append(out, tokenOpt{ID: t.ID, Label: t.Name + " (" + t.Masked() + ")"})
		}
	}
	return out
}

// blogIn reads the blog form.
func blogIn(r *http.Request) store.BlogIn {
	topics := formTopics(r)
	tok, _ := strconv.ParseInt(r.PostFormValue("publish_token"), 10, 64)
	return store.BlogIn{Zone: r.PostFormValue("zone"), PublishTokenID: tok, Name: r.PostFormValue("name"), SiteURL: r.PostFormValue("site_url"), Method: r.PostFormValue("method"), APIBase: r.PostFormValue("api_base"),
		PublishClock: r.PostFormValue("clock"), PromoAccount: r.PostFormValue("promo"), Topics: topics, EnDash: blogtext.Level(r.PostFormValue("lint_en")),
		Banned: blogtext.Level(r.PostFormValue("lint_banned")), LeadingH1: blogtext.Level(r.PostFormValue("lint_h1")), CoverAltRequired: r.PostFormValue("cover_alt") == "1"}
}

// formTopics reads the banned topic chips (a checked "topic" box keeps a
// topic), the Add topic field, and the older one-per-line "topics" field,
// without repeats.
func formTopics(r *http.Request) []string {
	var topics []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.Join(strings.Fields(t), " ")
		if k := strings.ToLower(t); t != "" && !seen[k] {
			seen[k] = true
			topics = append(topics, t)
		}
	}
	for _, t := range r.PostForm["topic"] {
		add(t)
	}
	for _, t := range strings.FieldsFunc(r.PostFormValue("topic_new"), func(c rune) bool { return c == ',' || c == '\n' }) {
		add(t)
	}
	for _, t := range strings.Split(r.PostFormValue("topics"), "\n") {
		add(t)
	}
	return topics
}

// secretFields are the sealed values each method uses.
func secretFields(method string) []string {
	if method == store.MethodAPI {
		return []string{store.BlogAPIToken}
	}
	return []string{store.BlogHookURL, store.BlogHookKey, store.BlogHookAuth}
}

func inputMsg(err error) (string, bool) {
	var ie *store.InputError
	if errors.As(err, &ie) {
		return strings.ToUpper(ie.Msg[:1]) + ie.Msg[1:] + ".", true
	}
	return "", false
}

// saveSecrets stores the sealed values typed into the form; an empty field
// keeps the saved value.
func (s *Server) saveSecrets(r *http.Request, u *store.User, b *store.Blog, method string) error {
	for _, f := range secretFields(method) {
		v := r.PostFormValue(f)
		if strings.TrimSpace(v) == "" {
			continue
		}
		if err := s.st.SetBlogSecret(r.Context(), u, b.ID, f, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) blogCreate(w http.ResponseWriter, r *http.Request, u *store.User) {
	in := blogIn(r)
	if r.PostFormValue("op") == "addtopic" { // Add topic without scripts: show the form again
		s.showBlogNew(w, r, u, http.StatusOK, in.Method, "", &in)
		return
	}
	// Check the sealed values before creating anything.
	for _, f := range secretFields(in.Method) {
		if _, err := store.CleanBlogSecret(f, r.PostFormValue(f)); err != nil {
			msg, _ := inputMsg(err)
			s.showBlogNew(w, r, u, http.StatusBadRequest, in.Method, msg, &in)
			return
		}
	}
	b, err := s.st.CreateBlog(r.Context(), u, in)
	if err != nil {
		if msg, ok := inputMsg(err); ok {
			s.showBlogNew(w, r, u, http.StatusBadRequest, in.Method, msg, &in)
			return
		}
		s.internalError(w, err)
		return
	}
	if err := s.saveSecrets(r, u, b, in.Method); err != nil {
		s.internalError(w, err)
		return
	}
	s.log.Printf("settings: blog %d added by the owner", b.ID)
	http.Redirect(w, r, fmt.Sprintf("/settings/blogs?done=added&open=%d#blog-%d", b.ID, b.ID), http.StatusSeeOther)
}

func (s *Server) blogSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	b, err := s.st.BlogByID(ctx, id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	back := func(done string) {
		http.Redirect(w, r, fmt.Sprintf("/settings/blogs?done=%s&open=%d#blog-%d", done, id, id), http.StatusSeeOther)
	}
	op := r.PostFormValue("op")
	switch {
	case op == "remove":
		err := s.st.RemoveBlog(ctx, u, id)
		var busy *store.BlogBusyError
		if errors.As(err, &busy) {
			s.showBlogSettings(w, r, u, http.StatusConflict, id, "", busy, nil, "")
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.log.Printf("settings: blog %d removed by the owner", id)
		http.Redirect(w, r, "/settings/blogs?done=removed", http.StatusSeeOther)
		return
	case op == "test":
		if s.cfg.BlogTest == nil {
			s.showBlogSettings(w, r, u, http.StatusConflict, id, "Testing is not available on this server yet: no blog publisher runs here.", nil, nil, "")
			return
		}
		start := time.Now()
		detail, terr := s.cfg.BlogTest(ctx, b)
		ms := int(time.Since(start).Milliseconds())
		if errors.Is(terr, store.ErrLiveOff) || errors.Is(terr, store.ErrNoPublisher) {
			// No call was made: say so, and keep the last real result.
			s.showBlogSettings(w, r, u, http.StatusOK, id, "", nil, nil, "", terr.Error())
			return
		}
		if terr != nil {
			detail = terr.Error()
		}
		// Never show or keep a URL or secret a publisher echoed back.
		detail = s.st.CleanBlogTestDetail(ctx, id, detail)
		if terr != nil && detail == "" {
			detail = "Test failed."
		}
		if err := s.st.RecordBlogTest(ctx, u, id, terr == nil, detail, ms); err != nil {
			s.internalError(w, err)
			return
		}
		if terr != nil {
			back("testfailed")
			return
		}
		back("tested")
		return
	case strings.HasPrefix(op, "clear-"):
		f := strings.TrimPrefix(op, "clear-")
		if f != store.BlogAPIToken && f != store.BlogHookURL && f != store.BlogHookKey && f != store.BlogHookAuth {
			s.notFound(w, r)
			return
		}
		if err := s.st.ClearBlogSecret(ctx, u, id, f); err != nil {
			s.internalError(w, err)
			return
		}
		back("cleared")
		return
	}
	in := blogIn(r)
	fail := func(err error) {
		if msg, ok := inputMsg(err); ok {
			s.showBlogSettings(w, r, u, http.StatusBadRequest, id, msg, nil, &in, "")
			return
		}
		s.internalError(w, err)
	}
	for _, f := range secretFields(in.Method) {
		if _, err := store.CleanBlogSecret(f, r.PostFormValue(f)); err != nil {
			fail(err)
			return
		}
	}
	if _, err := s.st.UpdateBlog(ctx, u, id, in); err != nil {
		fail(err)
		return
	}
	if err := s.saveSecrets(r, u, b, in.Method); err != nil {
		fail(err)
		return
	}
	if op == "addtopic" {
		http.Redirect(w, r, fmt.Sprintf("/settings/blogs?done=topic&open=%d#topics%d", id, id), http.StatusSeeOther)
		return
	}
	back("saved")
}
