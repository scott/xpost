package web

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

// page is what every template gets.
type page struct {
	Title string
	Nav   string // active nav item
	Back  string // phone header back link, if any
	// CancelForm puts a Cancel button for that form in the phone header
	// (the composer); NewPost puts the phone header's "+".
	CancelForm string
	NewPost    string
	User       *store.User
	CSRF       string
	Error      string
	Flash      string
	Demo       bool
	Pending    int
	Accounts   []store.Account
	Blogs      []*store.Blog // the sidebar's Blogs list
	Data       any
	// Live updates (static/live.js): the page kind ("queue", "schedule",
	// "log", or "draft" for one draft's page), the change feed position it
	// was rendered at, and for a draft page its id and version.
	Live      string
	LiveSeq   int64
	LiveDraft int64
	LiveV     int
}

// setLive marks a GET page as live. seq must be read before the page's
// data, so nothing that changes during the render is missed.
func setLive(r *http.Request, p *page, status int, kind string, seq int64) {
	if r.Method == http.MethodGet && status == http.StatusOK && seq >= 0 {
		p.Live, p.LiveSeq = kind, seq
	}
}

func (s *Server) newPage(r *http.Request, u *store.User) *page {
	p := &page{User: u, CSRF: csrfToken(r), Demo: s.cfg.Demo}
	if u != nil {
		p.Pending = s.st.PendingCount(r.Context())
		p.Accounts, _ = s.st.Accounts(r.Context())
		p.Blogs, _ = s.st.Blogs(r.Context())
	}
	return p
}

// zone is the owner's display zone.
func (s *Server) zone(r *http.Request) *time.Location { return s.st.Zone(r.Context(), s.cfg.Location) }

func money(micros int64) string {
	// Show three decimals under a dollar (posts cost fractions of a cent).
	d := float64(micros) / 1e6
	if micros != 0 && micros%10_000 != 0 && micros < 1_000_000 {
		return fmt.Sprintf("$%.3f", d)
	}
	return fmt.Sprintf("$%.2f", d)
}

func cents(c int64) string { return fmt.Sprintf("$%d.%02d", c/100, c%100) }

var icons = map[string]string{
	"inbox":     `<path d="M3 13h5l1.5 3h5L16 13h5"/><path d="M5.5 5h13L21 13v6H3v-6z"/>`,
	"cal":       `<rect x="3.5" y="5" width="17" height="15" rx="2"/><path d="M3.5 10h17M8 3v4M16 3v4"/>`,
	"log":       `<path d="M8 6h12M8 12h12M8 18h12"/><circle cx="4" cy="6" r="1"/><circle cx="4" cy="12" r="1"/><circle cx="4" cy="18" r="1"/>`,
	"at":        `<circle cx="12" cy="12" r="4"/><path d="M16 8v5a3 3 0 0 0 6 0v-1a10 10 0 1 0-4 8"/>`,
	"menu":      `<path d="M4 7h16M4 12h16M4 17h16"/>`,
	"gear":      `<circle cx="12" cy="12" r="3"/><path d="M12 2.5v3M12 18.5v3M2.5 12h3M18.5 12h3M5.3 5.3l2.1 2.1M16.6 16.6l2.1 2.1M5.3 18.7l2.1-2.1M16.6 7.4l2.1-2.1"/>`,
	"check":     `<path d="M5 12.5l4.5 4.5L19 7.5"/>`,
	"x":         `<path d="M6 6l12 12M18 6L6 18"/>`,
	"edit":      `<path d="M4 20h4L19 9l-4-4L4 16z"/><path d="M13.5 6.5l4 4"/>`,
	"clock":     `<circle cx="12" cy="12" r="8.5"/><path d="M12 7.5V12l3 2"/>`,
	"img":       `<rect x="3.5" y="4.5" width="17" height="15" rx="2"/><circle cx="9" cy="10" r="1.6"/><path d="M20.5 16l-5-5-9 8.5"/>`,
	"thread":    `<path d="M7 4v16M7 8h8a3 3 0 0 1 0 6H7"/>`,
	"link":      `<path d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1"/><path d="M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"/>`,
	"warn":      `<path d="M12 4l9 16H3z"/><path d="M12 10v4M12 17v.5"/>`,
	"info":      `<circle cx="12" cy="12" r="8.5"/><path d="M12 11v5M12 8v.5"/>`,
	"ext":       `<path d="M14 4h6v6M20 4l-9 9"/><path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>`,
	"key":       `<circle cx="8" cy="15" r="4"/><path d="M11 12l9-9M17 6l3 3M15 8l2 2"/>`,
	"plus":      `<path d="M12 5v14M5 12h14"/>`,
	"back":      `<path d="M15 5l-7 7 7 7"/>`,
	"robot":     `<rect x="5" y="8" width="14" height="11" rx="3"/><path d="M12 4v4M9 13v1M15 13v1"/>`,
	"lock":      `<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8 11V8a4 4 0 0 1 8 0v3"/>`,
	"chev":      `<path d="M9 5l7 7-7 7"/>`,
	"dollar":    `<path d="M12 3v18M16.5 7.5C16 6 14.3 5 12 5 9.5 5 7.5 6.3 7.5 8.3c0 4.7 9 2.7 9 7.4 0 2-2 3.3-4.5 3.3-2.4 0-4.2-1-4.8-2.7"/>`,
	"out":       `<path d="M15 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4M10 8l-4 4 4 4M6 12h10"/>`,
	"up":        `<path d="M12 19V5M6 11l6-6 6 6"/>`,
	"rewrite":   `<path d="M4 20h4L18 10l-4-4L4 16z"/><path d="M12.5 7.5l4 4"/><path d="M18 2.5l.9 1.9 1.9.9-1.9.9-.9 1.9-.9-1.9-1.9-.9 1.9-.9z"/>`,
	"send":      `<path d="M4 12l16-8-6 16-2.5-6.5z"/><path d="M11.5 13.5L20 4"/>`,
	"hourglass": `<path d="M7 3h10M7 21h10M8 3c0 5 8 5 8 9s-8 4-8 9M16 3c0 5-8 5-8 9"/>`,
	"retry":     `<path d="M20 11a8 8 0 1 0-2.3 5.7"/><path d="M20 4v7h-7"/>`,
	"code":      `<path d="M9 8l-4 4 4 4M15 8l4 4-4 4"/>`,
	"down":      `<path d="M5 9l7 7 7-7"/>`,
	"doc":       `<path d="M6 3h8l4 4v14H6z"/><path d="M14 3v4h4M9 12h6M9 16h6"/>`,
	"book":      `<path d="M4 5.5A2.5 2.5 0 0 1 6.5 3H20v15H6.5A2.5 2.5 0 0 0 4 20.5z"/><path d="M4 20.5A2.5 2.5 0 0 0 6.5 23H20v-5"/>`,
	"bolt":      `<path d="M13 3L5 14h6l-1 7 8-11h-6z"/>`,
	"eye":       `<path d="M2.5 12S6 5.5 12 5.5 21.5 12 21.5 12 18 18.5 12 18.5 2.5 12 2.5 12z"/><circle cx="12" cy="12" r="3"/>`,
	"import":    `<path d="M12 4v11M7 10l5 5 5-5M5 20h14"/>`,
	"reply":     `<path d="M10 7l-5 5 5 5"/><path d="M5 12h9a5 5 0 0 1 5 5v2"/>`,
	"copy":      `<rect x="8.5" y="8.5" width="11" height="11" rx="2"/><path d="M15.5 8.5V6a1.5 1.5 0 0 0-1.5-1.5H6A1.5 1.5 0 0 0 4.5 6v8A1.5 1.5 0 0 0 6 15.5h2.5"/>`,
	"globe":     `<circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17M12 3.5c2.5 2.5 3.5 5.5 3.5 8.5s-1 6-3.5 8.5c-2.5-2.5-3.5-5.5-3.5-8.5s1-6 3.5-8.5z"/>`,
}

