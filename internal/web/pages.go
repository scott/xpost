package web

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

func (s *Server) pageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /log", s.requireOwner(s.logPage))
	mux.HandleFunc("POST /tokens", s.requireOwner(s.tokenCreate))
	mux.HandleFunc("POST /tokens/{id}/revoke", s.requireOwner(s.tokenRevoke))
	mux.HandleFunc("GET /schedule", s.requireOwner(s.schedulePage))
	mux.HandleFunc("GET /settings", s.requireOwner(s.settingsPage))
	mux.HandleFunc("POST /settings", s.requireOwner(s.settingsSubmit))
	mux.HandleFunc("POST /drafts/{id}/retry", s.requireOwner(s.retryDraft))
	mux.HandleFunc("POST /drafts/{id}/resolve", s.requireOwner(s.resolvePart))
}

// ---- Log & spend ----

type logRow struct {
	E        *store.LogEntry
	Acct     store.Account
	Time     string
	Day      string
	Kind     string
	Cost     string
	LinkText string
	Label    string     // status chip text
	Act      *logAction // owner actions, for failed drafts
}

type tokenRow struct {
	T        *store.AgentToken
	Created  string
	LastUsed string
}

func (s *Server) ago(t *time.Time) string {
	if t == nil {
		return "Never"
	}
	d := s.st.Now().Sub(*t)
	switch {
	case d < time.Minute:
		return "Just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func (s *Server) logPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	flash := map[string]string{"retry": "Retry queued: only the parts not yet on X will post.", "resolved": "Saved. Retry the draft to post any remaining parts."}[r.URL.Query().Get("done")]
	s.showLog(w, r, u, http.StatusOK, "", flash, "")
}

// unknownPart is a part whose outcome the owner must resolve.
type unknownPart struct {
	Part  int
	Error string
}

// logAction is what the owner can do from a log row.
type logAction struct {
	DraftID int64
	Version int
	Retry   string // button label; "" when retry is not offered
	Unknown []unknownPart
}

// rateText formats an amount, or says the rates are unverified.
func (s *Server) rateText(micros int64) string {
	if !s.st.Rates.Known {
		return "unverified"
	}
	return money(micros)
}

// schedulerState describes this server's posting switch.
func (s *Server) schedulerState() (on bool, text string) {
	switch s.cfg.Scheduler {
	case "on":
		return true, "Scheduler on: approved posts publish to X at their time."
	case "fake":
		return true, "Fake poster on (local development): nothing is sent to X."
	}
	return false, "Scheduler off on this server (XPOST_SCHEDULER is not on): nothing posts."
}

// groupStatus is a draft's state from its parts' rows.
func groupStatus(group []*store.LogEntry) string {
	st := store.LogPosted
	rank := map[string]int{store.LogPosted: 0, store.LogPosting: 1, store.LogRetry: 2, store.LogFailed: 3, store.LogUnknown: 4}
	for _, e := range group {
		if rank[e.Status] > rank[st] {
			st = e.Status
		}
	}
	return st
}

func (s *Server) showLog(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash, newToken string) {
	seq := s.liveSeqNow(r)
	ctx := r.Context()
	loc := s.zone(r)
	now := s.st.Now()
	filter := r.URL.Query().Get("status")
	if filter != store.StatusPosted && filter != store.StatusFailed {
		filter = ""
	}
	sp, err := s.st.MonthSpend(ctx, loc, now)
	if err != nil {
		s.internalError(w, err)
		return
	}
	// Straight-line projection to month end from spend so far.
	monthEnd := sp.Month.AddDate(0, 1, 0)
	elapsed := now.Sub(sp.Month).Hours()
	proj := int64(-1) // too early in the month to project
	if elapsed >= 72 {
		proj = int64(math.Round(float64(sp.Micros) * monthEnd.Sub(sp.Month).Hours() / elapsed))
	}
	capMicros := sp.CapMicros()
	pct := func(v int64) int {
		if capMicros <= 0 || !sp.Rates.Known {
			return 0
		}
		return int(min(100, v*100/capMicros))
	}
	entries, err := s.st.PostLog(ctx, now.AddDate(0, 0, -30), "", 300)
	if err != nil {
		s.internalError(w, err)
		return
	}
	all, _ := s.st.PostLog(ctx, now.AddDate(0, 0, -30), "", 1000)
	// Counts are per draft, like the rows.
	groups := map[int64][]*store.LogEntry{}
	for _, e := range all {
		groups[e.DraftID] = append(groups[e.DraftID], e)
	}
	nPosted, nFailed := 0, 0
	for _, g := range groups {
		switch groupStatus(g) {
		case store.LogPosted:
			nPosted++
		case store.LogFailed, store.LogUnknown:
			nFailed++
		}
	}
	accts := s.accountMap(r)
	today := now.In(loc)
	// One row per draft: its parts are grouped, the first part's link
	// shown, and a failed or unknown part surfaced.
	var rows []logRow
	for i := 0; i < len(entries); {
		j := i
		for j < len(entries) && entries[j].DraftID == entries[i].DraftID {
			j++
		}
		group := entries[i:j]
		i = j
		head := group[len(group)-1] // oldest part in the group
		var cost int64
		var bad *store.LogEntry
		var unknown []unknownPart
		link := false
		images := 0
		for _, e := range group {
			images += e.Images
			if e.Status == store.LogPosted || e.Status == store.LogUnknown {
				cost += s.st.PartCost(e.HasURL)
			}
			link = link || e.HasURL
			if e.Part < head.Part {
				head = e
			}
			switch e.Status {
			case store.LogFailed, store.LogRetry, store.LogUnknown:
				if bad == nil || e.Part < bad.Part {
					bad = e
				}
			}
			if e.Status == store.LogUnknown {
				unknown = append([]unknownPart{{Part: e.Part, Error: e.Error}}, unknown...)
			}
		}
		e := *head
		e.Status = groupStatus(group)
		l := group[0].At.In(loc)
		day := l.Format("Mon, Jan 2")
		if l.YearDay() == today.YearDay() && l.Year() == today.Year() {
			day = "Today"
		}
		kind := partsLabel(e.Parts)
		if link {
			kind += " · link"
		}
		if images > 0 {
			kind += fmt.Sprintf(" · %d image%s", images, map[bool]string{true: "", false: "s"}[images == 1])
		}
		e.Error = ""
		if bad != nil {
			switch bad.Status {
			case store.LogRetry:
				when := ""
				if !bad.RetryAt.IsZero() {
					when = " at " + bad.RetryAt.In(loc).Format("3:04 PM")
				}
				e.Error = fmt.Sprintf("Part %d of %d: %s. Attempt %d; retrying%s.", bad.Part, bad.Parts, strings.TrimSuffix(bad.Error, "."), bad.Attempts, when)
			case store.LogUnknown:
				e.Error = fmt.Sprintf("Part %d of %d: %s. Check X, then say whether it posted.", bad.Part, bad.Parts, strings.TrimSuffix(bad.Error, "."))
			default:
				e.Error = fmt.Sprintf("Part %d of %d failed: %s", bad.Part, bad.Parts, bad.Error)
			}
			if bad.Part == 1 && e.Status != store.LogPosted {
				e.URL = ""
			}
		}
		if filter == store.StatusFailed && e.Status != store.LogFailed && e.Status != store.LogUnknown {
			continue
		}
		if filter == store.StatusPosted && e.Status != store.LogPosted {
			continue
		}
		row := logRow{E: &e, Acct: accts[e.Account], Time: l.Format("3:04 PM"), Day: day, Kind: kind, Cost: s.rateText(cost),
			LinkText: strings.TrimPrefix(e.URL, "https://"), Label: statusLabel(e.Status)}
		if !e.Example && (e.Status == store.LogFailed || e.Status == store.LogUnknown) {
			if d, err := s.st.Draft(ctx, e.DraftID); err == nil && d.Status == store.StatusFailed {
				act := &logAction{DraftID: d.ID, Version: d.Version, Unknown: unknown}
				if len(unknown) == 0 {
					act.Retry = "Retry"
					if posted := countPosted(group); posted > 0 {
						act.Retry = fmt.Sprintf("Retry parts %d to %d", posted+1, e.Parts)
						if posted+1 == e.Parts {
							act.Retry = fmt.Sprintf("Retry part %d", e.Parts)
						}
					}
				}
				row.Act = act
			}
		}
		rows = append(rows, row)
	}
	toks, err := s.st.AgentTokens(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var trows []tokenRow
	for _, t := range toks {
		trows = append(trows, tokenRow{T: t, Created: t.CreatedAt.In(loc).Format("Jan 2"), LastUsed: s.ago(t.LastUsedAt)})
	}
	schedOn, schedText := s.schedulerState()
	block := sp.PostingBlock()
	p := s.newPage(r, u)
	p.Nav, p.Title = "log", "Log & spend"
	p.Error, p.Flash = errMsg, flash
	p.Data = map[string]any{
		"Spend": sp, "Total": s.rateText(sp.Micros), "Cap": cents(sp.CapCents), "CapSet": sp.CapSet, "RatesKnown": sp.Rates.Known,
		"Proj": projText(proj), "ProjOK": proj >= 0 && sp.Rates.Known, "Pct": pct(sp.Micros), "ProjPct": pct(max(proj, sp.Micros)),
		"Warn": cents(sp.CapCents * 8 / 10), "MonthName": sp.Month.Format("January 2006"), "MonthEnd": monthEnd.AddDate(0, 0, -1).Format("Jan 2"),
		"PrevName": sp.Month.AddDate(0, -1, 0).Format("January"), "Prev": s.rateText(sp.PrevMicros),
		"PostsAmt": s.rateText(int64(sp.Posts) * sp.Rates.PostMicros), "LinksAmt": s.rateText(int64(sp.LinkPosts) * sp.Rates.LinkPostMicros),
		"Rate": s.rateText(sp.Rates.PostMicros), "LinkRate": s.rateText(sp.Rates.LinkPostMicros),
		"ImagesAmt": s.costText(int64(sp.Images)*sp.Rates.UploadMicros, 1, 0), "ImageRate": s.costText(sp.Rates.UploadMicros, 1, 0), "UploadKnown": sp.Rates.UploadKnown,
		"AltsAmt": s.costText(int64(sp.Alts)*sp.Rates.AltMicros, 0, 1), "AltRate": s.costText(sp.Rates.AltMicros, 0, 1), "AltKnown": sp.Rates.AltKnown,
		"SchedOn": schedOn, "SchedText": schedText, "Block": block, "CanPost": schedOn && block == "", "MediaBlocks": sp.MediaBlocks(),
		"Rows": rows, "Filter": filter, "NAll": len(groups), "NPosted": nPosted, "NFailed": nFailed,
		"Tokens": trows, "NewToken": newToken, "NewTokenName": r.PostFormValue("name"),
	}
	if newToken == "" { // never refetch a page that shows a new token
		setLive(r, p, status, "log", seq)
	}
	s.render(w, status, "log", "app", p)
}

func countPosted(group []*store.LogEntry) int {
	n := 0
	for _, e := range group {
		if e.Status == store.LogPosted {
			n++
		}
	}
	return n
}

func statusLabel(st string) string {
	switch st {
	case store.LogUnknown:
		return "needs review"
	case store.LogRetry:
		return "retrying"
	}
	return st
}

// ---- Owner actions on failed drafts (Log page) ----

func (s *Server) logAfter(w http.ResponseWriter, r *http.Request, u *store.User, err error, done string) {
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
		s.showLog(w, r, u, code, msg, "", "")
		return
	}
	http.Redirect(w, r, "/log?done="+done, http.StatusSeeOther)
}

func (s *Server) retryDraft(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	err := s.st.RetryDraft(r.Context(), u, id, formVersion(r))
	if err == nil {
		s.log.Printf("owner queued a retry of draft %d", id)
	}
	s.logAfter(w, r, u, err, "retry")
}

// statusIDRe takes the id from a pasted post link or a bare id.
var statusIDRe = regexp.MustCompile(`^(?:\S*/status(?:es)?/)?([0-9]{1,30})(?:[/?#]\S*)?$`)

func (s *Server) resolvePart(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	part, err := strconv.Atoi(r.PostFormValue("part"))
	if err != nil || part < 1 {
		s.showLog(w, r, u, http.StatusBadRequest, "Unknown part.", "", "")
		return
	}
	posted := r.PostFormValue("outcome") == "posted"
	var postID, link string
	if posted {
		m := statusIDRe.FindStringSubmatch(strings.TrimSpace(r.PostFormValue("post")))
		if m == nil {
			s.showLog(w, r, u, http.StatusBadRequest, "Paste the post's link (…/status/123…) or its numeric id.", "", "")
			return
		}
		postID = m[1]
		if s.cfg.X.Enabled() {
			if c, err := s.st.XConnection(r.Context(), r.PostFormValue("account")); err == nil {
				link = s.cfg.X.StatusURL(c.XUsername, postID)
			}
		}
	}
	err = s.st.ResolveUnknown(r.Context(), u, id, formVersion(r), part, posted, postID, link)
	if err == nil {
		s.log.Printf("owner resolved draft %d part %d: posted=%v", id, part, posted)
	}
	s.logAfter(w, r, u, err, "resolved")
}

func (s *Server) tokenCreate(w http.ResponseWriter, r *http.Request, u *store.User) {
	var scopes []string
	if r.PostFormValue("drafts_write") != "" {
		scopes = append(scopes, store.ScopeDraftsWrite)
	}
	if r.PostFormValue("log_read") != "" {
		scopes = append(scopes, store.ScopeLogRead)
	}
	if r.PostFormValue("drafts_revise") != "" {
		scopes = append(scopes, store.ScopeDraftsRevise)
	}
	if s.cfg.BlogsEnabled && r.PostFormValue("blogs_publish") != "" {
		scopes = append(scopes, store.ScopeBlogsPublish)
	}
	_, secret, err := s.st.CreateAgentToken(r.Context(), u, r.PostFormValue("name"), scopes)
	if err != nil {
		var ie *store.InputError
		if errors.As(err, &ie) {
			s.showLog(w, r, u, http.StatusBadRequest, ie.Msg, "", "")
			return
		}
		s.internalError(w, err)
		return
	}
	// Shown once, in this response only (Cache-Control: no-store). Never
	// logged and never stored in the clear.
	s.showLog(w, r, u, http.StatusOK, "", "", secret)
}

func (s *Server) tokenRevoke(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.st.RevokeAgentToken(r.Context(), u, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.notFound(w, r)
			return
		}
		s.internalError(w, err)
		return
	}
	s.showLog(w, r, u, http.StatusOK, "", "Token revoked. Its drafts stay in the queue.", "")
}

