package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/blogtext"
	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/weight"
	"github.com/scott/xpost/internal/xauth"
)

// The agent draft-intake API. Bearer agent tokens only; cookies are never
// read here. The full route list is below and there is deliberately no
// route that approves, edits, reschedules, rejects or posts: any other
// method or path answers 404 or 405 without touching a draft.
//
//	POST   /api/v1/media         drafts:write  upload an image -> {media_ref}
//	POST   /api/v1/drafts        drafts:write  propose a draft (always pending)
//	GET    /api/v1/drafts        drafts:write  list the token's own drafts
//	GET    /api/v1/drafts/{id}   drafts:write  read one of the token's own drafts
//	DELETE /api/v1/drafts/{id}   drafts:write  delete own draft, only while pending
//	POST   /api/v1/drafts/{id}/revise drafts:revise answer an open rewrite request (parts for X, post for a blog post)
//	                                      for an own draft: new text, same images
//	GET    /api/v1/log           log:read      posted/failed events
//	GET    /api/v1/blog-publishes/{id}[/cover], POST .../report
//	                             blogs:publish pull and report a publish of
//	                                      the blog this token publishes for
//	                                      (see blogpublish.go)

type agentHandler func(http.ResponseWriter, *http.Request, *store.AgentToken)

func (s *Server) apiRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/media", s.bearer(store.ScopeDraftsWrite, s.apiMedia))
	mux.HandleFunc("POST /api/v1/drafts", s.bearer(store.ScopeDraftsWrite, s.apiCreateDraft))
	mux.HandleFunc("GET /api/v1/drafts", s.bearer(store.ScopeDraftsWrite, s.apiListDrafts))
	mux.HandleFunc("GET /api/v1/drafts/{id}", s.bearer(store.ScopeDraftsWrite, s.apiGetDraft))
	mux.HandleFunc("DELETE /api/v1/drafts/{id}", s.bearer(store.ScopeDraftsWrite, s.apiDeleteDraft))
	mux.HandleFunc("POST /api/v1/drafts/{id}/revise", s.bearer(store.ScopeDraftsRevise, s.apiReviseDraft))
	mux.HandleFunc("GET /api/v1/log", s.bearer(store.ScopeLogRead, s.apiLog))
	mux.HandleFunc("GET /api/v1/blog-publishes/{id}", s.bearer(store.ScopeBlogsPublish, s.apiGetPublish))
	mux.HandleFunc("GET /api/v1/blog-publishes/{id}/cover", s.bearer(store.ScopeBlogsPublish, s.apiPublishCover))
	mux.HandleFunc("POST /api/v1/blog-publishes/{id}/report", s.bearer(store.ScopeBlogsPublish, s.apiReportPublish))
	// Known paths with another method are 405 (there is no PUT or PATCH:
	// agents cannot edit); anything else is 404.
	allow := map[string]string{
		"/api/v1/media": "POST", "/api/v1/drafts": "GET, POST", "/api/v1/log": "GET",
	}
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		a, ok := allow[r.URL.Path]
		if rest, cut := strings.CutPrefix(r.URL.Path, "/api/v1/drafts/"); cut && rest != "" {
			if !strings.Contains(rest, "/") {
				a, ok = "GET, DELETE", true
			} else if id, tail, _ := strings.Cut(rest, "/"); id != "" && tail == "revise" {
				a, ok = "POST", true
			}
		}
		if rest, cut := strings.CutPrefix(r.URL.Path, "/api/v1/blog-publishes/"); cut && rest != "" {
			switch id, tail, _ := strings.Cut(rest, "/"); {
			case id != "" && tail == "":
				a, ok = "GET", true
			case id != "" && tail == "cover":
				a, ok = "GET", true
			case id != "" && tail == "report":
				a, ok = "POST", true
			}
		}
		if ok {
			w.Header().Set("Allow", a)
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		jsonError(w, http.StatusNotFound, "no such endpoint")
	})
	return mux
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "status": status})
}

