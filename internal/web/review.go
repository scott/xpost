package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

func (s *Server) reviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /queue", s.requireOwner(s.queuePage))
	mux.HandleFunc("GET /drafts/{id}", s.requireOwner(s.editPage))
	mux.HandleFunc("POST /drafts/{id}", s.requireOwner(s.editSubmit))
	mux.HandleFunc("POST /drafts/{id}/approve", s.requireOwner(s.approve))
	mux.HandleFunc("POST /drafts/{id}/reject", s.requireOwner(s.reject))
	mux.HandleFunc("POST /drafts/{id}/delete", s.requireOwner(s.deleteDraft))
	mux.HandleFunc("GET /drafts/{id}/reschedule", s.requireOwner(s.reschedulePage))
	mux.HandleFunc("POST /drafts/{id}/reschedule", s.requireOwner(s.rescheduleSubmit))
	mux.HandleFunc("GET /media/{ref}", s.requireOwner(s.media))
}

// ---- view models ----

type whenView struct {
	Day, Time, Zone, Date, Clock string // "Mon, Nov 2", "9:00 AM", "MST", "2026-11-02", "09:00"
	Past                         bool
}

func (s *Server) when(t time.Time, loc *time.Location) whenView {
	l := t.In(loc)
	return whenView{Day: l.Format("Mon, Jan 2"), Time: l.Format("3:04 PM"), Zone: l.Format("MST"), Date: l.Format("2006-01-02"), Clock: l.Format("15:04"), Past: !t.After(s.st.Now())}
}

type conflictView struct {
	Msg       string
	Blocking  bool // an approved neighbor: approval is refused until moved
	MoveDate  string
	MoveClock string
	MoveLabel string
}

type cardView struct {
	D          *store.Draft
	Acct       store.Account
	Preview    string
	Full       []fullPart // every part's full text, for the queue and reschedule
	PartsLabel string
	Images     int
	HasURL     bool
	Cost       string
	Thumbs     []store.MediaRef
	Received   string
	When       whenView
	Conflicts  []conflictView
	Blocked    string       // why Approve is unavailable, if it is
	Posted     []postedLink // posted drafts: every part's links; When is the posted time
	// Rewrite (pending agent drafts only)
	Agent      *agentView
	CanRewrite bool    // show the Rewrite button
	RwOff      string  // why Rewrite is disabled (tooltip), if it is
	RwHint     string  // hint under the text when Rewrite is off
	RwLink     string  // its Settings link text
	RwWarn     string  // the webhook failed its last test
	RwOpen     bool    // the note box is open
	RwHref     string  // opens the note box
	RwClose    string  // closes it
	RW         *rwView // the latest request's outcome
	// Blog posts
	Blog  *blogView
	Promo *promoView // X posts promoting a blog post
	Reply *replyView // replies: posted by hand on x.com
	CSRF  string     // cards render from a partial: the form fields
	Ret   string
	// Queue only: a blog card's promos on the current status tab as full
	// cards nested under it, the flag those nested cards carry, and links
	// to its promos that live on other tabs (QueueNest marks a queue card,
	// which never falls back to the compact promo list).
	Promos    []cardView
	Nested    bool
	PromoElse []promoElse
	QueueNest bool
}

func (s *Server) accountMap(r *http.Request) map[string]store.Account {
	accts, _ := s.st.Accounts(r.Context())
	m := map[string]store.Account{}
	for _, a := range accts {
		m[a.Handle] = a
	}
	return m
}

func (s *Server) draftCost(d *store.Draft) int64 { return s.st.PartsEstimate(d.Parts) }

// costText formats an estimate that may include image uploads and alt text
// calls: unverified when the post rates are not set, or when it includes
// uploads (alt text calls) and the upload (metadata) rate is not set.
func (s *Server) costText(micros int64, images, alts int) string {
	if images > 0 && !s.st.Rates.UploadKnown || alts > 0 && !s.st.Rates.AltKnown {
		return "unverified"
	}
	return s.rateText(micros)
}