// ---- Accounts ----

// ---- Schedule ----
//
// The week grid, the zoom panel and the phone day view are inline SVG. The
// CSP forbids style attributes, so positions are SVG geometry attributes
// (x, y, width, height: not styles) and every color lives in xpost.css.

const (
	schedDefFrom = 8 * 60  // default axis start, minutes after local midnight
	schedDefTo   = 19 * 60 // default axis end
	zoomSpan     = 150     // minutes shown in the desktop zoom panel
	zoomLead     = 30      // minutes shown before the focus post
	phoneSpan    = 60      // minutes shown in the phone day grid (grows to fit)
	phoneMaxSpan = 180
	phonePx      = 4.6 // phone grid pixels per minute (15 min = 69px)
	phoneTop     = 23
	weekLaneH    = 30
	weekTop      = 8
	zoomRowH     = 52
	weekTrackPx  = 800.0
	labelCharPx  = 6.6
)

var schedWin = int(store.MinSpacing / time.Minute)

type schedItem struct {
	ID                       int64
	Acct, Initials, Color    string
	At                       time.Time
	Min                      int // minutes after local midnight
	Time, Clock              string
	Status, Cls, Fill        string
	Parts                    int
	Short, PShort, Preview   string
	Conflict                 bool
	GapMin, NbMin            int
	GapRel, GapRef           string
	MoveHref, Move, MoveShrt string
	MoveMin                  int    // -1 when the suggestion is another day
	Wait                     string // promos: "waiting" (on its blog) or "held" (the blog failed)
	WaitWhy                  string
}

