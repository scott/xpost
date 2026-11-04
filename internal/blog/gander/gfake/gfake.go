// Package gfake is a fake gander Publisher API for tests, demos and
// screenshots. It follows the shapes of gander's Publisher API: Bearer
// auth, the posts list, get, upsert (unknown fields refused), publish,
// unpublish, the public post page, and the error body
// {"error","message"}. It only serves: it never makes a request, and it
// never logs a token or a request body.
package gfake

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Post is one post on the fake.
type Post struct {
	Slug, Title, Description, Markdown, Date, CoverMIME string
	Draft                                               bool
	Cover                                               []byte
	CreatedAt, UpdatedAt                                time.Time
}

// Fault answers the next matching call (Op: list, get, put, publish,
// page; Slug empty matches any) with Status and the error body, once, or
// every time when Sticky.
type Fault struct {
	Op, Slug      string
	Status        int
	Code, Message string
	Sticky        bool
}

// Server is the fake. Base is the URL it reports posts at (its own https
// address). Token is the one Bearer token it accepts.
type Server struct {
	Base  string
	Token string
	Now   func() time.Time

	mu     sync.Mutex
	posts  map[string]*Post
	faults []Fault
	calls  []string
}

var slugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Seed adds a post as if someone else had made it.
func (s *Server) Seed(p Post) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.posts == nil {
		s.posts = map[string]*Post{}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.now().UTC()
	}
	p.UpdatedAt = p.CreatedAt
	s.posts[p.Slug] = &p
}

// Get returns a copy of a post.
func (s *Server) Get(slug string) (Post, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.posts[slug]
	if !ok {
		return Post{}, false
	}
	return *p, true
}

// AddFault queues a fault.
func (s *Server) AddFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
}

// ClearFaults drops every queued fault.
func (s *Server) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// Calls lists the calls made, e.g. "GET /api/blog/posts/x auth".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func (s *Server) fault(op, slug string) *Fault {
	for i, f := range s.faults {
		if f.Op == op && (f.Slug == "" || f.Slug == slug) {
			if !f.Sticky {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			return &f
		}
	}
	return nil
}