// bearer authenticates the agent token and checks its scope.
func (s *Server) bearer(scope string, h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.limited(w, r, "", func(st int, m string) { jsonError(w, st, m) }) {
			return
		}
		authz := r.Header.Get("Authorization")
		secret, ok := strings.CutPrefix(authz, "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="xpost"`)
			jsonError(w, http.StatusUnauthorized, "send Authorization: Bearer <agent token>")
			return
		}
		tok, err := s.st.AuthenticateAgent(r.Context(), strings.TrimSpace(secret))
		if err != nil {
			s.fail(r, "", "bad_agent_token")
			w.Header().Set("WWW-Authenticate", `Bearer realm="xpost", error="invalid_token"`)
			jsonError(w, http.StatusUnauthorized, "invalid or revoked token")
			return
		}
		if !tok.Has(scope) {
			jsonError(w, http.StatusForbidden, "this token lacks the "+scope+" scope")
			return
		}
		h(w, r, tok)
	}
}

// ---- wire types ----

// apiMedia is a media item in a request: "ref" or {"ref": ..., "alt": ...}.
type apiMedia struct {
	Ref string `json:"ref"`
	Alt string `json:"alt,omitempty"`
}

func (m *apiMedia) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		m.Ref = s
		return nil
	}
	type plain apiMedia
	return json.Unmarshal(b, (*plain)(m))
}

type apiPartIn struct {
	Text  string     `json:"text"`
	Media []apiMedia `json:"media"`
}

type apiDraftIn struct {
	Kind           string      `json:"kind"` // "x" (default), "reply" or "blog"
	Account        string      `json:"account"`
	Parts          []apiPartIn `json:"parts"`
	ProposedAt     string      `json:"proposed_at"`
	Note           string      `json:"note"`
	IdempotencyKey string      `json:"idempotency_key"`
	// Promo X posts only: the blog post this promotes, and minutes after
	// its publish time (default 120) unless proposed_at fixes the time.
	Promotes    int64 `json:"promotes"`
	PromoOffset *int  `json:"promo_offset_minutes"`
	// Blog posts (kind "blog") only.
	Site        string    `json:"site"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags"`
	Markdown    string    `json:"markdown"`
	Cover       *apiMedia `json:"cover"`
	// Replies (kind "reply", or any X draft with in_reply_to) only: the
	// post this answers, as the agent read it. xpost never reads X.
	InReplyTo     string       `json:"in_reply_to"`
	Original      *apiOriginal `json:"original"`
	ReplySettings string       `json:"reply_settings"`
}

// apiOriginal is the post a reply answers, as the agent supplies it (and
// as the API returns it). unavailable: the agent could not load it.
type apiOriginal struct {
	AuthorName   string `json:"author_name"`
	AuthorHandle string `json:"author_handle"`
	Text         string `json:"text"`
	CreatedAt    string `json:"created_at,omitempty"`
	URL          string `json:"url,omitempty"`
	Unavailable  bool   `json:"unavailable,omitempty"`
}

// apiReplyOut is the reply half of a reply draft. approve_block is why
// Approve is refused ("" when it is not); warnings never block.
type apiReplyOut struct {
	InReplyTo     string       `json:"in_reply_to"`
	Original      apiOriginal  `json:"original"`
	ReplySettings string       `json:"reply_settings,omitempty"`
	PostsBy       string       `json:"posts_by"` // always "hand": the owner posts it on x.com
	ApproveBlock  string       `json:"approve_block,omitempty"`
	Warnings      []string     `json:"warnings"`
	Lint          []apiFinding `json:"lint"`
	OpenedAt      *string      `json:"opened_at"`
	PostedURL     string       `json:"posted_url,omitempty"`
}

// apiFinding is a lint finding as the API reports it.
type apiFinding struct {
	Rule    string `json:"rule"`
	Level   string `json:"level"` // "block" or "warn"
	Where   string `json:"where"`
	Message string `json:"message"`
	Excerpt string `json:"excerpt"`
}

func apiFindings(fs []blogtext.Finding) []apiFinding {
	out := []apiFinding{}
	for _, f := range fs {
		out = append(out, apiFinding{Rule: f.Rule, Level: string(f.Level), Where: f.Where, Message: f.Msg, Excerpt: f.Before + f.Hit + f.After})
	}
	return out
}