type schedTick struct{ X, Label, Anchor string }

// edgeAnchor keeps the first and last axis labels inside the track.
func edgeAnchor(m, lo, hi int) string {
	switch m {
	case lo:
		return "start"
	case hi:
		return "end"
	}
	return "middle"
}

type schedMark struct{ X, W, Cls, Fill, Href, Title string }

type schedLabel struct {
	X, Anchor, Text, Sfx, Href string
	DX                         int
	Bad, Wide, Tight, Wait     bool
}

type schedLane struct {
	Acct         store.Account
	Y, BaseY, MY int
	TY           int
	Marks        []schedMark
	Labels       []schedLabel
}

type schedDay struct {
	Date, Label, Short, Num, Href string
	Weekday                       string
	Count                         int
	Today, On, Conflict           bool
	Lanes                         []schedLane
	EmptyY                        int
	items                         []*schedItem
	Blogs                         []schedBlog // blog posts: a day banner each
}

// blogStatus is a blog post's status as its pill reads.
func blogStatus(st string) string {
	switch st {
	case store.StatusPosted:
		return "published"
	case store.StatusPosting:
		return "publishing"
	}
	return st
}

// schedBlog is a blog post's day banner on the Schedule.
type schedBlog struct {
	ID                                 int64
	Time, Site, Title, St, Color, Href string
	Min                                int    // minutes after local midnight, in the configured zone
	TickX                              string // the publish-time tick on the week grid ("" on the phone)
	Pad                                string // the class that starts the text after the tick (or ends it before)
}

// schedChip is one All / X / Blog filter link.
type schedChip struct {
	Label, Href string
	On          bool
}

// blogTick places a banner's publish-time tick on the week axis and picks
// the padding class: text after the tick, or before it when the tick is
// far right.
func blogTick(b *schedBlog, lo, span int) {
	if span <= 0 {
		return
	}
	pct := (b.Min - lo) * 100 / span
	pct = max(0, min(100, pct))
	b.TickX = pctAt(b.Min, lo, span)
	if pct <= 60 {
		b.Pad = fmt.Sprintf("bl%d", pct)
	} else {
		b.Pad = fmt.Sprintf("br%d", 100-pct)
	}
}

// promoWait is how a promo stands on the calendar against its blog post:
// "waiting" (approved, but the blog is not approved yet or not live by the
// promo's time) or "held" (the blog's publish failed), with why.
func promoWait(p, blog *store.Draft, now time.Time) (string, string) {
	if p.Status != store.StatusApproved || blog == nil {
		return "", ""
	}
	switch {
	case blog.Status == store.StatusPending:
		return "waiting", "blog not approved yet"
	case store.PromoState(p, blog, now) == store.PromoWaiting:
		return "waiting", "blog not live yet"
	case store.PromoState(p, blog, now) == store.PromoHeld:
		return "held", "blog publish failed"
	}
	return "", ""
}

type zoomCard struct {
	X, W, Cls, Fill, Href, Time, Short, Title string
	Y, TY, SY, BY                             int
	Ghost                                     bool
}

type zoomText struct {
	X, Anchor, Text, Link, Href string
	DX, Y                       int
	Bad                         bool
}

type zoomBracket struct {
	X1, X2 string
	Y1, Y2 int
}

type zoomLane struct {
	Acct     store.Account
	H        int
	Cards    []zoomCard
	Texts    []zoomText
	Brackets []zoomBracket
}

type zoomView struct {
	Title string
	Ticks []schedTick
	Lines []string
	Lanes []zoomLane
	Empty bool
}

type phoneCard struct {
	Y, TY, BandY, BandH int
	Cls, Fill, Href     string
	Clock, Short, Title string
}

type phoneCol struct {
	Acct  store.Account
	Cards []phoneCard
}

type phoneLine struct {
	Y     int
	Label string
}

type phoneView struct {
	H              int
	Lines          []phoneLine
	Cols           []phoneCol
	Earlier, Later []*schedItem
	Conflicts      int
	First          *schedItem
	Day            string
	Empty          bool
}

func schedClass(status string, conflict bool) (cls, fill string) {
	switch {
	case conflict:
		return "co", "url(#xh-co)"
	case status == store.StatusPending:
		return "pe", "url(#xh-pe)"
	case status == store.StatusPosted:
		return "po", "url(#xh-po)"
	case status == store.StatusFailed:
		return "fa", "url(#xh-co)"
	}
	return "ap", "url(#xh-ap)"
}

func schedShort(d *store.Draft) string {
	switch {
	case len(d.Parts) > 1:
		return fmt.Sprintf("Thread · %d", len(d.Parts))
	case d.Status == store.StatusPending:
		return "Pending"
	case d.Status == store.StatusPosted:
		return "Posted"
	case d.Status == store.StatusFailed:
		return "Failed"
	}
	return "Single"
}