type view struct {
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	Description string `json:"description"`
	Markdown    string `json:"markdown,omitempty"`
	Date        string `json:"date"`
	Draft       bool   `json:"draft"`
	Source      string `json:"source"`
	CoverURL    string `json:"cover_url"`
	CoverMIME   string `json:"cover_mime"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func (s *Server) view(p *Post, body bool) view {
	base := strings.TrimRight(s.Base, "/")
	v := view{Slug: p.Slug, Title: p.Title, Author: "Gander", Description: p.Description, Date: p.Date, Draft: p.Draft, Source: "api",
		CoverURL: base + "/blog/" + p.Slug + "/cover", CoverMIME: p.CoverMIME, URL: base + "/blog/" + p.Slug,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339)}
	if body {
		v.Markdown = p.Markdown
	}
	return v
}

func (s *Server) live(p *Post) bool {
	day, err := time.Parse("2006-01-02", p.Date)
	return err == nil && !p.Draft && !day.After(s.now().UTC())
}

// Handler serves the fake API and public pages.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	authed := func(op string, h func(w http.ResponseWriter, r *http.Request, slug string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			slug := r.PathValue("slug")
			s.mu.Lock()
			defer s.mu.Unlock()
			s.calls = append(s.calls, r.Method+" "+r.URL.Path+" auth")
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || tok == "" {
				writeErr(w, http.StatusUnauthorized, "missing_bearer", "Authorization: Bearer <token> required")
				return
			}
			if s.Token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(tok)), []byte(s.Token)) != 1 {
				writeErr(w, http.StatusUnauthorized, "invalid_token", "blog API token not recognized")
				return
			}
			if f := s.fault(op, slug); f != nil {
				writeErr(w, f.Status, f.Code, f.Message)
				return
			}
			if s.posts == nil {
				s.posts = map[string]*Post{}
			}
			h(w, r, slug)
		}
	}
	mux.Handle("GET /api/blog/posts", authed("list", func(w http.ResponseWriter, r *http.Request, _ string) {
		out := []view{}
		var slugs []string
		for k := range s.posts {
			slugs = append(slugs, k)
		}
		sort.Strings(slugs)
		for _, k := range slugs {
			p := s.posts[k]
			if r.URL.Query().Get("published") == "1" && !s.live(p) {
				continue
			}
			out = append(out, s.view(p, false))
		}
		writeJSON(w, http.StatusOK, map[string]any{"posts": out})
	}))
	mux.Handle("GET /api/blog/posts/{slug}", authed("get", func(w http.ResponseWriter, r *http.Request, slug string) {
		p, ok := s.posts[slug]
		if !ok {
			writeErr(w, http.StatusNotFound, "not_found", "post not found")
			return
		}
		writeJSON(w, http.StatusOK, s.view(p, true))
	}))
	mux.Handle("PUT /api/blog/posts/{slug}", authed("put", func(w http.ResponseWriter, r *http.Request, slug string) {
		if !slugRe.MatchString(slug) {
			writeErr(w, http.StatusBadRequest, "invalid_slug", "slug must be lowercase kebab-case")
			return
		}
		var req struct {
			Title            *string `json:"title"`
			Date             *string `json:"date"`
			Author           *string `json:"author"`
			Description      *string `json:"description"`
			Markdown         *string `json:"markdown"`
			Draft            *bool   `json:"draft"`
			CoverBase64      *string `json:"cover_base64"`
			CoverContentType *string `json:"cover_content_type"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
			return
		}
		old, exists := s.posts[slug]
		p := Post{Slug: slug, Draft: true, CreatedAt: s.now().UTC()}
		if exists {
			p = *old
		}
		set := func(dst *string, v *string) {
			if v != nil {
				*dst = strings.TrimSpace(*v)
			}
		}
		set(&p.Title, req.Title)
		set(&p.Description, req.Description)
		if req.Markdown != nil {
			p.Markdown = *req.Markdown
		}
		if req.Draft != nil {
			p.Draft = *req.Draft
		}
		if req.Date != nil {
			if _, err := time.Parse("2006-01-02", strings.TrimSpace(*req.Date)); err != nil {
				writeErr(w, http.StatusBadRequest, "invalid_post", "invalid date")
				return
			}
			p.Date = strings.TrimSpace(*req.Date)
		}
		if req.CoverBase64 != nil {
			raw, err := base64.StdEncoding.DecodeString(*req.CoverBase64)
			if err != nil || len(raw) == 0 || len(raw) > 2<<20 {
				writeErr(w, http.StatusBadRequest, "invalid_post", "invalid cover_base64")
				return
			}
			mt := ""
			if req.CoverContentType != nil {
				mt = *req.CoverContentType
			}
			switch mt {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
			default:
				writeErr(w, http.StatusBadRequest, "invalid_post", "unsupported cover_content_type")
				return
			}
			p.Cover, p.CoverMIME = raw, mt
		}
		if !exists && (p.Title == "" || p.Description == "" || p.Markdown == "" || req.Date == nil || len(p.Cover) == 0) {
			writeErr(w, http.StatusBadRequest, "invalid_post", "title, description, and markdown required")
			return
		}
		p.UpdatedAt = s.now().UTC()
		s.posts[slug] = &p
		writeJSON(w, http.StatusOK, s.view(&p, true))
	}))
	flip := func(draft bool) func(w http.ResponseWriter, r *http.Request, slug string) {
		return func(w http.ResponseWriter, r *http.Request, slug string) {
			p, ok := s.posts[slug]
			if !ok {
				writeErr(w, http.StatusNotFound, "not_found", "post not found")
				return
			}
			p.Draft, p.UpdatedAt = draft, s.now().UTC()
			writeJSON(w, http.StatusOK, s.view(p, true))
		}
	}
	mux.Handle("POST /api/blog/posts/{slug}/publish", authed("publish", flip(false)))
	mux.Handle("POST /api/blog/posts/{slug}/unpublish", authed("unpublish", flip(true)))
	mux.HandleFunc("GET /blog/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		s.mu.Lock()
		defer s.mu.Unlock()
		c := "GET " + r.URL.Path
		if r.Header.Get("Authorization") != "" {
			c += " auth"
		}
		s.calls = append(s.calls, c)
		w.Header().Set("Cache-Control", "no-cache")
		if f := s.fault("page", slug); f != nil {
			w.WriteHeader(f.Status)
			return
		}
		p, ok := s.posts[slug]
		if !ok || !s.live(p) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("<!doctype html><title>Not found</title><p>Not found."))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>" + htmlEscape(p.Title) + "</title><h1>" + htmlEscape(p.Title) + "</h1>"))
	})
	return mux
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