// apiBlogOut is the blog half of a blog draft.
type apiBlogOut struct {
	Site         string       `json:"site"`
	Slug         string       `json:"slug"`
	Title        string       `json:"title"`
	Description  string       `json:"description"`
	Tags         []string     `json:"tags"`
	Markdown     string       `json:"markdown"`
	Cover        *apiMedia    `json:"cover"`
	Words        int          `json:"words"`
	Lint         []apiFinding `json:"lint"`
	PublishedURL string       `json:"published_url,omitempty"`
}

type apiPartOut struct {
	Text     string     `json:"text"`
	Weighted int        `json:"weighted_length"`
	Media    []apiMedia `json:"media"`
}

type apiPost struct {
	Part   int    `json:"part"`
	PostID string `json:"post_id"`
	URL    string `json:"url"`
}

type apiDraftOut struct {
	ID           int64        `json:"id"`
	Kind         string       `json:"kind"`
	Account      string       `json:"account"` // for a blog post, the site`
	Status       string       `json:"status"`
	ProposedAt   string       `json:"proposed_at"`
	ScheduledAt  *string      `json:"scheduled_at"`
	Parts        []apiPartOut `json:"parts"`
	Note         string       `json:"note,omitempty"`
	RejectReason string       `json:"reject_reason,omitempty"`
	Error        string       `json:"error,omitempty"`
	Posts        []apiPost    `json:"posts,omitempty"`
	Rewrite      *apiRewrite  `json:"rewrite,omitempty"`
	Blog         *apiBlogOut  `json:"blog,omitempty"`
	Reply        *apiReplyOut `json:"reply,omitempty"`
	Promotes     int64        `json:"promotes,omitempty"`
	PromoOffset  *int         `json:"promo_offset_minutes,omitempty"`
	CreatedAt    string       `json:"created_at"`
	UpdatedAt    string       `json:"updated_at"`
}

// apiRewrite is the draft's latest rewrite request. state is open (the
// owner is waiting for a revise), expired (60 minutes passed; a reply is
// refused), revised, unchanged or cancelled.
type apiRewrite struct {
	RequestID int64  `json:"request_id"`
	State     string `json:"state"`
	Note      string `json:"note,omitempty"`
	SentAt    string `json:"sent_at"`
	ExpiresAt string `json:"expires_at"`
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func (s *Server) draftOut(r *http.Request, d *store.Draft) apiDraftOut {
	o := apiDraftOut{ID: d.ID, Kind: d.Kind, Account: d.Account, Status: d.Status, ProposedAt: rfc(d.ProposedAt), Note: d.Note, Error: d.Error,
		CreatedAt: rfc(d.CreatedAt), UpdatedAt: rfc(d.UpdatedAt), Parts: []apiPartOut{}}
	if rq, err := s.st.LatestRewrite(r.Context(), d.ID); err == nil && rq.TokenID == d.TokenID {
		st := rq.State
		if rq.TimedOut(s.st.Now()) {
			st = "expired"
		}
		o.Rewrite = &apiRewrite{RequestID: rq.ID, State: st, Note: rq.Note, SentAt: rfc(rq.SentAt), ExpiresAt: rfc(rq.ExpiresAt)}
	}
	if d.IsBlog() {
		s.blogOut(r, d, &o)
		return o
	}
	o.Promotes, o.PromoOffset = d.Promotes, d.PromoOffset
	if d.Status == store.StatusRejected {
		o.RejectReason = d.RejectReason
	}
	if d.IsReply() {
		// A reply has no time: it never posts through the API.
		o.Kind = store.KindReply
		for _, p := range d.Parts {
			o.Parts = append(o.Parts, apiPartOut{Text: p.Text, Weighted: weight.Count(p.Text).Weighted, Media: []apiMedia{}})
		}
		o.Reply = s.replyOut(r, d)
		return o
	}
	// The scheduled time is only meaningful once the owner approved it.
	if d.Status != store.StatusPending && d.Status != store.StatusRejected {
		t := rfc(d.ScheduledAt)
		o.ScheduledAt = &t
	}
	for _, p := range d.Parts {
		po := apiPartOut{Text: p.Text, Weighted: weight.Count(p.Text).Weighted, Media: []apiMedia{}}
		for _, m := range p.Media {
			po.Media = append(po.Media, apiMedia{Ref: m.Ref, Alt: m.Alt})
		}
		o.Parts = append(o.Parts, po)
	}
	if d.Status == store.StatusPosted || d.Status == store.StatusFailed {
		if entries, err := s.st.PostLog(r.Context(), time.Unix(0, 0), store.StatusPosted, 1000); err == nil {
			for i := len(entries) - 1; i >= 0; i-- {
				if e := entries[i]; e.DraftID == d.ID {
					o.Posts = append(o.Posts, apiPost{Part: e.Part, PostID: e.PostID, URL: e.URL})
				}
			}
		}
	}
	return o
}

// blogOut fills a blog draft's API view. Blog posts are not in the post
// log: published_url is where the post went.
func (s *Server) blogOut(r *http.Request, d *store.Draft, o *apiDraftOut) {
	if d.Status != store.StatusPending && d.Status != store.StatusRejected && d.Status != store.StatusFailed {
		t := rfc(d.ScheduledAt)
		o.ScheduledAt = &t
	}
	p := d.Post
	bo := &apiBlogOut{Slug: p.Slug, Title: p.Title, Description: p.Description, Tags: p.Tags, Markdown: p.Markdown, Words: blogtext.Words(p.Markdown),
		PublishedURL: p.PublishedURL, Lint: []apiFinding{}}
	if bo.Tags == nil {
		bo.Tags = []string{}
	}
	if p.Cover != nil {
		bo.Cover = &apiMedia{Ref: p.Cover.Ref, Alt: p.Cover.Alt}
	}
	if b, err := s.st.BlogForAccount(r.Context(), d.Account); err == nil {
		bo.Site = b.Name
		bo.Lint = apiFindings(store.LintPost(b, p))
	}
	o.Account = bo.Site
	o.Blog = bo
}

// ---- handlers ----

func (s *Server) apiMedia(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			jsonError(w, http.StatusRequestEntityTooLarge, "images can be up to 5 MB")
			return
		}
		jsonError(w, http.StatusBadRequest, "send multipart/form-data with a file field named \"file\"")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		jsonError(w, http.StatusBadRequest, "missing file field \"file\"")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, store.MaxMediaBytes+1))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "could not read the file")
		return
	}
	ref, _, err := s.st.AddMedia(r.Context(), tok, data)
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"media_ref": ref})
}