func pctAt(min, from, span int) string {
	return fmt.Sprintf("%.3f%%", float64(min-from)*100/float64(span))
}

func floor15(m int) int {
	if m < 0 {
		return -((-m + 14) / 15 * 15)
	}
	return m / 15 * 15
}

func ceil15(m int) int { return floor15(m + 14) }

func ampm(min int) string {
	if (min/60)%24 < 12 {
		return "AM"
	}
	return "PM"
}

func clockOf(min int) string {
	h := (min / 60) % 24
	if h == 0 {
		h = 12
	} else if h > 12 {
		h -= 12
	}
	return fmt.Sprintf("%d:%02d", h, min%60)
}

// moveTarget is the earliest time at or after at that keeps MinSpacing from
// every other post on account (exclude is the post being moved), stepping
// past each neighbour in turn. It never suggests an earlier time. Times in
// the past fall back to the store's next free 15-minute slot. The schedule's
// Move to and the queue and reschedule conflict hints both use it.
func (s *Server) moveTarget(ctx context.Context, account string, at time.Time, exclude int64) time.Time {
	t, now := at, s.st.Now()
	for i := 0; i < 7*96; i++ {
		if !t.After(now) {
			return s.st.NextFree(ctx, account, now, exclude)
		}
		ns := s.st.Neighbors(ctx, account, t, exclude)
		if len(ns) == 0 {
			return t
		}
		latest := ns[0].At
		for _, n := range ns {
			if n.At.After(latest) {
				latest = n.At
			}
		}
		t = latest.Add(store.MinSpacing)
	}
	return time.Time{}
}

// Label fitting assumes day tracks about this wide: a 1440px window (wide),
// 1280px (normal) and a narrow tablet. A suffix that only fits the wide
// track gets class "w" and a label that would not fit the narrow one gets
// "tight"; xpost.css hides them on smaller screens instead of overlapping.
const (
	trackWide   = 960.0
	trackNarrow = 360.0
)

// clusterLabels merges posts whose labels would collide into one label.
func clusterLabels(items []*schedItem, from, span int) []schedLabel {
	width := func(s string) float64 { return 6 + float64(len([]rune(s)))*labelCharPx + 4 }
	// fits reports whether text starting at minute start ends before limit
	// on a track trackPx wide.
	fits := func(trackPx, start float64, text string, limit float64) bool {
		return start+width(text)*float64(span)/trackPx <= limit
	}
	var out []schedLabel
	for i := 0; i < len(items); {
		j := i
		var lab schedLabel
		for {
			text, sfx, bad := clusterText(items[i : j+1])
			start := float64(items[j].Min + schedWin)
			limit := float64(from + span)
			if j+1 < len(items) {
				limit = float64(items[j+1].Min)
			}
			full := text
			if sfx != "" {
				full += " · " + sfx
			}
			if j+1 < len(items) && !fits(weekTrackPx, start, text, limit) {
				j++ // even the times collide: merge the next post in
				continue
			}
			lab = schedLabel{X: pctAt(items[j].Min+schedWin, from, span), DX: 6, Anchor: "start", Text: text, Sfx: sfx, Bad: bad, Wait: i == j && items[i].Wait != ""}
			if j+1 == len(items) && !fits(trackNarrow, start, full, limit) {
				// at the right edge: put the label before the first marker
				lab.X, lab.DX, lab.Anchor = pctAt(items[i].Min, from, span), -6, "end"
				start = float64(items[i].Min) - width(full)*float64(span)/weekTrackPx
				limit = start + width(full)*float64(span)/weekTrackPx
				if len(out) > 0 || start < float64(from) {
					// no room on the left either: keep it on the right, clipped
					lab.X, lab.DX, lab.Anchor = pctAt(items[j].Min+schedWin, from, span), 6, "start"
					start, limit = float64(items[j].Min+schedWin), float64(from+span)
				}
			}
			switch {
			case sfx == "" || fits(weekTrackPx, start, full, limit):
			case fits(trackWide, start, full, limit):
				lab.Wide = true
			default:
				lab.Sfx = ""
			}
			lab.Tight = !fits(trackNarrow, start, text, limit)
			break
		}
		lab.Href = fmt.Sprintf("/drafts/%d", items[i].ID)
		for _, it := range items[i : j+1] {
			if it.Conflict {
				lab.Href = fmt.Sprintf("/drafts/%d", it.ID)
				break
			}
		}
		out = append(out, lab)
		i = j + 1
	}
	return out
}

func clusterText(c []*schedItem) (text, sfx string, bad bool) {
	same := true
	for _, it := range c {
		if ampm(it.Min) != ampm(c[0].Min) {
			same = false
		}
	}
	last := c[len(c)-1]
	switch {
	case len(c) > 3:
		text = clockOf(c[0].Min) + " to " + clockOf(last.Min) + " " + ampm(last.Min)
		if !same {
			text = c[0].Time + " to " + last.Time
		}
	case same:
		var cs []string
		for _, it := range c {
			cs = append(cs, clockOf(it.Min))
		}
		text = strings.Join(cs, ", ") + " " + ampm(last.Min)
	default:
		var cs []string
		for _, it := range c {
			cs = append(cs, it.Time)
		}
		text = strings.Join(cs, ", ")
	}
	if len(c) == 1 {
		switch {
		case c[0].Wait == "waiting":
			return text, "Waiting on blog", false
		case c[0].Wait == "held":
			return text, "Held", false
		case c[0].Conflict:
			return text, fmt.Sprintf("%d min gap", c[0].GapMin), true
		case c[0].Parts > 1:
			return text, "thread", false
		}
		return text, "", false
	}
	minGap := -1
	for k := 1; k < len(c); k++ {
		if g := c[k].Min - c[k-1].Min; minGap < 0 || g < minGap {
			minGap = g
		}
	}
	conflict := false
	for _, it := range c {
		if it.Conflict {
			conflict = true
			if it.GapMin < minGap {
				minGap = it.GapMin
			}
		}
	}
	switch {
	case conflict || minGap < schedWin:
		sfx, bad = fmt.Sprintf("%d min gap", minGap), true
	case minGap == schedWin:
		sfx = fmt.Sprintf("%d min, at minimum", schedWin)
	}
	if len(c) > 3 && sfx == "" {
		sfx = fmt.Sprintf("%d posts", len(c))
	}
	return text, sfx, bad
}

// zoomFocus is the minute the zoom panel centres on: the earlier post of the
// day's first conflict, else the post that starts the busiest window.
func zoomFocus(items []*schedItem) int {
	for _, it := range items {
		if it.Conflict {
			if it.NbMin < it.Min {
				return it.NbMin
			}
			return it.Min
		}
	}
	focus, best := 0, -1
	for _, it := range items {
		c := floor15(it.Min) - zoomLead
		n := 0
		for _, o := range items {
			if o.Min >= c && o.Min+schedWin <= c+zoomSpan {
				n++
			}
		}
		if n > best {
			focus, best = it.Min, n
		}
	}
	return focus
}