func icon(name string) template.HTML {
	return template.HTML(`<svg class="i" viewBox="0 0 24 24" aria-hidden="true">` + icons[name] + `</svg>`)
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		// lvl packs a lint rule select's name, label and value.
		"lvl":  func(n, l, v string) map[string]string { return map[string]string{"N": n, "L": l, "V": v} },
		"brk":  nameBreaks,
		"icon": icon,
		"inc":  func(i int) int { return i + 1 },
		// blogsOn: the optional blog feature is on (XPOST_BLOGS_ENABLED).
		"blogsOn": func() bool { return s.cfg.BlogsEnabled },
		"money":   money,
		"cents":   cents,
		"lower":   strings.ToLower,
		"initial": func(name string) string {
			for _, r := range strings.TrimSpace(name) {
				return strings.ToUpper(string(r))
			}
			return "?"
		},
		"splitScopes": func(s string) []string { return strings.Fields(s) },
		"seq": func(n int) []int {
			out := make([]int, n)
			for i := range out {
				out[i] = i
			}
			return out
		},
	}
	s.pages = map[string]*template.Template{}
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(n, "templates/"), ".html")
		if base == "layout" || base == "partials" || base == "blogparts" {
			continue
		}
		t, err := template.New(base).Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials.html", "templates/blogparts.html", n)
		if err != nil {
			return fmt.Errorf("template %s: %w", n, err)
		}
		s.pages[base] = t
	}
	return nil
}

// render executes a page inside the "app" (signed-in) or "auth" layout.
func (s *Server) render(w http.ResponseWriter, status int, name, layout string, p *page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "missing template "+name, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, layout, p); err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	_, u, _ := s.fullSession(r)
	p := s.newPage(r, u)
	p.Title = "Not found"
	layout := "auth"
	if u != nil {
		layout = "app"
	}
	s.render(w, http.StatusNotFound, "notfound", layout, p)
}

func (s *Server) notice(w http.ResponseWriter, r *http.Request, status int, title, body string) {
	p := s.newPage(r, nil)
	p.Title = title
	p.Data = map[string]string{"Title": title, "Body": body}
	s.render(w, status, "notice", "auth", p)
}

// nameBreaks escapes a name and lets it break after its dots, slashes,
// hyphens and underscores (<wbr>), so a long domain-like blog name wraps
// at a natural point rather than mid-word. Spaces still break as usual.
func nameBreaks(name string) template.HTML {
	var b strings.Builder
	start := 0
	for i, r := range name {
		if r == '.' || r == '/' || r == '-' || r == '_' {
			b.WriteString(template.HTMLEscapeString(name[start : i+1]))
			if i+1 < len(name) {
				b.WriteString("<wbr>")
			}
			start = i + 1
		}
	}
	b.WriteString(template.HTMLEscapeString(name[start:]))
	return template.HTML(b.String())
}
