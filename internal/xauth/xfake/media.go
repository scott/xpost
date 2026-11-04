package xfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Media is an upload the fake holds.
type Media struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Mime     string    `json:"mime"`
	Category string    `json:"media_category"`
	Size     int       `json:"size"`
	Alt      string    `json:"alt,omitempty"`
	Expires  time.Time `json:"expires"`
	checks   int       // status checks left before processing succeeds
	failProc bool      // processing ends in "failed"
}

// MediaFault is a queued behaviour for the next media call of one kind.
// Kind reuses the create-post faults: Status, RateLimit, DropAfterCreate
// (stores the upload, then drops the connection: the caller can't know),
// DropBeforeCreate, Hang (Create stores it first) and Garbled. Process
// makes the upload report "pending" for Checks status checks (FailProcess:
// then "failed").
type MediaFault struct {
	Fault
	Process     bool
	Checks      int
	FailProcess bool
}

// Media call targets for QueueMedia.
const (
	UploadCall   = "upload"
	MetadataCall = "metadata"
	StatusCall   = "status"
)

// QueueMedia appends faults for the next calls of one target.
func (s *Server) QueueMedia(target string, f ...MediaFault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	s.mediaFaults[target] = append(s.mediaFaults[target], f...)
}

// AllMedia returns the uploads the fake holds, oldest first.
func (s *Server) AllMedia() []Media {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Media
	for _, id := range s.mediaOrder {
		out = append(out, *s.media[id])
	}
	return out
}

// ExpireMedia makes every upload so far expired.
func (s *Server) ExpireMedia() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.media {
		m.Expires = s.now().Add(-time.Second)
	}
}

func (s *Server) nextMediaFault(target string) MediaFault {
	var f MediaFault
	if q := s.mediaFaults[target]; len(q) > 0 {
		f, s.mediaFaults[target] = q[0], q[1:]
	}
	return f
}