func clampWin(start, span int) int {
	if start < 0 {
		start = 0
	}
	if start+span > 24*60 {
		start = 24*60 - span
	}
	return start
}

func (s *Server) buildZoom(day *schedDay, accts []store.Account) zoomView {
	z := zoomView{Title: day.Label}
	if len(day.items) == 0 {
		z.Empty = true
		return z
	}
	ws := clampWin(floor15(zoomFocus(day.items))-zoomLead, zoomSpan)
	we := ws + zoomSpan
	z.Title += " · " + clockOf(ws) + " to " + clockOf(we) + " " + ampm(we)
	if ampm(ws) != ampm(we) {
		z.Title = day.Label + " · " + clockOf(ws) + " " + ampm(ws) + " to " + clockOf(we) + " " + ampm(we)
	}
	if we == 24*60 {
		z.Title = day.Label + " · " + clockOf(ws) + " " + ampm(ws) + " to midnight"
	}
	for m := ws; m <= we; m += 15 {
		z.Ticks = append(z.Ticks, schedTick{X: pctAt(m, ws, zoomSpan), Label: clockOf(m), Anchor: edgeAnchor(m, ws, we)})
		z.Lines = append(z.Lines, pctAt(m, ws, zoomSpan))
	}
	slotW := pctAt(ws+schedWin-1, ws, zoomSpan)
	for _, a := range accts {
		lane := zoomLane{Acct: a}
		var mine []*schedItem
		for _, it := range day.items {
			if it.Acct == a.Handle {
				mine = append(mine, it)
			}
		}
		var rowEnd []int
		place := func(min, minRow int) int {
			for r := minRow; ; r++ {
				if r >= len(rowEnd) {
					rowEnd = append(rowEnd, min+schedWin)
					return r
				}
				if rowEnd[r] <= min {
					rowEnd[r] = min + schedWin
					return r
				}
			}
		}
		rowOf := map[int64]int{}
		for k, it := range mine {
			if it.Min+schedWin <= ws || it.Min >= we {
				continue
			}
			r := place(it.Min, 0)
			rowOf[it.ID] = r
			y := weekTop + r*zoomRowH
			lane.Cards = append(lane.Cards, zoomCard{X: pctAt(it.Min, ws, zoomSpan), W: slotW, Y: y, TY: y + 18, SY: y + 34, BY: y + 7,
				Cls: it.Cls, Fill: it.Fill, Href: fmt.Sprintf("/drafts/%d", it.ID), Time: it.Time, Short: it.Short, Title: "@" + it.Acct + " " + it.Time + " " + it.Status + ": " + it.Preview})
			right := it.Min+schedWin > ws+zoomSpan*55/100
			tx, anchor, dx := pctAt(it.Min+schedWin, ws, zoomSpan), "start", 8
			if right {
				tx, anchor, dx = pctAt(it.Min, ws, zoomSpan), "end", -8
			}
			if it.Conflict {
				txt := zoomText{X: tx, Anchor: anchor, DX: dx, Y: y + 27, Bad: true,
					Text: fmt.Sprintf("%d min %s %s. Needs %d.", it.GapMin, it.GapRel, it.GapRef, schedWin)}
				if it.MoveHref != "" {
					txt.Link, txt.Href = "Move to "+it.Move, it.MoveHref
				}
				lane.Texts = append(lane.Texts, txt)
				if nr, ok := rowOf[nbID(mine, it)]; ok && nr < r && it.NbMin >= ws {
					lo, hi := it.NbMin, it.Min
					if hi < lo {
						lo, hi = hi, lo
					}
					lane.Brackets = append(lane.Brackets, zoomBracket{X1: pctAt(lo, ws, zoomSpan), X2: pctAt(hi, ws, zoomSpan), Y1: y - 7, Y2: y - 1})
				}
				if it.MoveMin >= ws && it.MoveMin < we {
					gr := place(it.MoveMin, r+1)
					gy := weekTop + gr*zoomRowH
					lane.Cards = append(lane.Cards, zoomCard{X: pctAt(it.MoveMin, ws, zoomSpan), W: slotW, Y: gy, TY: gy + 18, SY: gy + 34, BY: gy + 7,
						Cls: "gh", Href: it.MoveHref, Time: it.Move, Short: "Next free", Title: "Next free slot for @" + it.Acct + ": " + it.Move, Ghost: true})
				}
			} else if k+1 < len(mine) {
				nx := mine[k+1]
				if gap := nx.Min - it.Min; gap >= 45 && nx.Min < we && !right {
					lane.Texts = append(lane.Texts, zoomText{X: tx, Anchor: anchor, DX: dx, Y: y + 27, Text: fmt.Sprintf("%d min to next post", gap)})
				}
			}
		}
		rows := len(rowEnd)
		if rows == 0 {
			rows = 1
		}
		lane.H = weekTop + rows*zoomRowH + 4
		z.Lanes = append(z.Lanes, lane)
	}
	return z
}

// nbID finds the id of the nearest neighbour of a conflicting item.
func nbID(mine []*schedItem, it *schedItem) int64 {
	for _, o := range mine {
		if o.ID != it.ID && o.Min == it.NbMin {
			return o.ID
		}
	}
	return 0
}

func (s *Server) buildPhone(day *schedDay, accts []store.Account) phoneView {
	p := phoneView{Day: day.Label}
	if len(day.items) == 0 {
		p.Empty = true
		return p
	}
	ps := clampWin(floor15(zoomFocus(day.items)), phoneSpan)
	pe := ps + phoneSpan
	for grew := true; grew; {
		grew = false
		for _, it := range day.items {
			if it.Min >= ps && it.Min < pe && it.Min+schedWin > pe && ceil15(it.Min+schedWin) <= ps+phoneMaxSpan && ceil15(it.Min+schedWin) <= 24*60 {
				pe, grew = ceil15(it.Min+schedWin), true
			}
		}
	}
	y := func(m int) int { return phoneTop + int(float64(m-ps)*phonePx+0.5) }
	p.H = y(pe) + 8
	for m := ps; m <= pe; m += 15 {
		p.Lines = append(p.Lines, phoneLine{Y: y(m), Label: clockOf(m)})
	}
	for _, it := range day.items {
		if it.Conflict {
			p.Conflicts++
			if p.First == nil {
				p.First = it
			}
		}
		switch {
		case it.Min < ps:
			p.Earlier = append(p.Earlier, it)
		case it.Min+schedWin > pe:
			p.Later = append(p.Later, it)
		}
	}
	for _, a := range accts {
		col := phoneCol{Acct: a}
		prev := -1000
		for _, it := range day.items {
			if it.Acct != a.Handle || it.Min < ps || it.Min+schedWin > pe {
				continue
			}
			cy := y(it.Min) + 1
			if cy < prev+46 {
				cy = prev + 46
			}
			prev = cy
			col.Cards = append(col.Cards, phoneCard{Y: cy, TY: cy + 27, BandY: y(it.Min), BandH: y(it.Min+schedWin) - y(it.Min),
				Cls: it.Cls, Fill: it.Fill, Href: fmt.Sprintf("/drafts/%d", it.ID), Clock: it.Clock, Short: it.PShort,
				Title: "@" + it.Acct + " " + it.Time + " " + it.Status + ": " + it.Preview})
		}
		if b := prev + 46 + 4; b > p.H {
			p.H = b
		}
		p.Cols = append(p.Cols, col)
	}
	return p
}

