package web

import (
	"errors"
	"net/http"

	"github.com/scott/xpost/internal/store"
)

var errNoSession = errors.New("no session")

func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// stepSession returns a session in the given stage.
func (s *Server) stepSession(r *http.Request, stage string) (*store.Session, *store.User, error) {
	look := s.st.LookupSession
	if isLive(r) {
		look = s.st.PeekSession // live traffic is not owner activity
	}
	ss, u, err := look(r.Context(), sessionToken(r))
	if err != nil || ss.Stage != stage {
		return nil, nil, errNoSession
	}
	return ss, u, nil
}

// fullSession returns the owner only for a password + passkey session.
func (s *Server) fullSession(r *http.Request) (*store.Session, *store.User, error) {
	return s.stepSession(r, store.StageFull)
}

type ownerHandler func(http.ResponseWriter, *http.Request, *store.User)

// requireOwner runs h only for a full owner session. Every review action
// goes through it, so an agent token (which is never a session) or a
// half-finished sign-in can never reach one. Bearer headers are ignored.
func (s *Server) requireOwner(h ownerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, u, err := s.fullSession(r)
		if err != nil {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			http.Error(w, "Please sign in again.", http.StatusUnauthorized)
			return
		}
		h(w, r, u)
	}
}