// mediaAuth checks the bearer token has media.write; the caller holds s.mu.
func (s *Server) mediaAuth(w http.ResponseWriter, r *http.Request) (grant, bool) {
	at, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.access[at]
	if !ok || !s.now().Before(g.exp) {
		xProblem(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
		return g, false
	}
	if !strings.Contains(" "+g.scope+" ", " media.write ") {
		xProblem(w, http.StatusForbidden, "Forbidden", "missing media.write scope")
		return g, false
	}
	return g, true
}

// applyFault runs the fault kinds that end the call before (or instead of)
// a normal reply. It unlocks s.mu when it returns true.
func (s *Server) applyFault(w http.ResponseWriter, f Fault) bool {
	switch f.Kind {
	case Status:
		s.mu.Unlock()
		xProblem(w, f.Status, http.StatusText(f.Status), "fake x: injected failure")
		return true
	case RateLimit:
		s.mu.Unlock()
		w.Header().Set("x-rate-limit-limit", "100")
		w.Header().Set("x-rate-limit-remaining", "0")
		w.Header().Set("x-rate-limit-reset", fmt.Sprint(f.Reset.Unix()))
		xProblem(w, http.StatusTooManyRequests, "Too Many Requests", "Too Many Requests")
		return true
	case DropBeforeCreate:
		s.mu.Unlock()
		hijackClose(w)
		return true
	}
	return false
}

var allowedImage = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// upload is POST /2/media/upload (multipart: media, media_category). It
// checks the category against the sniffed type and X's size limits.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	s.UploadCalls++
	g, ok := s.mediaAuth(w, r)
	if !ok {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	bad := func(detail string) { xProblem(w, http.StatusBadRequest, "Invalid Request", detail) }
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		bad("expected multipart/form-data")
		return
	}
	cat := r.FormValue("media_category")
	file, _, err := r.FormFile("media")
	if err != nil {
		bad("media is required")
		return
	}
	data, _ := io.ReadAll(io.LimitReader(file, 16<<20))
	file.Close()
	mime := http.DetectContentType(data)
	switch {
	case cat != "tweet_image" && cat != "tweet_gif":
		bad("media_category must be tweet_image or tweet_gif for a post")
		return
	case !allowedImage[mime]:
		bad("unsupported media type")
		return
	case (mime == "image/gif") != (cat == "tweet_gif"):
		bad("media_category does not match the file")
		return
	case cat == "tweet_image" && len(data) > 5<<20, len(data) > 15<<20:
		bad("file is too large")
		return
	}
	s.mu.Lock()
	f := s.nextMediaFault(UploadCall)
	if s.applyFault(w, f.Fault) {
		return
	}
	m := &Media{ID: fmt.Sprint(s.nextMedia), Username: g.username, Mime: mime, Category: cat, Size: len(data), Expires: s.now().Add(s.mediaTTL())}
	s.nextMedia++
	if f.Process {
		m.checks, m.failProc = max(f.Checks, 1), f.FailProcess
	}
	s.media[m.ID] = m
	s.mediaOrder = append(s.mediaOrder, m.ID)
	cp := *m
	s.mu.Unlock()
	switch f.Kind {
	case DropAfterCreate:
		hijackClose(w)
		return
	case Garbled:
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"id":`)
		return
	case Hang:
		if f.Arrived != nil {
			close(f.Arrived)
		}
		select {
		case <-f.Release:
		case <-r.Context().Done():
			return
		}
	}
	s.writeMedia(w, &cp)
}

func (s *Server) mediaTTL() time.Duration {
	if s.MediaTTL > 0 {
		return s.MediaTTL
	}
	return 24 * time.Hour
}

func (s *Server) writeMedia(w http.ResponseWriter, m *Media) {
	d := map[string]any{"id": m.ID, "media_key": "3_" + m.ID, "size": m.Size, "expires_after_secs": int(time.Until(m.Expires) / time.Second)}
	if s.Now != nil {
		d["expires_after_secs"] = int(m.Expires.Sub(s.now()) / time.Second)
	}
	switch {
	case m.checks > 0:
		d["processing_info"] = map[string]any{"state": "pending", "check_after_secs": 1}
	case m.failProc:
		d["processing_info"] = map[string]any{"state": "failed"}
	case m.Category == "tweet_gif":
		d["processing_info"] = map[string]any{"state": "succeeded", "progress_percent": 100}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": d})
}

// status is GET /2/media/upload?command=STATUS&media_id=.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	s.StatusCalls++
	g, ok := s.mediaAuth(w, r)
	if !ok {
		s.mu.Unlock()
		return
	}
	if r.URL.Query().Get("command") != "STATUS" {
		s.mu.Unlock()
		xProblem(w, http.StatusBadRequest, "Invalid Request", "command must be STATUS")
		return
	}
	f := s.nextMediaFault(StatusCall)
	if s.applyFault(w, f.Fault) {
		return
	}
	m, ok := s.media[r.URL.Query().Get("media_id")]
	if !ok || m.Username != g.username || !s.now().Before(m.Expires) {
		s.mu.Unlock()
		xProblem(w, http.StatusNotFound, "Not Found Error", "media not found")
		return
	}
	if m.checks > 0 {
		m.checks--
	}
	cp := *m
	s.mu.Unlock()
	s.writeMedia(w, &cp)
}

// metadata is POST /2/media/metadata: {id, metadata: {alt_text: {text}}}.
func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.init()
	s.MetadataCalls++
	g, ok := s.mediaAuth(w, r)
	if !ok {
		s.mu.Unlock()
		return
	}
	var body struct {
		ID       string `json:"id"`
		Metadata struct {
			AltText struct {
				Text string `json:"text"`
			} `json:"alt_text"`
		} `json:"metadata"`
	}
	if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil || len([]rune(body.Metadata.AltText.Text)) > 1000 {
		s.mu.Unlock()
		xProblem(w, http.StatusBadRequest, "Invalid Request", "One or more parameters to your request was invalid.")
		return
	}
	f := s.nextMediaFault(MetadataCall)
	if s.applyFault(w, f.Fault) {
		return
	}
	m, ok := s.media[body.ID]
	if !ok || m.Username != g.username || !s.now().Before(m.Expires) {
		s.mu.Unlock()
		xProblem(w, http.StatusNotFound, "Not Found Error", "media not found")
		return
	}
	m.Alt = body.Metadata.AltText.Text
	s.mu.Unlock()
	if f.Kind == DropAfterCreate {
		hijackClose(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": body.ID, "associated_metadata": map[string]any{"alt_text": map[string]string{"text": body.Metadata.AltText.Text}}}})
}

// checkPostMedia validates media_ids on a create-post call: each must be
// this user's, unexpired and processed; up to 4 images or 1 GIF. The
// caller holds s.mu. It returns a problem detail, or "".
func (s *Server) checkPostMedia(username string, ids []string) string {
	if len(ids) > 4 {
		return "Your media IDs are invalid."
	}
	gifs := 0
	for _, id := range ids {
		m, ok := s.media[id]
		if !ok || m.Username != username || !s.now().Before(m.Expires) || m.checks > 0 || m.failProc {
			return "Your media IDs are invalid."
		}
		if m.Category == "tweet_gif" {
			gifs++
		}
	}
	if gifs > 0 && len(ids) > 1 {
		return "Your media IDs are invalid."
	}
	return ""
}