func (s *Server) schedulePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	seq := s.liveSeqNow(r)
	ctx := r.Context()
	loc := s.zone(r)
	now := s.st.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := today
	q := r.URL.Query()
	if f := q.Get("from"); f != "" {
		if t, err := time.ParseInLocation("2006-01-02", f, loc); err == nil {
			start = t
		}
	}
	end := start.AddDate(0, 0, 7)
	blogs := map[string]*store.Blog{}
	if bs, err := s.st.Blogs(ctx); err == nil {
		for _, b := range bs {
			blogs[b.Account] = b
		}
	}
	// All / X / Blog: X hides the blog banners, Blog shows only blog posts
	// and their promos.
	kind := q.Get("kind")
	if kind != store.KindX && kind != store.KindBlog || len(blogs) == 0 {
		kind = ""
	}
	link := func(from, day, k string) string {
		var v []string
		if from != "" {
			v = append(v, "from="+from)
		}
		if day != "" {
			v = append(v, "day="+day)
		}
		if k != "" {
			v = append(v, "kind="+k)
		}
		if len(v) == 0 {
			return "/schedule"
		}
		return "/schedule?" + strings.Join(v, "&")
	}
	drafts, err := s.st.Between(ctx, start, end)
	if err != nil {
		s.internalError(w, err)
		return
	}
	accts, _ := s.st.Accounts(ctx)
	type stat struct {
		Acct            store.Account
		Items, Approved int
	}
	stats := map[string]*stat{}
	for _, a := range accts {
		stats[a.Handle] = &stat{Acct: a}
	}
	from := start.Format("2006-01-02")
	days := make([]*schedDay, 7)
	for i := range days {
		ds := start.AddDate(0, 0, i)
		days[i] = &schedDay{Date: ds.Format("2006-01-02"), Label: ds.Format("Mon, Jan 2"), Weekday: ds.Format("Monday"), Short: ds.Format("Mon"), Num: ds.Format("2"),
			Today: ds.Equal(today), Href: link(from, ds.Format("2006-01-02"), kind)}
	}
	conflicts := 0
	var firstConflict string
	var next *store.Draft
	failed := false
	lo, hi := schedDefFrom, schedDefTo
	nBlogs := 0
	type blogStat struct{ Total, Published, Scheduled, Pending, Failed int }
	var bst, pst blogStat
	var nextBlog *store.Draft
	var bposts []*store.Draft
	if kind != store.KindX {
		bposts, _ = s.st.BlogsBetween(ctx, start, end)
	}
	for _, d := range bposts {
		if b := blogs[d.Account]; b != nil {
			l := d.ScheduledAt.In(loc)
			for i := range days {
				if days[i].Date == l.Format("2006-01-02") {
					m := l.Hour()*60 + l.Minute()
					days[i].Blogs = append(days[i].Blogs, schedBlog{ID: d.ID, Time: l.Format("3:04 PM"), Site: b.Name, Title: d.Post.Title, St: blogStatus(d.Status), Color: b.Color,
						Href: "/drafts/" + strconv.FormatInt(d.ID, 10), Min: m})
					nBlogs++
					bst.Total++
					switch d.Status {
					case store.StatusPosted:
						bst.Published++
					case store.StatusApproved, store.StatusPosting:
						bst.Scheduled++
					case store.StatusFailed:
						bst.Failed++
					default:
						bst.Pending++
					}
					if (d.Status == store.StatusPending || d.Status == store.StatusApproved) && d.ScheduledAt.After(s.st.Now()) && (nextBlog == nil || d.ScheduledAt.Before(nextBlog.ScheduledAt)) {
						nextBlog = d
					}
					// An hour of axis before the banner's tick, so it stands clear of the edge.
					if h := max(0, m/60*60-60); h < lo {
						lo = h
					}
					if h := ceilHour(m + 30); h > hi {
						hi = h
					}
				}
			}
		}
	}
	blogOf := map[int64]*store.Draft{}
	nWait := 0
	firstWait := ""
	for _, d := range drafts {
		if _, ok := stats[d.Account]; !ok {
			continue
		}
		if kind == store.KindBlog && !d.IsPromo() {
			continue
		}
		l := d.ScheduledAt.In(loc)
		mid := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
		di := -1
		for i := range days {
			if days[i].Date == mid.Format("2006-01-02") {
				di = i
			}
		}
		if di < 0 {
			continue
		}
		it := &schedItem{ID: d.ID, Acct: d.Account, At: d.ScheduledAt, Min: l.Hour()*60 + l.Minute(), Time: l.Format("3:04 PM"), Clock: l.Format("3:04"),
			Status: d.Status, Parts: len(d.Parts), Short: schedShort(d), Preview: preview(d), MoveMin: -1}
		it.PShort = strings.Replace(it.Short, " · ", " ", 1)
		it.Initials, it.Color = stats[d.Account].Acct.Initials, stats[d.Account].Acct.Color
		if d.Status == store.StatusPending {
			if ns := s.st.Neighbors(ctx, d.Account, d.ScheduledAt, d.ID); len(ns) > 0 {
				it.Conflict = true
				nb := ns[0]
				for _, n := range ns {
					if absDur(n.At.Sub(d.ScheduledAt)) < absDur(nb.At.Sub(d.ScheduledAt)) {
						nb = n
					}
				}
				gap := d.ScheduledAt.Sub(nb.At)
				it.GapRel = "after"
				if gap < 0 {
					it.GapRel, gap = "before", -gap
				}
				it.GapMin = int(gap / time.Minute)
				it.GapRef = nb.At.In(loc).Format("3:04")
				if it.GapRel == "after" {
					it.NbMin = it.Min - it.GapMin
				} else {
					it.NbMin = it.Min + it.GapMin
				}
				if mv := s.moveTarget(ctx, d.Account, d.ScheduledAt, d.ID); !mv.IsZero() {
					ml := mv.In(loc)
					it.Move, it.MoveShrt = ml.Format("3:04 PM"), ml.Format("3:04")
					if ml.Format("2006-01-02") == days[di].Date {
						it.MoveMin = ml.Hour()*60 + ml.Minute()
					} else {
						it.Move, it.MoveShrt = ml.Format("Mon 3:04 PM"), ml.Format("Mon 3:04")
					}
					it.MoveHref = fmt.Sprintf("/drafts/%d/reschedule?date=%s&time=%s", d.ID, ml.Format("2006-01-02"), ml.Format("15:04"))
				}
				conflicts++
				days[di].Conflict = true
				if firstConflict == "" {
					firstConflict = l.Format("Mon 3:04 PM") + " @" + d.Account
				}
			}
		}
		it.Cls, it.Fill = schedClass(d.Status, it.Conflict)
		if d.IsPromo() {
			bd, ok := blogOf[d.Promotes]
			if !ok {
				bd, _ = s.st.Draft(ctx, d.Promotes)
				blogOf[d.Promotes] = bd
			}
			it.Wait, it.WaitWhy = promoWait(d, bd, s.st.Now())
			pst.Total++
			switch d.Status {
			case store.StatusPosted:
				pst.Published++
			case store.StatusApproved, store.StatusPosting:
				pst.Scheduled++
			case store.StatusFailed:
				pst.Failed++
			default:
				pst.Pending++
			}
			if it.Wait != "" {
				nWait++
				if firstWait == "" {
					firstWait = l.Format("Mon 3:04 PM") + ", " + it.WaitWhy
				}
				if !it.Conflict {
					it.Cls, it.Fill = "wb", "url(#xh-wb)"
				}
				it.Short = "Waiting on blog"
				if it.Wait == "held" {
					it.Short = "Held"
				}
				it.PShort = "Waiting" // the phone column is narrow
				if it.Wait == "held" {
					it.PShort = "Held"
				}
			}
		}
		if d.Status == store.StatusFailed {
			failed = true
		}
		days[di].items = append(days[di].items, it)
		days[di].Count++
		st := stats[d.Account]
		st.Items++
		if d.Status == store.StatusApproved {
			st.Approved++
			if d.ScheduledAt.After(s.st.Now()) && (next == nil || d.ScheduledAt.Before(next.ScheduledAt)) {
				next = d
			}
		}
		if h := it.Min / 60 * 60; h < lo {
			lo = h
		}
		if h := ceilHour(it.Min + schedWin); h > hi {
			hi = h
		}
	}
	if hi > 24*60 {
		hi = 24 * 60
	}
	span := hi - lo
	for _, d := range days {
		for i := range d.Blogs {
			blogTick(&d.Blogs[i], lo, span)
		}
	}
	var ticks []schedTick
	var grid []string
	for m := lo; m <= hi; m += 60 {
		lab := fmt.Sprintf("%d %s", (m/60+11)%12+1, ampm(m))
		if m == 24*60 {
			lab = "12 AM"
		}
		ticks = append(ticks, schedTick{X: pctAt(m, lo, span), Label: lab, Anchor: edgeAnchor(m, lo, hi)})
		if m > lo && m < hi {
			grid = append(grid, pctAt(m, lo, span))
		}
	}
	// The zoomed day: ?day= when it is in this week, else the first day with
	// a conflict, else today, else the first day with posts.
	sel := -1
	for i, d := range days {
		if d.Date == q.Get("day") {
			sel = i
		}
	}
	for i, d := range days {
		if sel < 0 && d.Conflict {
			sel = i
		}
	}
	for i, d := range days {
		if sel < 0 && d.Today && d.Count > 0 {
			sel = i
		}
	}
	for i, d := range days {
		if sel < 0 && d.Count > 0 {
			sel = i
		}
	}
	if sel < 0 {
		sel = 0
	}
	days[sel].On = true
	for _, d := range days {
		for li, a := range accts {
			y := weekTop + li*weekLaneH
			lane := schedLane{Acct: a, Y: y, BaseY: y + 12, MY: y + 3, TY: y + 17}
			var mine []*schedItem
			for _, it := range d.items {
				if it.Acct != a.Handle {
					continue
				}
				mine = append(mine, it)
				lane.Marks = append(lane.Marks, schedMark{X: pctAt(it.Min, lo, span), W: pctAt(lo+schedWin, lo, span), Cls: it.Cls, Fill: it.Fill,
					Href: fmt.Sprintf("/drafts/%d", it.ID), Title: "@" + it.Acct + " " + it.Time + " " + it.Status + ": " + it.Preview})
			}
			lane.Labels = clusterLabels(mine, lo, span)
			d.Lanes = append(d.Lanes, lane)
		}
		d.EmptyY = weekTop + 17
	}
	var statList []*stat
	for _, a := range accts {
		statList = append(statList, stats[a.Handle])
	}
	nextLabel := ""
	if next != nil {
		nextLabel = next.ScheduledAt.In(loc).Format("Mon 3:04 PM") + " @" + next.Account
	}
	// New post from a day the owner picked suggests a time on that day.
	var kindChips []schedChip
	if len(blogs) > 0 {
		for _, k := range []struct{ label, kind string }{{"All", ""}, {"X", store.KindX}, {"Blog", store.KindBlog}} {
			kindChips = append(kindChips, schedChip{Label: k.label, Href: link(q.Get("from"), q.Get("day"), k.kind), On: kind == k.kind})
		}
	}
	statLine := func(st blogStat, done string) string {
		var parts []string
		for _, x := range []struct {
			n    int
			word string
		}{{st.Published, done}, {st.Scheduled, "scheduled"}, {st.Pending, "pending"}, {st.Failed, "failed"}} {
			if x.n > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", x.n, x.word))
			}
		}
		if len(parts) == 0 {
			return "none this week"
		}
		return strings.Join(parts, " · ")
	}
	nextBlogLabel, nextBlogNote := "", ""
	if nextBlog != nil {
		nextBlogLabel = nextBlog.ScheduledAt.In(loc).Format("Mon 3:04 PM")
		if b := blogs[nextBlog.Account]; b != nil {
			nextBlogNote = b.Name
		}
		if nextBlog.Status == store.StatusPending {
			nextBlogNote += ", if approved"
		}
	}
	compose := "/compose?from=schedule"
	if days[sel].Date == q.Get("day") {
		compose += "&day=" + days[sel].Date
	}
	p := s.newPage(r, u)
	p.Nav, p.Title, p.NewPost = "schedule", "Schedule", compose
	p.Data = map[string]any{
		"Compose": compose,
		"Days":    days, "Stats": statList, "Conflicts": conflicts, "FirstConflict": firstConflict, "Next": nextLabel,
		"Range": start.Format("Jan 2") + " to " + end.AddDate(0, 0, -1).Format("Jan 2"), "Zone": zoneLabel(loc, now),
		"Span": schedSpan(start, today, end),
		"Prev": start.AddDate(0, 0, -7).Format("2006-01-02"), "NextWeek": end.Format("2006-01-02"),
		"Accts": accts, "Ticks": ticks, "Grid": grid, "TrackH": weekTop + len(accts)*weekLaneH + 2, "Failed": failed,
		"Sel": days[sel], "Zoom": s.buildZoom(days[sel], accts), "Phone": s.buildPhone(days[sel], accts), "NBlogs": nBlogs,
		"KindChips": kindChips, "Kind": kind, "BlogStats": len(blogs) > 0 && kind != store.KindX,
		"NBlogPosts": bst.Total, "BlogLine": statLine(bst, "published"), "NPromos": pst.Total, "PromoLine": statLine(pst, "posted"),
		"NWait": nWait, "FirstWait": firstWait, "NextBlog": nextBlogLabel, "NextBlogNote": nextBlogNote,
		"PrevHref": link(start.AddDate(0, 0, -7).Format("2006-01-02"), "", kind), "NextHref": link(end.Format("2006-01-02"), "", kind), "ThisHref": link("", "", kind),
	}
	setLive(r, p, http.StatusOK, "schedule", seq)
	s.render(w, http.StatusOK, "schedule", "app", p)
}