func (s *Server) apiCreateDraft(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // e.g. "status": "approved" is refused, not ignored
	var in apiDraftIn
	if err := dec.Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	switch in.Kind {
	case "blog":
		if !s.cfg.BlogsEnabled {
			jsonError(w, http.StatusBadRequest, `blogs are off on this server (XPOST_BLOGS_ENABLED): propose kind "x" or "reply"`)
			return
		}
		s.apiCreateBlogDraft(w, r, tok, in)
		return
	case store.KindReply:
		if in.InReplyTo == "" {
			jsonError(w, http.StatusBadRequest, `a reply needs in_reply_to: the post id or its x.com link`)
			return
		}
	case "", "x":
	default:
		jsonError(w, http.StatusBadRequest, `kind must be "x", "reply" or "blog"`)
		return
	}
	if in.InReplyTo != "" {
		s.apiCreateReply(w, r, tok, in)
		return
	}
	if in.Original != nil || in.ReplySettings != "" {
		jsonError(w, http.StatusBadRequest, `original and reply_settings are for replies: send in_reply_to too`)
		return
	}
	if in.Site != "" || in.Slug != "" || in.Title != "" || in.Description != "" || in.Tags != nil || in.Markdown != "" || in.Cover != nil {
		jsonError(w, http.StatusBadRequest, `site, slug, title, description, tags, markdown and cover are for kind "blog"`)
		return
	}
	if in.PromoOffset != nil && in.Promotes == 0 {
		jsonError(w, http.StatusBadRequest, "promo_offset_minutes needs promotes")
		return
	}
	var at time.Time
	if in.ProposedAt != "" || in.Promotes == 0 {
		var err error
		if at, err = time.Parse(time.RFC3339, in.ProposedAt); err != nil {
			jsonError(w, http.StatusBadRequest, "proposed_at must be RFC 3339, e.g. 2026-11-02T16:00:00Z")
			return
		}
	}
	nd := store.NewDraft{Account: strings.TrimPrefix(in.Account, "@"), ProposedAt: at, Note: in.Note, IdempotencyKey: in.IdempotencyKey,
		Promotes: in.Promotes, PromoOffset: in.PromoOffset}
	for _, p := range in.Parts {
		pi := store.PartIn{Text: p.Text}
		for _, m := range p.Media {
			pi.Media = append(pi.Media, store.MediaIn{Ref: m.Ref, Alt: m.Alt})
		}
		nd.Parts = append(nd.Parts, pi)
	}
	d, created, err := s.st.CreateDraft(r.Context(), tok, nd)
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK // idempotent replay: the existing draft, unchanged
	}
	writeJSON(w, status, s.draftOut(r, d))
}

