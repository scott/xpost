package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/scott/xpost/internal/store"
)

// Import of a blog's existing live posts (slice 5): owner only, a dry run
// first, then an import of exactly the plan the owner saw. It reads the
// site through cfg.BlogImport (GET only, the import-only guarded client,
// and only when XPOST_BLOG_IMPORT=1) and never writes to the site,
// publishes, wakes an agent or creates a promo.

type importLineView struct {
	store.ImportLine
	Label, Cls string
	Name       string
	ChangedTxt string
}

type importData struct {
	B        *store.Blog
	On       bool   // a reader runs on this server
	Note     string // no call was made, and why
	Err      string
	Plan     *store.ImportPlan
	Lines    []importLineView
	Done     bool
	Act      int  // posts the import adds or adopts
	Changed  bool // the apply found a different plan
	Reads    string
	QueueURL string
}

// importOff is the page's word when the import is off on this server.
const importOff = "Import is off on this server, so it can't read the site. No call was made and nothing was imported."

func (s *Server) blogImportRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/blogs/{id}/import", s.requireOwner(s.blogImportPage))
	mux.HandleFunc("POST /settings/blogs/{id}/import", s.requireOwner(s.blogImportSubmit))
}

func (s *Server) importBlog(w http.ResponseWriter, r *http.Request) *store.Blog {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return nil
	}
	b, err := s.st.BlogByID(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return nil
	}
	return b
}

func (s *Server) showImport(w http.ResponseWriter, r *http.Request, u *store.User, status int, d importData) {
	d.On = s.cfg.BlogImport != nil
	if !d.On && d.Note == "" {
		d.Note = "Import is off on this server. Dry run stays off, and no call is made."
	}
	d.QueueURL = "/queue?kind=blog&blog=" + strconv.FormatInt(d.B.ID, 10) + "&status=posted"
	if d.B.Method == store.MethodWebhook {
		d.Reads = "One read of the site's post index, then one read of each listed post's page. No token or secret is sent."
	} else {
		d.Reads = "One authenticated read of the published posts on the blog's API."
	}
	if d.Plan != nil {
		d.Act = d.Plan.New + d.Plan.Adopted
		for _, ln := range d.Plan.Lines {
			v := importLineView{ImportLine: ln, Name: ln.Title}
			if v.Name == "" {
				v.Name = ln.Slug
			}
			if v.Name == "" {
				v.Name = "Untitled post"
			}
			switch ln.Action {
			case store.ImportNew:
				v.Label, v.Cls = "Import", "ok"
				if d.Done {
					v.Label = "Imported"
				}
			case store.ImportAdopt:
				v.Label, v.Cls = "Adopt", "ok"
				if d.Done {
					v.Label = "Adopted"
				}
			default:
				v.Label, v.Cls = "Skip", "neutral"
			}
			if len(ln.Changed) > 0 {
				v.ChangedTxt = "Changed on the site: " + strings.Join(ln.Changed, ", ") + ". xpost keeps its own copy."
			}
			d.Lines = append(d.Lines, v)
		}
	}
	p := s.newPage(r, u)
	p.Nav, p.Title = "settings", "Import posts"
	p.Data = d
	s.render(w, status, "blogimport", "app", p)
}

func (s *Server) blogImportPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	b := s.importBlog(w, r)
	if b == nil {
		return
	}
	s.showImport(w, r, u, http.StatusOK, importData{B: b})
}

// importErr is what the page says about a failed read: the adapter's
// status-only text, scrubbed again of anything the blog's secrets or a URL
// could leave in it.
func (s *Server) importErr(r *http.Request, b *store.Blog, err error) string {
	msg := s.st.CleanBlogTestDetail(r.Context(), b.ID, err.Error())
	if msg == "" {
		msg = "The site could not be read. Nothing was imported."
	}
	return msg
}

func (s *Server) blogImportSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	b := s.importBlog(w, r)
	if b == nil {
		return
	}
	ctx := r.Context()
	op := r.PostFormValue("op")
	if op != "dry" && op != "apply" {
		s.notFound(w, r)
		return
	}
	d := importData{B: b}
	if s.cfg.BlogImport == nil {
		d.Note = importOff
		s.showImport(w, r, u, http.StatusOK, d)
		return
	}
	ups, err := s.cfg.BlogImport(ctx, b)
	if errors.Is(err, store.ErrNoPublisher) {
		d.Note = "Import is not available for this blog's publish method on this server. No call was made."
		s.showImport(w, r, u, http.StatusOK, d)
		return
	}
	if err != nil {
		d.Err = s.importErr(r, b, err)
		s.log.Printf("settings: blog %d import read failed", b.ID)
		s.showImport(w, r, u, http.StatusBadGateway, d)
		return
	}
	if op == "dry" {
		plan, err := s.st.PlanImport(ctx, b, ups)
		if err != nil {
			if errors.Is(err, store.ErrImportTooMany) {
				d.Err = err.Error()
				s.showImport(w, r, u, http.StatusOK, d)
				return
			}
			s.internalError(w, err)
			return
		}
		d.Plan = plan
		s.showImport(w, r, u, http.StatusOK, d)
		return
	}
	plan, err := s.st.ApplyImport(ctx, u, b, ups, r.PostFormValue("key"), s.zone(r))
	switch {
	case errors.Is(err, store.ErrImportChanged):
		d.Plan, d.Changed = plan, true
		s.showImport(w, r, u, http.StatusConflict, d)
		return
	case errors.Is(err, store.ErrImportTooMany):
		d.Err = err.Error()
		s.showImport(w, r, u, http.StatusOK, d)
		return
	case errors.Is(err, store.ErrConflict):
		d.Err = "A draft changed while importing. Nothing was imported. Run the dry run again."
		s.showImport(w, r, u, http.StatusConflict, d)
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	s.log.Printf("settings: blog %d import by the owner: %d new, %d adopted, %d skipped", b.ID, plan.New, plan.Adopted, plan.Skip)
	d.Plan, d.Done = plan, true
	s.showImport(w, r, u, http.StatusOK, d)
}