func ceilHour(m int) int { return (m + 59) / 60 * 60 }

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// ---- Settings ----

var commonZones = []string{"America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Chicago", "America/New_York", "UTC", "Europe/London", "Asia/Kolkata", "Pacific/Auckland"}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, u *store.User) {
	flash := settingsFlash[r.URL.Query().Get("done")]
	if r.URL.Query().Get("done") == "hookfailed" {
		// Name the outcome, e.g. "HTTP 401", from the stored test result.
		if id, err := strconv.ParseInt(r.URL.Query().Get("agent"), 10, 64); err == nil {
			if h, err := s.st.AgentWebhook(r.Context(), id); err == nil && h.TestedAt != nil && !h.TestOK && h.TestDetail != "" {
				if h.TestDetail == "HTTP 401" || h.TestDetail == "HTTP 403" {
					flash = "Test failed: " + h.TestDetail + ". The receiver refused the request; check the Authorization header. Rewrite stays on."
				} else {
					flash = "Test failed: " + h.TestDetail + ". Rewrite stays on; check the URL and key."
				}
			}
		}
	}
	s.showSettings(w, r, u, http.StatusOK, "", flash)
}

var settingsFlash = map[string]string{"saved": "Settings saved.", "hooksaved": "Rewrite webhook saved.", "hookremoved": "Rewrite webhook removed. Rewrite is off for that agent.",
	"hooktested": "Test sent and answered.", "hookauthcleared": "Authorization header cleared. Requests go out without one.", "hookfailed": "The test did not get an answer. Rewrite stays on; check the URL and key.",
	"hookrefused": "Not sent: that webhook host resolves to a private or reserved address. Use a public https URL; rewrites to it will fail until then."}