// apiCreateBlogDraft proposes a blog post. Lint that blocks is a 422 with
// the findings; warnings come back on the draft.
func (s *Server) apiCreateBlogDraft(w http.ResponseWriter, r *http.Request, tok *store.AgentToken, in apiDraftIn) {
	if in.Account != "" || in.Parts != nil || in.Promotes != 0 || in.PromoOffset != nil {
		jsonError(w, http.StatusBadRequest, `account, parts and promotes are for X posts; a blog post names its "site"`)
		return
	}
	if in.InReplyTo != "" || in.Original != nil || in.ReplySettings != "" {
		jsonError(w, http.StatusBadRequest, `in_reply_to, original and reply_settings are for replies, not blog posts`)
		return
	}
	nb := store.NewBlogDraft{Site: in.Site, Slug: in.Slug, Title: in.Title, Description: in.Description, Tags: in.Tags, Markdown: in.Markdown,
		Note: in.Note, IdempotencyKey: in.IdempotencyKey}
	if in.ProposedAt != "" {
		at, err := time.Parse(time.RFC3339, in.ProposedAt)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "proposed_at must be RFC 3339, e.g. 2026-11-02T16:00:00Z, or left out for the blog's default time")
			return
		}
		nb.ProposedAt = at
	}
	if in.Cover != nil {
		nb.Cover = &store.MediaIn{Ref: in.Cover.Ref, Alt: in.Cover.Alt}
	}
	d, created, err := s.st.CreateBlogDraft(r.Context(), tok, nb, s.zone(r))
	var le *store.LintError
	if errors.As(err, &le) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": le.Error(), "lint": apiFindings(le.Findings)})
		return
	}
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, s.draftOut(r, d))
}

