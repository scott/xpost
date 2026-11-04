package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

// promoView is what an X card shows about the blog post it promotes.
type promoView struct {
	BlogID   int64
	Title    string
	Site     string
	Color    string
	Href     string // the blog post's preview
	Timing   string // "+2h after the blog" or "Fixed time"
	State    string // store.Promo* for approved promos, "" otherwise
	Note     string
	Returned bool // back in pending: the blog was not live
	Rejected bool // the blog post was rejected
}

// promoLine is one promo listed under its blog post's card.
type promoLine struct {
	ID       int64
	Acct     store.Account
	Time     string
	St       string // pill text
	StCls    string
	Text     string
	Href     string
	Approved bool
}

func offsetLabel(min int) string {
	switch {
	case min == 0:
		return "With the blog"
	case min%60 == 0:
		return "+" + strconv.Itoa(min/60) + "h after the blog"
	case min > 60:
		return "+" + strconv.Itoa(min/60) + "h " + strconv.Itoa(min%60) + "m after the blog"
	}
	return "+" + strconv.Itoa(min) + " min after the blog"
}

// promoFor fills a promo's card view, or returns nil for other drafts.
func (s *Server) promoFor(r *http.Request, d *store.Draft, loc *time.Location) *promoView {
	if !d.IsPromo() {
		return nil
	}
	b, err := s.st.Draft(r.Context(), d.Promotes)
	if err != nil || !b.IsBlog() {
		return nil
	}
	v := &promoView{BlogID: b.ID, Title: b.Post.Title, Href: "/drafts/" + strconv.FormatInt(b.ID, 10), Timing: "Fixed time", Returned: d.PromoReturned && d.Status == store.StatusPending,
		Rejected: b.Status == store.StatusRejected}
	if bl, err := s.st.BlogForAccount(r.Context(), b.Account); err == nil {
		v.Site, v.Color = bl.Name, bl.Color
	}
	if d.PromoOffset != nil {
		v.Timing = offsetLabel(*d.PromoOffset)
	}
	now := s.st.Now()
	v.State = store.PromoState(d, b, now)
	back := d.ScheduledAt.Add(store.PromoReturnAfter).In(loc)
	switch v.State {
	case store.PromoEarly:
		v.Note = "Posts after the blog is live."
		if d.PromoOffset != nil {
			v.Note += " If the blog moves, this promo moves with it."
		}
	case store.PromoWaiting:
		v.Note = "Due at " + d.ScheduledAt.In(loc).Format("3:04 PM") + ". " + v.Site + " has not published the blog post yet. This promo posts as soon as it is live. If the blog is not live by " +
			back.Format("3:04 PM Mon") + " (24h), it returns to Pending."
	case store.PromoHeld:
		v.Note = "Held until blog is live. Stays approved. Posts on its own once the blog is live. Returns to Pending if the blog isn't live by " + back.Format("3:04 PM Mon") + "."
	}
	return v
}

// promoLines lists a blog post's promos for its card.
func (s *Server) promoLines(r *http.Request, blog *store.Draft, loc *time.Location) []promoLine {
	ps, _ := s.st.Promos(r.Context(), blog.ID)
	accts := s.accountMap(r)
	var out []promoLine
	for _, p := range ps {
		l := promoLine{ID: p.ID, Acct: accts[p.Account], Time: p.ScheduledAt.In(loc).Format("Mon 3:04 PM"), St: p.Status, StCls: p.Status, Approved: p.Status == store.StatusApproved}
		switch store.PromoState(p, blog, s.st.Now()) {
		case store.PromoHeld:
			l.St, l.StCls = "held", "held"
		case store.PromoWaiting:
			l.St, l.StCls = "waiting on blog", "waitblog"
		}
		l.Text = strings.ReplaceAll(preview(p), store.BlogURLToken, "[blog link]")
		st := p.Status
		if st != store.StatusApproved && st != store.StatusRejected && st != store.StatusPosted {
			st = store.StatusPending
		}
		l.Href = "/queue?kind=x"
		if st != store.StatusPending {
			l.Href += "&status=" + st
		}
		l.Href += "#d" + strconv.FormatInt(p.ID, 10)
		out = append(out, l)
	}
	return out
}

// blogURLSegs marks {{blog_url}} in a promo's text so it renders as a
// "blog link" chip.
func blogURLSegs(text string) []seg {
	if !strings.Contains(text, store.BlogURLToken) {
		return nil
	}
	var out []seg
	for i, piece := range strings.Split(text, store.BlogURLToken) {
		if i > 0 {
			out = append(out, seg{K: "blink"})
		}
		if piece != "" {
			out = append(out, seg{T: piece})
		}
	}
	return out
}

// promoElse links a blog card to its promos on another tab: how many, the
// status label, and where the first of them shows.
type promoElse struct {
	N     int
	Label string
	Href  string
}

// promoTab is the queue tab a promo shows on ("" when it is on the Log
// page instead) and a link to it. The link uses the All filter, where a
// promo always shows (nested under its blog, or on its own).
func promoTab(p *store.Draft) (tab, href string) {
	id := "#d" + strconv.FormatInt(p.ID, 10)
	switch p.Status {
	case store.StatusPending:
		return store.StatusPending, "/queue" + id
	case store.StatusApproved, store.StatusRejected, store.StatusPosted:
		return p.Status, "/queue?status=" + p.Status + id
	case store.StatusFailed:
		return "", "/log?status=failed"
	}
	return "", "/log"
}

// promoOrder lists the other-tab links in tab order.
var promoOrder = []string{store.StatusPending, store.StatusApproved, store.StatusRejected, store.StatusPosted, store.StatusPosting, store.StatusFailed}

// nestedPromos builds full cards for a blog post's promos that are on the
// current status tab, nested under its queue card with the same actions as
// any X card (Rewrite by the X rules). A promo on another tab is not nested
// here: it shows on its own tab (nested there if its blog is on that tab,
// otherwise as its own card), and this card links to it.
func (s *Server) nestedPromos(r *http.Request, blog *store.Draft, tab string, accts map[string]store.Account, agents map[int64]*agentView, loc *time.Location) ([]cardView, []promoElse) {
	ps, _ := s.st.Promos(r.Context(), blog.ID)
	var out []cardView
	other := map[string]*promoElse{}
	for _, p := range ps {
		t, href := promoTab(p)
		if t != tab {
			if e := other[p.Status]; e != nil {
				e.N++
			} else {
				other[p.Status] = &promoElse{N: 1, Label: p.Status, Href: href}
			}
			continue
		}
		c := s.card(r, p, accts, loc)
		c.CSRF, c.Ret, c.Nested = csrfToken(r), r.URL.RequestURI(), true
		s.rewriteCard(r, &c, agents, loc)
		out = append(out, c)
	}
	var els []promoElse
	for _, st := range promoOrder {
		if e := other[st]; e != nil {
			els = append(els, *e)
		}
	}
	return out, els
}