func partsLabel(n int) string {
	if n == 1 {
		return "Single post"
	}
	return fmt.Sprintf("Thread · %d parts", n)
}

func (s *Server) received(t time.Time, loc *time.Location) string {
	l, now := t.In(loc), s.st.Now().In(loc)
	if l.Year() == now.Year() && l.YearDay() == now.YearDay() {
		return l.Format("3:04 PM")
	}
	return l.Format("Jan 2, 3:04 PM")
}

func (s *Server) conflicts(r *http.Request, d *store.Draft, at time.Time, loc *time.Location) []conflictView {
	var out []conflictView
	for _, n := range s.st.Neighbors(r.Context(), d.Account, at, d.ID) {
		gap := n.At.Sub(at)
		rel := "after"
		if gap > 0 {
			rel = "before"
		} else {
			gap = -gap
		}
		kind := "an approved"
		if n.Status == store.StatusPending {
			kind = "a pending"
		} else if n.Status != store.StatusApproved {
			kind = "a " + n.Status
		}
		c := conflictView{Blocking: n.Status != store.StatusPending,
			Msg: fmt.Sprintf("Only %d min %s %s @%s post at %s. Minimum is %d min.", int(gap.Minutes()), rel, kind, d.Account, n.At.In(loc).Format("3:04 PM"), int(store.MinSpacing.Minutes()))}
		if free := s.moveTarget(r.Context(), d.Account, at, d.ID); !free.IsZero() {
			w := s.when(free, loc)
			c.MoveDate, c.MoveClock, c.MoveLabel = w.Date, w.Clock, w.Time
			if w.Day != s.when(at, loc).Day {
				c.MoveLabel = w.Day + " " + w.Time
			}
		}
		out = append(out, c)
	}
	return out
}

func preview(d *store.Draft) string {
	if len(d.Parts) == 0 {
		return ""
	}
	return d.Parts[0].Text
}

// fullPart is one part's full text; Label is "1/3" on threads, empty on a
// single post.
type fullPart struct {
	Label, Text string
	Segs        []seg // set on revised drafts and in the previous version: marked words
}

func fullParts(d *store.Draft) []fullPart {
	out := make([]fullPart, 0, len(d.Parts))
	for i, p := range d.Parts {
		fp := fullPart{Text: p.Text, Segs: blogURLSegs(p.Text)}
		if len(d.Parts) > 1 {
			fp.Label = strconv.Itoa(i+1) + "/" + strconv.Itoa(len(d.Parts))
		}
		out = append(out, fp)
	}
	return out
}

func (s *Server) card(r *http.Request, d *store.Draft, accts map[string]store.Account, loc *time.Location) cardView {
	if d.IsBlog() {
		c := cardView{D: d, Blog: s.blogView(r, d, loc), Received: s.received(d.CreatedAt, loc), When: s.when(d.ScheduledAt, loc)}
		if c.Blog.Imported {
			// An imported post has a day only: the blog's, with no clock.
			c.When = s.when(d.ScheduledAt, c.Blog.B.Location(loc))
			c.When.Time, c.When.Clock = "", ""
		}
		if (d.Status == store.StatusPending || d.Status == store.StatusApproved) && c.When.Past {
			c.Blocked = "That time has passed. Reschedule it first."
		}
		return c
	}
	c := cardView{D: d, Acct: accts[d.Account], Preview: preview(d), Full: fullParts(d), PartsLabel: partsLabel(len(d.Parts)), Images: d.Images(), HasURL: d.HasURL(),
		Cost: s.costText(s.draftCost(d), d.Images(), d.Alts()), Received: s.received(d.CreatedAt, loc), When: s.when(d.ScheduledAt, loc)}
	for _, p := range d.Parts {
		for _, m := range p.Media {
			if len(c.Thumbs) < 4 {
				c.Thumbs = append(c.Thumbs, m)
			}
		}
	}
	if d.Status == store.StatusPosted {
		var at time.Time
		c.Posted, at = s.postedLinks(r.Context(), d)
		c.When = s.when(at, loc)
		if parts, ok, _ := s.st.PromoText(r.Context(), d); ok && d.IsPromo() {
			c.Full = fullParts(&store.Draft{Parts: parts})
		}
	}
	if d.IsReply() {
		s.replyCard(r, &c, loc)
		return c
	}
	c.Promo = s.promoFor(r, d, loc)
	if d.Status == store.StatusPending || d.Status == store.StatusApproved {
		c.Conflicts = s.conflicts(r, d, d.ScheduledAt, loc)
		for _, x := range c.Conflicts {
			if x.Blocking {
				c.Blocked = "Move it first: too close to an approved post."
			}
		}
		if c.When.Past {
			c.Blocked = "That time has passed. Reschedule it first."
		}
		if c.Promo != nil && c.Promo.Rejected {
			c.Blocked = "Its blog post was rejected, so this promo can't be approved."
		}
	}
	return c
}