func (s *Server) apiListDrafts(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	q := r.URL.Query()
	st := q.Get("status")
	if st != "" && !store.ValidStatus(st) {
		jsonError(w, http.StatusBadRequest, "unknown status")
		return
	}
	ds, err := s.st.AgentDrafts(r.Context(), tok, st, strings.TrimPrefix(q.Get("account"), "@"))
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	out := []apiDraftOut{}
	for _, d := range ds {
		out = append(out, s.draftOut(r, d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": out})
}

func (s *Server) apiGetDraft(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	d, err := s.st.AgentDraft(r.Context(), tok, id)
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.draftOut(r, d))
}

func (s *Server) apiDeleteDraft(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.st.AgentDeleteDraft(r.Context(), tok, id); err != nil {
		if errors.Is(err, store.ErrConflict) {
			jsonError(w, http.StatusConflict, "only pending drafts can be deleted; this one is no longer pending")
			return
		}
		s.apiStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiReviseIn answers a rewrite request. Parts carry text only: a rewrite
// can't change images (each image stays on its part).
type apiReviseIn struct {
	RequestID int64 `json:"request_id"`
	Parts     []struct {
		Text  string          `json:"text"`
		Media json.RawMessage `json:"media"`
	} `json:"parts"`
	Post *apiRevisePost `json:"post"`
}

// apiRevisePost is a blog post's new text. Every field is required (send
// the old value to keep it); slug and cover are not accepted.
type apiRevisePost struct {
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Tags        *[]string `json:"tags"`
	Markdown    *string   `json:"markdown"`
}

func (s *Server) apiReviseDraft(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var in apiReviseIn
	if err := dec.Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if in.RequestID <= 0 {
		jsonError(w, http.StatusBadRequest, "request_id is required: the id from the rewrite webhook")
		return
	}
	if in.Post != nil {
		p := in.Post
		if in.Parts != nil {
			jsonError(w, http.StatusBadRequest, `send "post" for a blog post or "parts" for an X post, not both`)
			return
		}
		if p.Title == nil || p.Description == nil || p.Tags == nil || p.Markdown == nil {
			jsonError(w, http.StatusBadRequest, "post needs title, description, tags and markdown (send the old value to keep one)")
			return
		}
		d, result, err := s.st.ReviseBlogPost(r.Context(), tok, id, in.RequestID, store.BlogRevise{Title: *p.Title, Description: *p.Description, Tags: *p.Tags, Markdown: *p.Markdown})
		s.reviseResult(w, r, d, result, err)
		return
	}
	texts := make([]string, 0, len(in.Parts))
	for i, p := range in.Parts {
		if m := strings.TrimSpace(string(p.Media)); m != "" && m != "null" {
			jsonError(w, http.StatusBadRequest, "part "+strconv.Itoa(i+1)+": a rewrite can't change images; send parts with text only (images stay on their parts)")
			return
		}
		texts = append(texts, p.Text)
	}
	d, result, err := s.st.ReviseDraft(r.Context(), tok, id, in.RequestID, texts)
	s.reviseResult(w, r, d, result, err)
}

// reviseResult answers a revise: the draft (a blog post's lint included)
// and the result, or the refusal.
func (s *Server) reviseResult(w http.ResponseWriter, r *http.Request, d *store.Draft, result string, err error) {
	if err != nil {
		var re *store.RewriteError
		if errors.As(err, &re) {
			jsonError(w, http.StatusConflict, re.Msg)
			return
		}
		s.apiStoreError(w, err)
		return
	}
	out := s.draftOut(r, d)
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "draft": out})
}

type apiLogOut struct {
	DraftID      int64  `json:"draft_id"`
	Account      string `json:"account"`
	Part         int    `json:"part"`
	Parts        int    `json:"parts"`
	At           string `json:"at"`
	Status       string `json:"status"`
	PostID       string `json:"post_id,omitempty"`
	URL          string `json:"url,omitempty"`
	Error        string `json:"error,omitempty"`
	Images       int    `json:"images"`
	CostEstimate string `json:"cost_estimate_usd"` // the post only; image uploads are in the spend meter
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	since := time.Now().AddDate(0, 0, -30)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "since must be RFC 3339")
			return
		}
		since = t
	}
	entries, err := s.st.PostLog(r.Context(), since, "", 500)
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	out := []apiLogOut{}
	for _, e := range entries {
		cost := ""
		if s.st.Rates.Known && (e.Status == store.LogPosted || e.Status == store.LogUnknown) {
			cost = strings.TrimPrefix(money(s.st.PartCost(e.HasURL)), "$")
		}
		out = append(out, apiLogOut{DraftID: e.DraftID, Account: e.Account, Part: e.Part, Parts: e.Parts, At: rfc(e.At), Status: e.Status,
			PostID: e.PostID, URL: e.URL, Error: e.Error, Images: e.Images, CostEstimate: cost})
	}
	// rates_configured: the operator set cost rates; amounts are still
	// estimates. Empty cost_estimate_usd means unverified or not billed.
	writeJSON(w, http.StatusOK, map[string]any{"events": out, "rates_configured": s.st.Rates.Known})
}

func (s *Server) apiStoreError(w http.ResponseWriter, err error) {
	var ie *store.InputError
	switch {
	case errors.As(err, &ie):
		jsonError(w, http.StatusBadRequest, ie.Msg)
	case errors.Is(err, store.ErrNotFound):
		jsonError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		var ce *store.ConflictError
		if errors.As(err, &ce) && ce.Msg != "" {
			jsonError(w, http.StatusConflict, ce.Msg)
			return
		}
		jsonError(w, http.StatusConflict, "conflict")
	default:
		s.log.Printf("api error: %v", err)
		jsonError(w, http.StatusInternalServerError, "something went wrong")
	}
}

// parseInReplyTo takes a post id or an x.com status link and returns the
// id (digits only).
func parseInReplyTo(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if store.ValidPostID(v) {
		return v, true
	}
	if _, id, err := xauth.ParseStatusLink(v); err == nil {
		return id, true
	}
	return "", false
}