func (s *Server) showSettingsHook(w http.ResponseWriter, r *http.Request, u *store.User, status int, tokenID int64, msg string) {
	s.showSettingsFor(w, r, u, status, "", "", tokenID, msg)
}

func (s *Server) showSettings(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash string) {
	s.showSettingsFor(w, r, u, status, errMsg, flash, 0, "")
}

func (s *Server) showSettingsFor(w http.ResponseWriter, r *http.Request, u *store.User, status int, errMsg, flash string, hookFor int64, hookErr string) {
	ctx := r.Context()
	capC, capSet := s.st.CapCents(ctx)
	capStr := ""
	if capSet {
		capStr = fmt.Sprintf("%d.%02d", capC/100, capC%100)
	}
	if status != http.StatusOK && r.Method == http.MethodPost && hookFor == 0 {
		capStr = r.PostFormValue("cap") // keep what they typed
	}
	sp, _ := s.st.MonthSpend(ctx, s.zone(r), s.st.Now())
	p := s.newPage(r, u)
	p.Nav, p.Title = "settings", "Settings"
	p.Error, p.Flash = errMsg, flash
	p.Data = map[string]any{"TZ": s.zone(r).String(), "Zones": commonZones, "Cap": capStr, "CapSet": capSet, "Block": sp.PostingBlock(), "RatesKnown": s.st.Rates.Known, "MediaBlocks": sp.MediaBlocks(),
		"Hooks": s.hookRows(r, s.zone(r), hookFor, hookErr)}
	s.render(w, status, "settings", "app", p)
}

// parseCap reads a dollar amount into cents: positive, at most $10,000,
// at most two decimals.
func parseCap(v string) (int64, bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "$"))
	v = strings.ReplaceAll(v, ",", "")
	if !capRe.MatchString(v) {
		return 0, false
	}
	whole, frac, _ := strings.Cut(v, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	for len(frac) < 2 {
		frac += "0"
	}
	f, _ := strconv.ParseInt(frac, 10, 64)
	c := w*100 + f
	return c, c >= store.MinCapCents && c <= store.MaxCapCents
}

var capRe = regexp.MustCompile(`^[0-9]{1,6}(\.[0-9]{1,2})?$`)

func (s *Server) settingsSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	tz := strings.TrimSpace(r.PostFormValue("tz"))
	if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
		s.showSettings(w, r, u, http.StatusBadRequest, "Use an IANA time zone name such as America/Denver.", "")
		return
	}
	old, hadCap := s.st.CapCents(ctx)
	capIn := strings.TrimSpace(r.PostFormValue("cap"))
	var capC int64
	if capIn != "" || hadCap {
		var ok bool
		if capC, ok = parseCap(capIn); !ok {
			s.showSettings(w, r, u, http.StatusBadRequest, "Enter the monthly cap in dollars, more than $0 and at most $10,000, e.g. 20.00.", "")
			return
		}
	}
	if err := s.st.SetSetting(ctx, store.SettingTZ, tz); err != nil {
		s.internalError(w, err)
		return
	}
	if capIn != "" || hadCap {
		if err := s.st.SetCapCents(ctx, u, capC); err != nil {
			s.internalError(w, err)
			return
		}
		if !hadCap || old != capC {
			was := "unset"
			if hadCap {
				was = cents(old)
			}
			s.log.Printf("settings: monthly spend cap changed from %s to %s by the owner", was, cents(capC))
		}
	}
	http.Redirect(w, r, "/settings?done=saved", http.StatusSeeOther)
}

func projText(micros int64) string {
	if micros < 0 {
		return "after day 3"
	}
	return money(micros)
}

// friendlyZones names common configured zones the way people say them. Any
// other zone shows its abbreviation at that time (JST, CET, +0545).
var friendlyZones = map[string]string{
	"America/New_York": "Eastern Time", "America/Detroit": "Eastern Time",
	"America/Chicago": "Central Time", "America/Denver": "Mountain Time",
	"America/Boise": "Mountain Time", "America/Phoenix": "Arizona Time",
	"America/Los_Angeles": "Pacific Time", "America/Anchorage": "Alaska Time",
	"Pacific/Honolulu": "Hawaii Time", "UTC": "UTC", "Etc/UTC": "UTC",
}

// zoneLabel is a short name for loc: friendly when known, else the zone
// abbreviation in effect at t.
func zoneLabel(loc *time.Location, t time.Time) string {
	if n, ok := friendlyZones[loc.String()]; ok {
		return n
	}
	return t.In(loc).Format("MST")
}

// schedSpan is the week's wording in the Schedule subtitle: "Next 7 days"
// for the default week from today, else its date range.
func schedSpan(start, today, end time.Time) string {
	if start.Equal(today) {
		return "Next 7 days"
	}
	return start.Format("Jan 2") + " to " + end.AddDate(0, 0, -1).Format("Jan 2")
}