// ---- queue ----

type chip struct {
	Label, Href, Initials, Color string
	Count                        int
	On                           bool
}

func (s *Server) queuePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	s.showQueue(w, r, u, http.StatusOK, "", queueFlash[r.URL.Query().Get("done")])
}

func (s *Server) showQueue(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash string) {
	seq := s.liveSeqNow(r)
	ctx := r.Context()
	q := r.URL.Query()
	st, acct, kind := q.Get("status"), q.Get("account"), q.Get("kind")
	switch st {
	case store.StatusApproved, store.StatusRejected, store.StatusPosted:
	default:
		st = store.StatusPending
	}
	// The bare /queue (no status or filter in the URL) shows what is
	// approved today, today first, once nothing is left to review.
	todayView := false
	start, end := todayBounds(s.st.Now(), s.zone(r))
	if !q.Has("status") && kind == "" && acct == "" && q.Get("blog") == "" {
		if all, _ := s.st.QueueCounts(ctx, "", ""); all.ByStatus[store.StatusPending] == 0 && all.ByStatus[store.StatusApproved] > 0 {
			if appr, err := s.st.Queue(ctx, store.Filter{Status: store.StatusApproved}); err == nil {
				for _, d := range appr {
					if approvedToday(d, start, end) {
						st, todayView = store.StatusApproved, true
						break
					}
				}
			}
		}
	}
	accts := s.accountMap(r)
	if _, ok := accts[acct]; !ok {
		acct = ""
	}
	blogs, _ := s.st.Blogs(ctx)
	replies := s.st.HasReplies(ctx)
	switch {
	case kind == store.KindBlog && len(blogs) > 0:
	case kind == store.KindReply && replies:
	case kind == store.KindX && (len(blogs) > 0 || replies):
	default:
		kind = ""
	}
	if acct != "" && kind != store.KindReply {
		kind = store.KindX // account chips are X accounts
	}
	// ?blog=<id> narrows the Blog view to one blog (from Settings > Blogs).
	var oneBlog *store.Blog
	if id, err := strconv.ParseInt(q.Get("blog"), 10, 64); err == nil && acct == "" {
		for _, b := range blogs {
			if b.ID == id {
				oneBlog, kind, acct = b, store.KindBlog, b.Account
			}
		}
	}
	drafts, err := s.st.Queue(ctx, store.Filter{Status: st, Account: acct, Kind: kind})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if todayView {
		todayFirst(drafts, start, end)
	}
	counts, _ := s.st.QueueCounts(ctx, acct, kind)
	loc := s.zone(r)
	agents := s.agentViews(r)
	var cards []cardView
	pub, _ := strconv.ParseInt(q.Get("publish"), 10, 64)
	// Promos sit under their blog's card when it is on this page.
	onPage := map[int64]bool{}
	for _, d := range drafts {
		if d.IsBlog() {
			onPage[d.ID] = true
		}
	}
	for _, d := range drafts {
		if d.IsPromo() && onPage[d.Promotes] {
			continue
		}
		c := s.card(r, d, accts, loc)
		c.CSRF, c.Ret = csrfToken(r), r.URL.RequestURI()
		if c.Blog != nil {
			c.Promos, c.PromoElse = s.nestedPromos(r, d, st, accts, agents, loc)
			c.QueueNest = true
			c.Blog.PubHref = withQuery(withQuery(r.URL.RequestURI(), "done", ""), "publish", strconv.FormatInt(d.ID, 10)) + "#d" + strconv.FormatInt(d.ID, 10)
			c.Blog.PubClose = withQuery(withQuery(r.URL.RequestURI(), "done", ""), "publish", "") + "#d" + strconv.FormatInt(d.ID, 10)
			c.Blog.PubOpen = pub == d.ID && c.Blog.Off == "" && c.Blog.PubOff == "" && (d.Status == store.StatusPending || d.Status == store.StatusApproved)
			if c.Blog.PubOpen {
				c.Ret = c.Blog.PubClose
			}
		}
		s.rewriteCard(r, &c, agents, loc)
		s.replyQueue(r, &c)
		cards = append(cards, c)
	}
	href := func(status, account string) string {
		v := url.Values{}
		if status != store.StatusPending {
			v.Set("status", status)
		}
		if oneBlog != nil {
			v.Set("kind", store.KindBlog)
			v.Set("blog", strconv.FormatInt(oneBlog.ID, 10))
		} else if account != "" {
			v.Set("account", account)
			if kind == store.KindReply {
				v.Set("kind", kind)
			}
		} else if kind != "" {
			v.Set("kind", kind)
		}
		if len(v) == 0 {
			// Explicit, so the bare /queue's approved-today view never
			// replaces a Pending link.
			return "/queue?status=pending"
		}
		return "/queue?" + v.Encode()
	}
	total := 0
	for _, n := range counts.PendingByAccount {
		total += n
	}
	acctChips := []chip{{Label: "All", Href: href(st, ""), Count: total, On: acct == ""}}
	statusAcct := acct
	if oneBlog != nil {
		statusAcct = "" // href keeps the blog filter
	}
	list, _ := s.st.Accounts(ctx)
	for _, a := range list {
		acctChips = append(acctChips, chip{Label: "@" + a.Handle, Href: href(st, a.Handle), Initials: a.Initials, Color: a.Color, Count: counts.PendingByAccount[a.Handle], On: acct == a.Handle})
	}
	var statusChips []chip
	for _, x := range []string{store.StatusPending, store.StatusApproved, store.StatusRejected, store.StatusPosted} {
		statusChips = append(statusChips, chip{Label: strings.ToUpper(x[:1]) + x[1:], Href: href(x, statusAcct), Count: counts.ByStatus[x], On: st == x})
	}
	// With blogs set up the queue gets the kind filter; X account chips
	// show under X (and on installs without blogs, as before).
	// Replies get their own chip once there are any (X then reads "X
	// posts").
	var kindChips []chip
	if len(blogs) > 0 || replies {
		kinds := []struct{ label, kind string }{{"All", ""}, {"X", store.KindX}}
		if replies {
			kinds[1].label = "X posts"
			kinds = append(kinds, struct{ label, kind string }{"Replies", store.KindReply})
		}
		if len(blogs) > 0 {
			kinds = append(kinds, struct{ label, kind string }{"Blog", store.KindBlog})
		}
		for _, k := range kinds {
			v := url.Values{}
			if st != store.StatusPending {
				v.Set("status", st)
			}
			if k.kind != "" {
				v.Set("kind", k.kind)
			}
			h := "/queue?status=pending"
			if len(v) > 0 {
				h = "/queue?" + v.Encode()
			}
			kindChips = append(kindChips, chip{Label: k.label, Href: h, On: kind == k.kind})
		}
	}
	allBlogs := ""
	if oneBlog != nil {
		allBlogs = "/queue?kind=blog"
		if st != store.StatusPending {
			allBlogs += "&status=" + url.QueryEscape(st)
		}
	}
	p := s.newPage(r, u)
	p.Nav, p.Title = "queue", "Review queue"
	pendingLeft := 0
	if all, err := s.st.QueueCounts(ctx, "", ""); err == nil {
		pendingLeft = all.ByStatus[store.StatusPending]
	}
	rf := s.replyFlashFor(ctx, q, pendingLeft, s.zone(r))
	switch {
	case rf != nil:
		flash = ""
	case q.Get("done") == "approved" && pendingLeft > 0 && flash != "":
		flash = "Approved. " + strconv.Itoa(pendingLeft) + " left to review."
	}
	p.Error, p.Flash = errMsg, flash
	p.NewPost = "/compose"
	// Posting and failed drafts (failed includes parts whose outcome is
	// unknown) have no tab here: they live on the Log page, so say so.
	p.Data = map[string]any{"Cards": cards, "Status": st, "AcctChips": acctChips, "StatusChips": statusChips, "Return": r.URL.RequestURI(),
		"KindChips": kindChips, "OneBlog": oneBlog, "AllBlogs": allBlogs, "ShowAccts": len(blogs) == 0 || kind == store.KindX || kind == store.KindReply, "Blogs": len(blogs) > 0, "Replies": replies, "Kind": kind, "TodayView": todayView, "ReplyFlash": rf, "CSRF": csrfToken(r),
		"NPosting": counts.ByStatus[store.StatusPosting], "NFailed": counts.ByStatus[store.StatusFailed],
		"NHidden": counts.ByStatus[store.StatusPosting] + counts.ByStatus[store.StatusFailed]}
	setLive(r, p, status, "queue", seq)
	s.allowXForms(w) // reply cards' Open on X (live updates can add one)
	s.render(w, status, "queue", "app", p)
}

