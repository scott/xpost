package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/scott/xpost/internal/blog"
	"github.com/scott/xpost/internal/store"
)

// The pull API for webhook-blog agents (scope blogs:publish). A token sees
// only publishes of the blog it is the Publishing token for, and only
// while they are in flight; anything else is 404 or 409. Nothing here
// ever marks a post Published: only xpost's own live check does.
//
//	GET  /api/v1/blog-publishes/{id}          the frozen payload and its hash
//	GET  /api/v1/blog-publishes/{id}/cover    the cover bytes, for that publish only
//	POST /api/v1/blog-publishes/{id}/report   opened, merged or failed

type apiPublish struct {
	PublishID int64  `json:"publish_id"`
	PostID    int64  `json:"post_id"`
	Version   int    `json:"version"`
	Attempt   int    `json:"attempt"`
	State     string `json:"state"`
	Key       string `json:"idempotency_key"`
	blog.HookPayload
	ExpiresAt string `json:"expires_at"`
}

func publishPath(id int64) string { return "/api/v1/blog-publishes/" + strconv.FormatInt(id, 10) }

// ownPublish loads the token's own publish that may still be pulled, or
// writes the error.
func (s *Server) ownPublish(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) (*store.BlogPublish, *store.Blog, bool) {
	id, ok := pathID(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "no such publish")
		return nil, nil, false
	}
	p, b, err := s.st.OwnBlogPublish(r.Context(), tok, id)
	if err != nil {
		jsonError(w, http.StatusNotFound, "no such publish")
		return nil, nil, false
	}
	if err := p.Serveable(s.st.Now()); err != nil {
		jsonError(w, http.StatusConflict, err.Error())
		return nil, nil, false
	}
	return p, b, true
}

func (s *Server) apiGetPublish(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	p, _, ok := s.ownPublish(w, r, tok)
	if !ok {
		return
	}
	out := apiPublish{PublishID: p.ID, PostID: p.DraftID, Version: p.Version, Attempt: p.Attempt, State: p.State, Key: p.Key(), ExpiresAt: p.ExpiresAt.UTC().Format(time.RFC3339)}
	if err := json.Unmarshal([]byte(p.Payload), &out.HookPayload); err != nil {
		s.internalError(w, err)
		return
	}
	if out.Cover != nil {
		out.Cover.Path = publishPath(p.ID) + "/cover"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiPublishCover(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	p, _, ok := s.ownPublish(w, r, tok)
	if !ok {
		return
	}
	mime, data, err := s.st.PublishCover(r.Context(), p)
	if err != nil {
		jsonError(w, http.StatusNotFound, "this publish has no cover")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) apiReportPublish(w http.ResponseWriter, r *http.Request, tok *store.AgentToken) {
	id, ok := pathID(r)
	if !ok {
		jsonError(w, http.StatusNotFound, "no such publish")
		return
	}
	var in store.PublishReport
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "send a JSON report: status, attempt, idempotency_key, content_sha256, pr, head_sha, merge_sha, code")
		return
	}
	state, err := s.st.ReportBlogPublish(r.Context(), tok, id, in)
	var ie *store.InputError
	switch {
	case errors.Is(err, store.ErrNotFound):
		jsonError(w, http.StatusNotFound, "no such publish")
		return
	case errors.As(err, &ie):
		jsonError(w, http.StatusBadRequest, ie.Msg)
		return
	case errors.Is(err, store.ErrNotInFlight), errors.Is(err, store.ErrReportMismatch):
		jsonError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.log.Printf("blog publish %d: token %d reported %s", id, tok.ID, in.Status)
	writeJSON(w, http.StatusOK, map[string]any{"state": state})
}
