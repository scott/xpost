package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/scott/xpost/internal/auth"
)

type ctxKey int

const csrfKey ctxKey = 1

// csrfProtect gives every browser a random CSRF cookie and requires a
// matching HMAC token in the "csrf" form field on every unsafe request. It
// also refuses cross-site requests by Sec-Fetch-Site and Origin.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieVal := ""
		if c, err := r.Cookie(CSRFCookie); err == nil && len(c.Value) >= 40 {
			cookieVal = c.Value
		}
		if cookieVal == "" {
			tok, err := auth.NewToken()
			if err != nil {
				s.internalError(w, err)
				return
			}
			cookieVal = tok
			s.setCookie(w, CSRFCookie, cookieVal, 0)
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
				http.Error(w, "Cross-site request refused.", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "Cross-origin request refused.", http.StatusForbidden)
					return
				}
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
				if err := r.ParseMultipartForm(8 << 20); err != nil {
					var mbe *http.MaxBytesError
					if errors.As(err, &mbe) {
						http.Error(w, "That upload is too large. Images can be up to 5 MB each.", http.StatusRequestEntityTooLarge)
						return
					}
				}
			}
			if !s.csrf.Valid(cookieVal, r.PostFormValue("csrf")) {
				http.Error(w, "Your form expired. Go back, reload the page and try again.", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), csrfKey, s.csrf.Token(cookieVal))))
	})
}

func csrfToken(r *http.Request) string {
	v, _ := r.Context().Value(csrfKey).(string)
	return v
}