// formVersion reads the "v" field: the version the owner was looking at.
func formVersion(r *http.Request) int {
	v, _ := strconv.Atoi(r.PostFormValue("v"))
	return v
}

// back is where an action returns: a same-site /queue URL from the form, or
// the queue.
func back(r *http.Request) string {
	if b := r.PostFormValue("return"); strings.HasPrefix(b, "/queue") && !strings.HasPrefix(b, "//") {
		return b
	}
	return "/queue"
}

func withFlash(to, flash string) string {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	return to + sep + "done=" + flash
}

// queueFlash is the note the queue shows for ?done=KEY after an action
// redirects there; unknown keys show nothing.
var queueFlash = map[string]string{"approved": "Approved.", "lastapproved": "Approved. Nothing left to review.", "replyleft": "Approved.", "replyapproved": "Approved. Open on X to post it.", "rejected": "Rejected.", "deleted": "Deleted.", "moved": "Moved.", "saved": "Saved.",
	"pending": "Saved. It is back in Pending until you approve it.", "drafted": "Saved as draft. It is in Pending until you approve it.",
	"publishing": "Publishing now. It shows as Published once the live page checks out.",
	"rewrite":    "Sent for a rewrite. The new version comes back to Pending.", "rwcancel": "Rewrite request cancelled. The draft is Pending as it was.",
	"unapproved":  "Back in Pending. It won't open on X until you approve it again.",
	"replyposted": "Marked posted.", "loaded": "The original post loaded.",
	"notloaded": "The original post is still unavailable. xpost only has what the agent sent; reject this reply or ask the agent to send it again."}