// apiCreateReply proposes a reply. It lands in Pending with no time; the
// owner approves it and posts it by hand on x.com. Nothing about a reply
// ever goes to the X API, so it adds $0 to the spend estimate.
func (s *Server) apiCreateReply(w http.ResponseWriter, r *http.Request, tok *store.AgentToken, in apiDraftIn) {
	if in.Site != "" || in.Slug != "" || in.Title != "" || in.Description != "" || in.Tags != nil || in.Markdown != "" || in.Cover != nil {
		jsonError(w, http.StatusBadRequest, `site, slug, title, description, tags, markdown and cover are for kind "blog"`)
		return
	}
	if in.Promotes != 0 || in.PromoOffset != nil {
		jsonError(w, http.StatusBadRequest, "a reply can't promote a blog post")
		return
	}
	if in.ProposedAt != "" {
		jsonError(w, http.StatusBadRequest, "a reply has no proposed_at: the owner posts it by hand on X after approving it")
		return
	}
	id, ok := parseInReplyTo(in.InReplyTo)
	if !ok {
		jsonError(w, http.StatusBadRequest, "in_reply_to must be a post id (digits) or an x.com status link")
		return
	}
	nr := &store.NewReply{InReplyTo: id, ReplySettings: in.ReplySettings, Unavailable: true}
	if o := in.Original; o != nil {
		nr.AuthorName, nr.AuthorHandle, nr.Text, nr.Unavailable = o.AuthorName, o.AuthorHandle, o.Text, o.Unavailable
		if o.CreatedAt != "" {
			t, err := time.Parse(time.RFC3339, o.CreatedAt)
			if err != nil {
				jsonError(w, http.StatusBadRequest, "original.created_at must be RFC 3339, e.g. 2026-11-03T19:14:00Z")
				return
			}
			nr.CreatedAt = &t
		}
		if o.URL != "" {
			h, lid, err := xauth.ParseStatusLink(o.URL)
			if err != nil || lid != id {
				jsonError(w, http.StatusBadRequest, "original.url must be the x.com link of the post in in_reply_to")
				return
			}
			if o.AuthorHandle != "" && !strings.EqualFold(h, strings.TrimPrefix(o.AuthorHandle, "@")) {
				jsonError(w, http.StatusBadRequest, "original.url is not a post by original.author_handle")
				return
			}
			nr.URL = strings.TrimSpace(o.URL)
		}
	}
	nd := store.NewDraft{Account: strings.TrimPrefix(in.Account, "@"), Note: in.Note, IdempotencyKey: in.IdempotencyKey, Reply: nr}
	for _, p := range in.Parts {
		pi := store.PartIn{Text: p.Text}
		for _, m := range p.Media {
			pi.Media = append(pi.Media, store.MediaIn{Ref: m.Ref, Alt: m.Alt})
		}
		nd.Parts = append(nd.Parts, pi)
	}
	d, created, err := s.st.CreateDraft(r.Context(), tok, nd)
	if err != nil {
		s.apiStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, s.draftOut(r, d))
}

func (s *Server) replyOut(r *http.Request, d *store.Draft) *apiReplyOut {
	rp := d.Reply
	o := &apiReplyOut{InReplyTo: rp.InReplyTo, ReplySettings: rp.ReplySettings, PostsBy: "hand", PostedURL: rp.PostedURL,
		Original: apiOriginal{AuthorName: rp.AuthorName, AuthorHandle: rp.AuthorHandle, Text: rp.Text, URL: rp.URL, Unavailable: !rp.Available},
		Warnings: s.replyWarnings(r, d), Lint: apiFindings(s.st.LintReply(r.Context(), d.Account, d.Parts[0].Text))}
	if rp.CreatedAt != nil {
		o.Original.CreatedAt = rfc(*rp.CreatedAt)
	}
	if rp.OpenedAt != nil {
		t := rfc(*rp.OpenedAt)
		o.OpenedAt = &t
	}
	if d.Status == store.StatusPending {
		o.ApproveBlock = s.st.ReplyBlock(r.Context(), d)
	}
	return o
}
