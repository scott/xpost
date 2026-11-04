// Package hfake is a fake webhook-blog agent and site for tests and local
// screenshots: it receives wakes (checking their signature) and serves the
// pages the test "deploys". It only serves; it never makes a request.
package hfake

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Wake is one wake the fake received.
type Wake struct {
	Event, Delivery, Auth string
	SigOK                 bool
	Body                  map[string]any
	Raw                   string
}

// Server is the fake. Key is the sender key it checks signatures with.
type Server struct {
	Key string

	mu       sync.Mutex
	status   int               // the wake answer; 0 = 202
	pages    map[string]string // path -> HTML
	redirect map[string]bool
	files    map[string][2]string // path -> content type, body
	wakes    []Wake
	gets     []string // "GET path auth=<bool>"
}

// SetStatus sets what the fake answers wakes with (0: 202).
func (s *Server) SetStatus(code int) { s.mu.Lock(); s.status = code; s.mu.Unlock() }

// Deploy serves html at path (e.g. "/blog/sample-week/").
func (s *Server) Deploy(path, html string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pages == nil {
		s.pages = map[string]string{}
	}
	s.pages[path] = html
}

// Serve answers GET path with body as ctype (e.g. a posts.json index).
func (s *Server) Serve(path, ctype, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][2]string{}
	}
	s.files[path] = [2]string{ctype, body}
}

// Redirect makes path answer 301 to path without its trailing slash.
func (s *Server) Redirect(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redirect == nil {
		s.redirect = map[string]bool{}
	}
	s.redirect[path] = true
}

// Wakes lists the wakes received.
func (s *Server) Wakes() []Wake {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Wake(nil), s.wakes...)
}

// Gets lists the page requests.
func (s *Server) Gets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.gets...)
}

// Page is a minimal post page carrying marker in its head.
func Page(title, marker, body string) string {
	return "<!doctype html><html lang=\"en\"><head><title>" + title + "</title>" + marker + "</head><body><article>" + body + "</article></body></html>"
}

func sign(key, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Handler serves POST /hook (wakes) and GET of deployed pages.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/hook" {
			raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			wk := Wake{Event: r.Header.Get("X-Xpost-Event"), Delivery: r.Header.Get("X-Xpost-Delivery"), Auth: r.Header.Get("Authorization"), Raw: string(raw)}
			ts := r.Header.Get("X-Xpost-Timestamp")
			if _, err := strconv.ParseInt(ts, 10, 64); err == nil && s.Key != "" {
				wk.SigOK = hmac.Equal([]byte(sign(s.Key, ts, raw)), []byte(r.Header.Get("X-Xpost-Signature")))
			}
			json.Unmarshal(raw, &wk.Body)
			s.wakes = append(s.wakes, wk)
			code := s.status
			if code == 0 {
				code = http.StatusAccepted
			}
			w.WriteHeader(code)
			return
		}
		if r.Method == http.MethodGet {
			s.gets = append(s.gets, "GET "+r.URL.Path+" auth="+strconv.FormatBool(r.Header.Get("Authorization") != ""))
			if s.redirect[r.URL.Path] {
				w.Header().Set("Location", strings.TrimSuffix(r.URL.Path, "/"))
				w.WriteHeader(http.StatusMovedPermanently)
				return
			}
			if f, ok := s.files[r.URL.Path]; ok {
				w.Header().Set("Content-Type", f[0])
				io.WriteString(w, f[1])
				return
			}
			if html, ok := s.pages[r.URL.Path]; ok {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, html)
				return
			}
		}
		http.NotFound(w, r)
	})
}