// reviewError turns a store error into a status and message.
func reviewError(err error) (int, string) {
	var ce *store.ConflictError
	var ie *store.InputError
	switch {
	case errors.As(err, &ce):
		return http.StatusConflict, ce.Msg
	case errors.As(err, &ie):
		return http.StatusBadRequest, ie.Msg
	case errors.Is(err, store.ErrStale):
		return http.StatusConflict, store.ErrStale.Error()
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "That can't be done in this draft's current state. Reload and try again."
	}
	return 0, ""
}

func (s *Server) afterAction(w http.ResponseWriter, r *http.Request, u *store.User, err error, flash string) {
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		code, msg := reviewError(err)
		if code == 0 {
			s.internalError(w, err)
			return
		}
		to, _ := url.Parse(back(r))
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = to.RawQuery
		s.showQueue(w, r2, u, code, msg, "")
		return
	}
	http.Redirect(w, r, withFlash(back(r), flash), http.StatusSeeOther)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	next := s.nextPending(r.Context(), back(r), id)
	err := s.st.Approve(r.Context(), u, id, formVersion(r))
	if err == nil {
		if d, derr := s.st.Draft(r.Context(), id); derr == nil {
			http.Redirect(w, r, s.approvedLanding(r.Context(), r, d, next), http.StatusSeeOther)
			return
		}
	}
	s.afterAction(w, r, u, err, "approved")
}

func (s *Server) reject(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	s.afterAction(w, r, u, s.st.Reject(r.Context(), u, id, formVersion(r), r.PostFormValue("reason")), "rejected")
}

func (s *Server) deleteDraft(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	s.afterAction(w, r, u, s.st.Delete(r.Context(), u, id, formVersion(r)), "deleted")
}

func (s *Server) media(w http.ResponseWriter, r *http.Request, _ *store.User) {
	// The ref is checked for shape (store.ValidRef) before any lookup;
	// images live in the database, never on disk, so there is no path.
	mime, data, err := s.st.Media(r.Context(), r.PathValue("ref"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; sandbox")
	w.Write(data)
}

// ---- time fields ----

// parseWhen reads date + time fields in the owner's zone.
func parseWhen(date, clock string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(date)+" "+strings.TrimSpace(clock), loc)
	if err != nil {
		return time.Time{}, errors.New("pick a date and a time")
	}
	return t, nil
}

type slotView struct {
	Clock, Label string
	Taken, On    bool
}

// slots lists 15-minute slots around at on the same day, marking taken ones.
func (s *Server) slots(r *http.Request, d *store.Draft, at time.Time, loc *time.Location) []slotView {
	l := at.In(loc)
	start := l.Truncate(time.Minute).Add(-time.Duration(l.Minute()%15) * time.Minute).Add(-time.Hour)
	var out []slotView
	for i := 0; i < 9; i++ {
		t := start.Add(time.Duration(i) * store.SlotStep)
		if t.In(loc).YearDay() != l.YearDay() {
			continue
		}
		taken := len(s.st.Neighbors(r.Context(), d.Account, t, d.ID)) > 0
		out = append(out, slotView{Clock: t.In(loc).Format("15:04"), Label: t.In(loc).Format("3:04"), Taken: taken, On: t.Equal(at)})
	}
	return out
}

// spacingNote summarises the gap to the nearest other items.
func (s *Server) spacingNote(r *http.Request, d *store.Draft, at time.Time, loc *time.Location) string {
	ds, err := s.st.Between(r.Context(), at.Add(-24*time.Hour), at.Add(24*time.Hour))
	if err != nil {
		return ""
	}
	var prev, next *store.Draft
	for _, o := range ds {
		if o.ID == d.ID || o.Account != d.Account {
			continue
		}
		if !o.ScheduledAt.After(at) {
			prev = o
		} else if next == nil {
			next = o
		}
	}
	var parts []string
	if prev != nil {
		parts = append(parts, fmt.Sprintf("%d min after the %s post.", int(at.Sub(prev.ScheduledAt).Minutes()), prev.ScheduledAt.In(loc).Format("3:04 PM")))
	}
	if next != nil {
		parts = append(parts, fmt.Sprintf("Next post is %s.", next.ScheduledAt.In(loc).Format("Mon 3:04 PM")))
	}
	if len(parts) == 0 {
		return "No other @" + d.Account + " posts within a day."
	}
	return strings.Join(parts, " ")
}

// ---- reschedule ----

func (s *Server) reschedulePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	d, err := s.st.Draft(r.Context(), id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	at := d.ScheduledAt
	loc := s.zone(r)
	if q := r.URL.Query(); q.Get("date") != "" {
		if t, err := parseWhen(q.Get("date"), q.Get("time"), loc); err == nil {
			at = t
		}
	}
	s.showReschedule(w, r, u, d, at, http.StatusOK, "")
}

func (s *Server) showReschedule(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft, at time.Time, status int, msg string) {
	loc := s.zone(r)
	accts := s.accountMap(r)
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = "queue", "Reschedule", "/queue"
	p.Error = msg
	if d.IsBlog() {
		// Blog posts have no 15 minute spacing and no slot picker.
		editable := d.Status == store.StatusPending || d.Status == store.StatusApproved || d.Status == store.StatusFailed
		p.Data = map[string]any{"Card": s.card(r, d, accts, loc), "When": s.when(at, loc), "NoSpacing": true, "Editable": editable, "V": d.Version, "ZoneName": loc.String()}
	} else {
		p.Data = map[string]any{"Card": s.card(r, d, accts, loc), "When": s.when(at, loc), "Slots": s.slots(r, d, at, loc),
			"Spacing": s.spacingNote(r, d, at, loc), "Conflicts": s.conflicts(r, d, at, loc), "Editable": d.Status == store.StatusPending || d.Status == store.StatusApproved, "V": d.Version, "ZoneName": loc.String()}
	}
	if v := formVersion(r); r.Method == http.MethodPost && v > 0 && status == http.StatusOK {
		p.Data.(map[string]any)["V"] = v
	}
	s.render(w, status, "reschedule", "app", p)
}

func (s *Server) rescheduleSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	d, err := s.st.Draft(ctx, id)
	if err != nil {
		s.notFound(w, r)
		return
	}
	loc := s.zone(r)
	clock := r.PostFormValue("time")
	op := r.PostFormValue("op")
	if c, ok := strings.CutPrefix(op, "slot:"); ok {
		clock = c
	}
	at, perr := parseWhen(r.PostFormValue("date"), clock, loc)
	if perr != nil {
		s.showReschedule(w, r, u, d, d.ScheduledAt, http.StatusBadRequest, "Pick a date and a time.")
		return
	}
	if op != "move" && op != "approve" && op != "" {
		// slot picks and the default (Enter) button only re-render
		s.showReschedule(w, r, u, d, at, http.StatusOK, "")
		return
	}
	approve := op == "approve"
	var next int64
	if approve {
		next = s.nextPending(ctx, back(r), id)
	}
	err = s.st.Reschedule(ctx, u, id, formVersion(r), at, approve)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		code, msg := reviewError(err)
		if code == 0 {
			s.internalError(w, err)
			return
		}
		if fresh, e := s.st.Draft(ctx, id); e == nil {
			d = fresh
		}
		s.showReschedule(w, r, u, d, at, code, msg)
		return
	}
	flash := "moved"
	if approve {
		if fresh, e := s.st.Draft(ctx, id); e == nil {
			http.Redirect(w, r, s.approvedLanding(ctx, r, fresh, next), http.StatusSeeOther)
			return
		}
		flash = "approved"
	} else if d.Status == store.StatusApproved {
		flash = "pending"
	}
	http.Redirect(w, r, withFlash(back(r), flash), http.StatusSeeOther)
}

// readUpload reads one uploaded file, capped at the media limit.
func readUpload(r *http.Request, field string) ([][]byte, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	var out [][]byte
	for _, fh := range r.MultipartForm.File[field] {
		f, err := fh.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, store.MaxMediaBytes+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > 0 {
			out = append(out, b)
		}
	}
	return out, nil
}
