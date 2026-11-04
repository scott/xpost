package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/weight"
)

// The owner's composer ("New post"). Like the edit page it is one no-JS
// form: structural buttons (add or remove a part, images, Use 9:20, the
// format and When toggles) re-render it without saving; Save as draft and
// Schedule create the draft. xpost.js only adds the live counter, the
// Cancel confirm and auto-refresh on account or time changes.

func (s *Server) composeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /compose", s.requireOwner(s.composePage))
	mux.HandleFunc("POST /compose", s.requireOwner(s.composeSubmit))
	mux.HandleFunc("GET /compose/done/{id}", s.requireOwner(s.composeDone))
}

// composeState is what the form carries between round trips.
type composeState struct {
	From   string // "queue" or "schedule": where Cancel and Back go
	Nonce  string // one per composer: a resubmit returns the first draft
	Acct   string
	Parts  []editPart
	Mode   string // "next" (Next free slot) or "pick" (Pick a time)
	Date   string // pick inputs
	Clock  string
	Slot   int64 // the next free slot the form showed (unix)
	Active int   // the part open on a phone
}

const (
	modeNext = "next"
	modePick = "pick"
	// composeWarnAt turns the counter ring amber.
	composeWarnAt = 260
)

func newNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func composeOrigin(from string) (href, label string) {
	if from == "schedule" {
		return "/schedule", "Schedule"
	}
	return "/queue", "Review queue"
}

func (st *composeState) empty() bool {
	for _, p := range st.Parts {
		if strings.TrimSpace(p.Text) != "" || len(p.Media) > 0 {
			return false
		}
	}
	return true
}

// ceil5 rounds up to the next 5-minute mark (whole minutes, zero seconds).
func ceil5(t time.Time) time.Time {
	c := t.Truncate(5 * time.Minute)
	if c.Before(t) {
		c = c.Add(5 * time.Minute)
	}
	return c
}

// nextSlot is "Next free slot": the earliest 5-minute mark at least 10
// minutes from now that is MinSpacing clear of every other item on the
// account. It steps with moveTarget, the same earliest-clear-minute helper
// the queue and schedule use, then rounds up to the 5-minute grid.
func (s *Server) nextSlot(ctx context.Context, acct string, from time.Time) time.Time {
	t := ceil5(from)
	for i := 0; i < 7*288; i++ {
		m := s.moveTarget(ctx, acct, t, 0)
		if m.IsZero() {
			return m
		}
		c := ceil5(m)
		if c.Equal(m) {
			return m
		}
		t = c
	}
	return time.Time{}
}

func (s *Server) defaultAccount(ctx context.Context, accts []store.Account) string {
	last := s.st.LastOwnerAccount(ctx)
	for _, a := range accts {
		if a.Handle == last {
			return last
		}
	}
	if len(accts) > 0 {
		return accts[0].Handle
	}
	return ""
}

func (s *Server) composePage(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	q := r.URL.Query()
	loc := s.zone(r)
	accts, _ := s.st.Accounts(ctx)
	if len(accts) == 0 {
		// Nothing to post as yet: add an X account first.
		http.Redirect(w, r, accountsDone("noaccts"), http.StatusSeeOther)
		return
	}
	st := &composeState{From: "queue", Nonce: newNonce(), Parts: []editPart{{}}, Mode: modeNext}
	if q.Get("from") == "schedule" {
		st.From = "schedule"
	}
	st.Acct = s.defaultAccount(ctx, accts)
	if a := q.Get("account"); a != "" {
		for _, x := range accts {
			if x.Handle == a {
				st.Acct = a
			}
		}
	}
	// From the schedule with a day picked: pick a time on that day.
	if day, err := time.ParseInLocation("2006-01-02", q.Get("day"), loc); err == nil && st.Acct != "" {
		now := s.st.Now()
		start := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, loc)
		if end := start.AddDate(0, 0, 1).Add(-9 * time.Hour); end.After(now) {
			from := start
			if min := now.Add(10 * time.Minute); from.Before(min) {
				from = min
			}
			at := s.nextSlot(ctx, st.Acct, from)
			if at.IsZero() || at.In(loc).Format("2006-01-02") != day.Format("2006-01-02") {
				at = ceil5(from)
			}
			st.Mode, st.Date, st.Clock = modePick, day.Format("2006-01-02"), at.In(loc).Format("15:04")
		}
	}
	s.showCompose(w, r, u, st, http.StatusOK, "", false)
}

func composeFromForm(r *http.Request) *composeState {
	st := &composeState{From: "queue", Nonce: r.PostFormValue("nonce"), Acct: r.PostFormValue("acct"), Parts: partsFromForm(r),
		Mode: r.PostFormValue("mode"), Date: r.PostFormValue("date"), Clock: r.PostFormValue("time")}
	if r.PostFormValue("from") == "schedule" {
		st.From = "schedule"
	}
	if st.Mode != modePick {
		st.Mode = modeNext
	}
	if len(st.Nonce) != 32 {
		st.Nonce = newNonce()
	}
	st.Slot, _ = strconv.ParseInt(r.PostFormValue("slot"), 10, 64)
	st.Active, _ = strconv.Atoi(r.PostFormValue("active"))
	if len(st.Parts) == 0 {
		st.Parts = []editPart{{}}
	}
	if len(st.Parts) > store.MaxParts {
		st.Parts = st.Parts[:store.MaxParts]
	}
	return st
}

// splitPart cuts text into the longest prefix that fits weight.Limit,
// preferring a word boundary, and the rest.
func splitPart(text string) (string, string) {
	if weight.Count(text).Weighted <= weight.Limit {
		return text, ""
	}
	rs := []rune(text)
	fits := func(n int) bool { return weight.Count(string(rs[:n])).Weighted <= weight.Limit }
	// The longest fitting rune prefix (binary search; the count only grows).
	lo, hi := 0, len(rs)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cut := lo
	for i := lo; i > lo/2; i-- {
		if i < len(rs) && unicode.IsSpace(rs[i]) {
			cut = i
			break
		}
	}
	if cut == 0 {
		cut = lo
	}
	return strings.TrimRightFunc(string(rs[:cut]), unicode.IsSpace), strings.TrimLeftFunc(string(rs[cut:]), unicode.IsSpace)
}

func (s *Server) composeSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
	ctx := r.Context()
	loc := s.zone(r)
	st := composeFromForm(r)
	accts, _ := s.st.Accounts(ctx)
	known := false
	for _, a := range accts {
		known = known || a.Handle == st.Acct
	}
	if !known {
		st.Acct = s.defaultAccount(ctx, accts)
	}
	op := r.PostFormValue("op")
	msg := ""
	// New images first, so they survive any op.
	for i := range st.Parts {
		files, err := readUpload(r, fmt.Sprintf("add_%d", i))
		if err != nil {
			s.internalError(w, err)
			return
		}
		for _, b := range files {
			if len(st.Parts[i].Media) >= store.MaxImages {
				msg = fmt.Sprintf("Part %d already has %d images.", i+1, store.MaxImages)
				break
			}
			ref, mid, err := s.st.AddMedia(ctx, nil, b)
			if err != nil {
				var ie *store.InputError
				if errors.As(err, &ie) {
					msg = ie.Msg
					continue
				}
				s.internalError(w, err)
				return
			}
			mime, _, _ := s.st.Media(ctx, ref)
			st.Parts[i].Media = append(st.Parts[i].Media, store.MediaRef{ID: mid, Ref: ref, Mime: mime})
			st.Active = i
		}
	}
	idx := func(prefix string) (int, bool) {
		i, err := strconv.Atoi(strings.TrimPrefix(op, prefix))
		return i, err == nil && i >= 0 && i < len(st.Parts)
	}
	switch {
	case op == "fmt:thread":
		if len(st.Parts) == 1 {
			st.Parts = append(st.Parts, editPart{})
			st.Active = 1
		}
	case op == "fmt:single":
		keep := true
		for _, p := range st.Parts[1:] {
			if strings.TrimSpace(p.Text) != "" || len(p.Media) > 0 {
				keep = false
			}
		}
		if keep {
			st.Parts, st.Active = st.Parts[:1], 0
		} else {
			msg = fmt.Sprintf("Remove parts 2 to %d first to make this a single post.", len(st.Parts))
			if len(st.Parts) == 2 {
				msg = "Remove part 2 first to make this a single post."
			}
		}
	case op == "addpart":
		if len(st.Parts) < store.MaxParts {
			st.Parts = append(st.Parts, editPart{})
			st.Active = len(st.Parts) - 1
		}
	case strings.HasPrefix(op, "rmpart:"):
		if i, ok := idx("rmpart:"); ok && i > 0 {
			st.Parts = append(st.Parts[:i], st.Parts[i+1:]...)
			st.Active = min(st.Active, len(st.Parts)-1)
		}
	case strings.HasPrefix(op, "rmimg:"):
		var i, j int
		if _, err := fmt.Sscanf(op, "rmimg:%d:%d", &i, &j); err == nil && i >= 0 && i < len(st.Parts) && j >= 0 && j < len(st.Parts[i].Media) {
			st.Parts[i].Media = append(st.Parts[i].Media[:j], st.Parts[i].Media[j+1:]...)
			st.Active = i
		}
	case strings.HasPrefix(op, "split:"):
		if i, ok := idx("split:"); ok {
			head, rest := splitPart(st.Parts[i].Text)
			switch {
			case rest == "":
			case len(st.Parts) >= store.MaxParts:
				msg = fmt.Sprintf("A thread can have at most %d parts. Trim part %d instead.", store.MaxParts, i+1)
			default:
				st.Parts[i].Text = head
				tail := append([]editPart{{Text: rest}}, st.Parts[i+1:]...)
				st.Parts = append(st.Parts[:i+1], tail...)
				st.Active = i + 1
			}
		}
	case strings.HasPrefix(op, "open:"):
		if i, ok := idx("open:"); ok {
			st.Active = i
		}
	case op == "mode:next":
		st.Mode = modeNext
	case op == "mode:pick":
		if st.Mode != modePick {
			st.Mode = modePick
			if at := s.nextSlot(ctx, st.Acct, s.st.Now().Add(10*time.Minute)); !at.IsZero() {
				st.Date, st.Clock = at.In(loc).Format("2006-01-02"), at.In(loc).Format("15:04")
			}
		}
	case strings.HasPrefix(op, "use:"):
		// "Use 9:20": move only this post to the time the hint offered.
		if d, c, ok := strings.Cut(strings.TrimPrefix(op, "use:"), "T"); ok {
			if _, err := parseWhen(d, c, loc); err == nil {
				st.Mode, st.Date, st.Clock = modePick, d, c
			}
		}
	case op == "cancel":
		if st.empty() {
			href, _ := composeOrigin(st.From)
			http.Redirect(w, r, href, http.StatusSeeOther)
			return
		}
		s.showCompose(w, r, u, st, http.StatusOK, msg, true)
		return
	case op == "draft" || op == "schedule":
		s.composeCreate(w, r, u, st, op == "schedule")
		return
	}
	s.showCompose(w, r, u, st, http.StatusOK, msg, false)
}

// composeCreate validates like the page does (the client is never trusted)
// and creates the draft: approved for Schedule, pending for Save as draft.
func (s *Server) composeCreate(w http.ResponseWriter, r *http.Request, u *store.User, st *composeState, schedule bool) {
	ctx := r.Context()
	// A resubmit of a form that already made a draft (double click, back
	// and submit again) goes to that draft instead of a second one.
	if d := s.st.OwnerDraftByKey(ctx, st.Nonce); d != nil {
		composeRedirect(w, r, d, st.From)
		return
	}
	shown := st.Slot
	v := s.composeModel(r, st)
	if v.Acct.Handle == "" {
		s.showCompose(w, r, u, st, http.StatusBadRequest, "Add an X account first.", false)
		return
	}
	// Next free slot: the post goes at the slot the form showed. If that
	// slot is no longer what Next free slot gives, show the new one first.
	if st.Mode == modeNext && !v.At.IsZero() && v.At.Unix() != shown {
		s.showCompose(w, r, u, st, http.StatusConflict, "The next free slot changed to "+v.When.Day+" "+v.When.Time+". Check it, then try again.", false)
		return
	}
	block := v.DraftBlock
	if schedule {
		block = v.SchedBlock
	}
	if block != "" {
		s.showCompose(w, r, u, st, http.StatusBadRequest, block+".", false)
		return
	}
	od := store.OwnerDraft{Account: st.Acct, When: v.At, Key: st.Nonce}
	for _, p := range st.Parts {
		in := store.PartIn{Text: p.Text}
		for _, m := range p.Media {
			in.Media = append(in.Media, store.MediaIn{ID: m.ID, Alt: m.Alt})
		}
		od.Parts = append(od.Parts, in)
	}
	d, _, err := s.st.CreateOwnerDraft(ctx, u, od, schedule)
	if err != nil {
		code, m := reviewError(err)
		if code == 0 {
			s.internalError(w, err)
			return
		}
		s.showCompose(w, r, u, st, code, m, false)
		return
	}
	composeRedirect(w, r, d, st.From)
}

func composeRedirect(w http.ResponseWriter, r *http.Request, d *store.Draft, from string) {
	if d.Status == store.StatusApproved {
		http.Redirect(w, r, "/compose/done/"+strconv.FormatInt(d.ID, 10)+"?from="+from, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, withFlash("/queue", "drafted"), http.StatusSeeOther)
}

// ---- view model ----

type composePart struct {
	partView
	Role    string // "Post" or "Reply"
	Link    bool
	Warn    bool // over composeWarnAt, not over the limit
	Left    int
	Open    bool // the expanded part on a phone
	Preview string
}

type acctOpt struct {
	store.Account
	On bool
}

type costItem struct {
	Label, Amount string
	Link          bool
}

type laneItem struct {
	X, Cls string
}

type laneWin struct {
	X, W, Cls string
}

type laneLabel struct {
	X, Text, Cls, Anchor string
}

// laneView is the two-hour preview of one account's lane.
type laneView struct {
	Title, Aria string
	Grid        []string
	Ticks       []laneLabel
	Wins        []laneWin
	Items       []laneItem
	Up          []laneLabel // "This post 9:15"
	Down        []laneLabel // other items and the suggested time
	Now         string
	Posted      bool
	Pending     bool
	Conflict    bool
	Fix         bool
	NoKey       bool // no legend (the confirmation)
}

type capBar struct {
	X, W, Cls string
}

type capView struct {
	Show    bool // cap and rates set
	Text    string
	Left    string
	Over    string
	Cap     string
	Spent   string
	Sched   string
	Bars    []capBar
	Example bool
	Need    string
	Room    string
	Tip     string // how to get under, if there is an obvious way
	Month   string // set when the post is in another month
}

type composeView struct {
	St          *composeState
	Back        string
	BackLabel   string
	Accts       []acctOpt
	Acct        store.Account
	Parts       []composePart
	NParts      int
	Thread      bool
	CanAddPart  bool
	Limit       int
	Max         int
	At          time.Time
	When        whenView
	SlotLabel   string
	ZoneName    string
	Lane        *laneView
	Spacing     string
	Conflict    string
	PendingWarn string // pending neighbours: shown, never blocks Schedule
	UseDate     string
	UseClock    string
	UseLabel    string
	TimeError   string
	Items       []costItem
	Summary     string
	Total       string
	RateLine    string
	LinkTip     string
	CostNote    string // unverified rates
	Cap         capView
	CostBlock   string // why Schedule is off for cost reasons (panel text)
	SchedBlock  string // "" when Schedule is allowed
	DraftBlock  string // "" when Save as draft is allowed
	Other       string // the non-text Schedule reason (for xpost.js)
	DraftOther  string // the non-text Save as draft reason
	Zone        string
	LinkRate    string
	OtherTail   string
	Reason      string
	Confirm     bool
	Empty       bool
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (s *Server) composeModel(r *http.Request, st *composeState) *composeView {
	ctx := r.Context()
	loc := s.zone(r)
	now := s.st.Now()
	accts, _ := s.st.Accounts(ctx)
	v := &composeView{St: st, Limit: weight.Limit, Max: store.MaxImages, NParts: len(st.Parts), Thread: len(st.Parts) > 1,
		CanAddPart: len(st.Parts) < store.MaxParts, ZoneName: loc.String(), Empty: st.empty()}
	v.Back, v.BackLabel = composeOrigin(st.From)
	v.Zone = now.In(loc).Format("MST")
	v.LinkRate = s.rateText(s.st.Rates.LinkPostMicros)
	for _, a := range accts {
		v.Accts = append(v.Accts, acctOpt{Account: a, On: a.Handle == st.Acct})
		if a.Handle == st.Acct {
			v.Acct = a
		}
	}
	// Parts.
	pv, firstOver := viewParts(st.Parts)
	if firstOver > 0 {
		st.Active = firstOver - 1
	}
	if st.Active < 0 || st.Active >= len(st.Parts) {
		st.Active = 0
	}
	var links []int
	for i, p := range pv {
		cp := composePart{partView: p, Role: "Reply", Link: p.URLs > 0, Warn: p.Over == 0 && p.Count > composeWarnAt, Left: weight.Limit - p.Count, Open: i == st.Active}
		if i == 0 {
			cp.Role = "Post"
		}
		cp.Preview = strings.Join(strings.Fields(p.Text), " ")
		if cp.Preview == "" {
			cp.Preview = "Empty"
		}
		if cp.Link {
			links = append(links, i+1)
		}
		v.Parts = append(v.Parts, cp)
	}
	// When.
	if st.Mode == modeNext {
		if v.Acct.Handle != "" {
			v.At = s.nextSlot(ctx, v.Acct.Handle, now.Add(10*time.Minute))
		}
		st.Slot = 0
		if !v.At.IsZero() {
			st.Slot = v.At.Unix()
		}
	} else if at, err := parseWhen(st.Date, st.Clock, loc); err != nil {
		v.TimeError = "Pick a date and a time"
	} else if !at.After(now) {
		v.At, v.TimeError = at, "Pick a time in the future"
	} else {
		v.At = at
	}
	if !v.At.IsZero() {
		v.When = s.when(v.At, loc)
		v.Zone = v.When.Zone
		day := v.When.Day
		if l, n := v.At.In(loc), now.In(loc); l.Format("2006-01-02") == n.Format("2006-01-02") {
			day = "Today, " + l.Format("Mon Jan 2")
		} else if l.Format("2006-01-02") == n.AddDate(0, 0, 1).Format("2006-01-02") {
			day = "Tomorrow, " + l.Format("Mon Jan 2")
		}
		v.SlotLabel = day + " · " + v.When.Time
		if st.Mode == modeNext {
			st.Date, st.Clock = v.When.Date, v.When.Clock
		}
	}
	if v.Acct.Handle != "" && !v.At.IsZero() && v.TimeError == "" {
		s.composeSpacing(r, v, loc)
		v.Lane = s.composeLane(r, v.Acct.Handle, v.At, loc, v.Conflict != "", v.UseDate != "", v.useAt(loc))
	}
	// Cost.
	parts := make([]store.Part, len(st.Parts))
	plain, linked, images, alts := 0, 0, 0, 0
	for i, p := range st.Parts {
		parts[i] = store.Part{Text: p.Text, Media: p.Media}
		if strings.TrimSpace(p.Text) == "" && len(p.Media) == 0 {
			continue
		}
		if weight.HasURL(p.Text) {
			linked++
		} else {
			plain++
		}
		images += len(p.Media)
		for _, m := range p.Media {
			if strings.TrimSpace(m.Alt) != "" {
				alts++
			}
		}
	}
	rates := s.st.Rates
	est := s.st.PartsEstimate(parts)
	if v.Empty {
		est = 0
		v.Items = []costItem{{Label: "Nothing to post yet", Amount: s.rateText(0)}}
	} else {
		if plain > 0 {
			v.Items = append(v.Items, costItem{Label: fmt.Sprintf("%d %s × %s", plain, plural(plain, "part", "parts"), s.rateText(rates.PostMicros)), Amount: s.rateText(int64(plain) * rates.PostMicros)})
		}
		if linked > 0 {
			v.Items = append(v.Items, costItem{Label: fmt.Sprintf("%d %s with a link × %s", linked, plural(linked, "part", "parts"), s.rateText(rates.LinkPostMicros)), Amount: s.rateText(int64(linked) * rates.LinkPostMicros), Link: true})
		}
		if images > 0 {
			v.Items = append(v.Items, costItem{Label: fmt.Sprintf("%d %s × %s", images, plural(images, "image", "images"), s.costText(rates.UploadMicros, images, 0)), Amount: s.costText(s.st.UploadCost(images), images, 0)})
		}
		if alts > 0 {
			v.Items = append(v.Items, costItem{Label: fmt.Sprintf("%d alt text × %s", alts, s.costText(rates.AltMicros, 1, 1)), Amount: s.costText(s.st.AltCost(alts), 1, alts)})
		}
		n := plain + linked
		sum := fmt.Sprintf("%d %s", n, plural(n, "part", "parts"))
		if linked > 0 {
			sum += fmt.Sprintf(" (%d with a link)", linked)
		}
		if images > 0 {
			sum += fmt.Sprintf(", %d %s", images, plural(images, "image", "images"))
		}
		if alts > 0 {
			sum += fmt.Sprintf(", %d alt text", alts)
		}
		v.Summary = sum
	}
	v.Total = s.costText(est, images, alts)
	if rates.Known {
		v.RateLine = fmt.Sprintf("Each part %s, or %s with a link.", money(rates.PostMicros), money(rates.LinkPostMicros))
		switch {
		case rates.UploadKnown && rates.AltKnown && rates.UploadMicros == rates.AltMicros:
			v.RateLine += fmt.Sprintf(" Images and alt text %s each.", money(rates.UploadMicros))
		case rates.UploadKnown && rates.AltKnown:
			v.RateLine += fmt.Sprintf(" Images %s and alt text %s each.", money(rates.UploadMicros), money(rates.AltMicros))
		}
		if len(links) > 0 && rates.LinkPostMicros != rates.PostMicros {
			if len(links) == 1 {
				v.LinkTip = fmt.Sprintf("Part %d has a link, so it costs %s instead of %s.", links[0], money(rates.LinkPostMicros), money(rates.PostMicros))
			} else {
				v.LinkTip = fmt.Sprintf("Parts %s have links, so each costs %s instead of %s.", joinInts(links), money(rates.LinkPostMicros), money(rates.PostMicros))
			}
			if len(st.Parts) == 1 {
				v.LinkTip = fmt.Sprintf("This post has a link, so it costs %s instead of %s.", money(rates.LinkPostMicros), money(rates.PostMicros))
			}
		}
	} else {
		v.CostNote = "Cost rates are not configured on this server, so this is unverified."
	}
	s.composeCap(r, v, loc, est, images, alts, links)
	s.composeReasons(v)
	return v
}

func (v *composeView) useAt(loc *time.Location) time.Time {
	if v.UseDate == "" {
		return time.Time{}
	}
	t, _ := parseWhen(v.UseDate, v.UseClock, loc)
	return t
}

func joinInts(ns []int) string {
	ss := make([]string, len(ns))
	for i, n := range ns {
		ss[i] = strconv.Itoa(n)
	}
	if len(ss) == 2 {
		return ss[0] + " and " + ss[1]
	}
	return strings.Join(ss[:len(ss)-1], ", ") + " and " + ss[len(ss)-1]
}

// shortClock is "9:05", with the day when it is not at's day.
func shortClock(t, at time.Time, loc *time.Location) string {
	if t.In(loc).Format("2006-01-02") != at.In(loc).Format("2006-01-02") {
		return t.In(loc).Format("Mon 3:04")
	}
	return t.In(loc).Format("3:04")
}

// composeSpacing fills the spacing line, and on a conflict the Use target
// from moveTarget (only this post moves).
func (s *Server) composeSpacing(r *http.Request, v *composeView, loc *time.Location) {
	at, acct := v.At, v.Acct.Handle
	var block, pend []store.Neighbor
	for _, o := range s.st.Neighbors(r.Context(), acct, at, 0) {
		if o.Status == store.StatusPending {
			pend = append(pend, o)
		} else {
			block = append(block, o)
		}
	}
	if len(pend) > 0 {
		v.PendingWarn = pendingWarn(pend, at, loc)
	}
	if ns := block; len(ns) > 0 {
		n := ns[0]
		for _, o := range ns {
			if absDur(o.At.Sub(at)) < absDur(n.At.Sub(at)) {
				n = o
			}
		}
		gap := n.At.Sub(at)
		rel := "before"
		if gap <= 0 {
			rel = "after"
		}
		need := int(store.MinSpacing.Minutes())
		v.Conflict = fmt.Sprintf("%d min %s %s. Needs %d.", int(absDur(gap).Minutes()), rel, shortClock(n.At, at, loc), need)
		if free := s.moveTarget(r.Context(), acct, at, 0); !free.IsZero() {
			w := s.when(free, loc)
			v.UseDate, v.UseClock, v.UseLabel = w.Date, w.Clock, shortClock(free, at, loc)
			v.Conflict += " Use " + v.UseLabel + "."
		}
		return
	}
	if len(pend) > 0 {
		return
	}
	ds, err := s.st.Between(r.Context(), at.Add(-24*time.Hour), at.Add(24*time.Hour))
	if err != nil {
		return
	}
	var prev, next *store.Draft
	for _, o := range ds {
		if o.Account != acct || o.Status == store.StatusFailed {
			continue
		}
		if !o.ScheduledAt.After(at) {
			prev = o
		} else if next == nil {
			next = o
		}
	}
	var out []string
	if prev != nil {
		out = append(out, fmt.Sprintf("%s after %s.", gapText(at.Sub(prev.ScheduledAt)), shortClock(prev.ScheduledAt, at, loc)))
	}
	if next != nil {
		out = append(out, fmt.Sprintf("%s before %s.", gapText(next.ScheduledAt.Sub(at)), shortClock(next.ScheduledAt, at, loc)))
	}
	if len(out) == 0 {
		out = append(out, "No other @"+acct+" posts within a day.")
	}
	v.Spacing = strings.Join(out, " ") + " Spacing OK."
}

// pendingWarn says which pending drafts sit within MinSpacing of at. They
// do not block Schedule (only approved posts do, as on approve), but each
// will need moving before it can be approved.
func pendingWarn(ns []store.Neighbor, at time.Time, loc *time.Location) string {
	var ts []string
	for _, n := range ns {
		ts = append(ts, shortClock(n.At, at, loc))
	}
	if len(ts) == 1 {
		return "A pending draft at " + ts[0] + " would conflict; it'll need moving to be approved."
	}
	return "Pending drafts at " + strings.Join(ts[:len(ts)-1], ", ") + " and " + ts[len(ts)-1] + " would conflict; they'll need moving to be approved."
}

// composeLane is the two hours around at on one account's lane: from 30
// minutes before at's quarter hour.
func (s *Server) composeLane(r *http.Request, acct string, at time.Time, loc *time.Location, conflict, fix bool, fixAt time.Time) *laneView {
	l := at.In(loc)
	start := l.Truncate(time.Minute).Add(-time.Duration(l.Minute()%15) * time.Minute).Add(-30 * time.Minute)
	end := start.Add(2 * time.Hour)
	span := end.Sub(start).Minutes()
	pct := func(t time.Time) float64 { return t.Sub(start).Minutes() * 100 / span }
	ps := func(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) + "%" }
	anchor := func(f float64) string {
		switch {
		case f < 7:
			return "start"
		case f > 93:
			return "end"
		}
		return "middle"
	}
	lv := &laneView{Title: "@" + acct + " on " + l.Format("Mon, Jan 2"), Conflict: conflict, Fix: fix}
	if l.Format("2006-01-02") == s.st.Now().In(loc).Format("2006-01-02") {
		lv.Title = "@" + acct + " today"
	}
	lv.Aria = "@" + acct + " queue around this time"
	winW := ps(store.MinSpacing.Minutes() * 100 / span)
	var downX []float64
	addDown := func(f float64, text, cls string) {
		for _, x := range downX {
			if absF(x-f) < 11 {
				return
			}
		}
		downX = append(downX, f)
		lv.Down = append(lv.Down, laneLabel{X: ps(f), Text: text, Cls: cls, Anchor: anchor(f)})
	}
	thisX := pct(at)
	ds, _ := s.st.Between(r.Context(), start.Add(-store.MinSpacing), end.Add(store.MinSpacing))
	for _, d := range ds {
		if d.Account != acct || d.Status == store.StatusFailed {
			continue
		}
		f := pct(d.ScheduledAt)
		cls := "ap"
		switch d.Status {
		case store.StatusPosted:
			cls, lv.Posted = "po", true
		case store.StatusPending:
			cls, lv.Pending = "pe", true
		}
		lv.Wins = append(lv.Wins, laneWin{X: ps(f), W: winW, Cls: ""})
		lv.Items = append(lv.Items, laneItem{X: ps(f), Cls: cls})
		if f >= 0 && f <= 100 && absF(f-thisX) >= 4 {
			addDown(f, d.ScheduledAt.In(loc).Format("3:04"), "")
		}
	}
	me := "me"
	if conflict {
		me = "co"
	}
	lv.Wins = append(lv.Wins, laneWin{X: ps(thisX), W: winW, Cls: me})
	lv.Items = append(lv.Items, laneItem{X: ps(thisX), Cls: me})
	lv.Up = append(lv.Up, laneLabel{X: ps(thisX), Text: "This post " + l.Format("3:04"), Cls: me, Anchor: anchor(thisX)})
	if fix && !fixAt.IsZero() {
		if f := pct(fixAt); f >= 0 && f <= 100 {
			lv.Items = append(lv.Items, laneItem{X: ps(f), Cls: "gh"})
			downX = downX[:0:0]
			// The suggestion wins over a neighbor label it would hit.
			keep := lv.Down[:0]
			for _, d := range lv.Down {
				x, _ := strconv.ParseFloat(strings.TrimSuffix(d.X, "%"), 64)
				if absF(x-f) >= 11 {
					keep = append(keep, d)
					downX = append(downX, x)
				}
			}
			lv.Down = keep
			addDown(f, fixAt.In(loc).Format("3:04"), "acc")
		}
	}
	for t := start.Truncate(30 * time.Minute); !t.After(end); t = t.Add(30 * time.Minute) {
		f := pct(t)
		if f <= 0 || f >= 100 {
			continue
		}
		lv.Grid = append(lv.Grid, ps(f))
		if f < 20 || f > 80 {
			continue
		}
		clear := true
		for _, x := range downX {
			if absF(x-f) < 11 {
				clear = false
			}
		}
		if clear {
			lv.Ticks = append(lv.Ticks, laneLabel{X: ps(f), Text: t.In(loc).Format("3:04"), Anchor: "middle"})
		}
	}
	if now := s.st.Now(); now.After(start) && now.Before(end) {
		lv.Now = ps(pct(now))
	}
	return lv
}

// gapText is "25 min" under two hours, else "3 h" or "18 h 15 min".
func gapText(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m < 120:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// composeCap is the cap meter. Room is the cap minus what the month has
// spent minus every approved post already scheduled in that month, all at
// the dispatcher's estimate. It fails closed: no cap, or rates not set
// (post rates always, the image and alt text rates when the post has
// them), turns Schedule off.
func (s *Server) composeCap(r *http.Request, v *composeView, loc *time.Location, est int64, images, alts int, links []int) {
	ctx := r.Context()
	when := v.At
	if when.IsZero() {
		when = s.st.Now()
	}
	sp, err := s.st.MonthSpend(ctx, loc, when)
	if err != nil {
		v.CostBlock = "Spend could not be read"
		return
	}
	switch {
	case !sp.CapSet:
		v.CostBlock = "No monthly spend cap is set. Set one in Settings to schedule; Save as draft still works."
		return
	case !sp.Rates.Known:
		v.CostBlock = "Cost rates are not configured on this server (XPOST_COST_POST_USD and XPOST_COST_LINK_POST_USD), so the cap can't be checked. Schedule is off until they are set; Save as draft still works."
		return
	case images > 0 && !sp.Rates.UploadKnown:
		v.CostBlock = "The image upload cost rate (XPOST_COST_MEDIA_UPLOAD_USD) is not configured, so a post with images can't be checked against the cap. Schedule is off for it; Save as draft still works."
	case alts > 0 && !sp.Rates.AltKnown:
		v.CostBlock = "The alt text cost rate (XPOST_COST_MEDIA_METADATA_USD) is not configured, so a post with alt text can't be checked against the cap. Schedule is off for it; Save as draft still works."
	}
	start := sp.Month
	sched, err := s.st.ApprovedBetween(ctx, start, start.AddDate(0, 1, 0))
	if err != nil {
		v.CostBlock = "Spend could not be read"
		return
	}
	capM := sp.CapMicros()
	room := capM - sp.Micros - sched
	c := capView{Show: true, Cap: cents(sp.CapCents), Spent: money(sp.Micros), Sched: money(sched), Example: sp.Example}
	if m := start.In(loc); m.Format("2006-01") != s.st.Now().In(loc).Format("2006-01") {
		c.Month = m.Format("January")
	}
	if est > room {
		c.Over = money(est - max(room, 0))
		c.Need, c.Room = money(est), money(max(room, 0))
		if len(links) > 0 && s.st.Rates.LinkPostMicros > s.st.Rates.PostMicros {
			c.Tip = fmt.Sprintf("Remove the link in part %d to save %s", links[0], money(s.st.Rates.LinkPostMicros-s.st.Rates.PostMicros))
		}
		if v.CostBlock == "" {
			v.CostBlock = "over"
		}
	} else {
		c.Left = money(room - est)
	}
	// The bar: spent, scheduled, this post (red past the cap).
	if capM > 0 {
		w := func(m int64) float64 { return float64(max(m, 0)) * 100 / float64(capM) }
		x := 0.0
		add := func(m int64, cls string) {
			f := min(w(m), 100-x)
			if f <= 0 {
				return
			}
			c.Bars = append(c.Bars, capBar{X: strconv.FormatFloat(x, 'f', 2, 64) + "%", W: strconv.FormatFloat(f, 'f', 2, 64) + "%", Cls: cls})
			x += f
		}
		add(sp.Micros, "sp")
		add(sched, "sc")
		if est > room {
			add(max(room, 0), "me")
			add(est-max(room, 0), "ov")
		} else {
			add(est, "me")
		}
	}
	v.Cap = c
}

// composeReasons decides what Schedule and Save as draft may do, and the
// one reason line the action bar shows.
func (s *Server) composeReasons(v *composeView) {
	var text string
	firstOver := 0
	for _, p := range v.Parts {
		if p.Over > 0 && firstOver == 0 {
			firstOver = p.N
		}
	}
	emptyPart := 0
	if !v.Empty {
		for i, p := range v.St.Parts {
			if strings.TrimSpace(p.Text) == "" && len(p.Media) == 0 {
				emptyPart = i + 1
				break
			}
		}
	}
	switch {
	case v.Acct.Handle == "":
		text = "Add an X account first"
	case v.Empty:
		text = "Write something to schedule"
	case firstOver > 0:
		text = fmt.Sprintf("Part %d is over %d", firstOver, weight.Limit)
	case emptyPart > 0:
		text = fmt.Sprintf("Part %d is empty. Write it or remove it", emptyPart)
	}
	// Non-text reasons, from this render (the server re-checks on submit).
	other, tail := "", ""
	switch {
	case v.TimeError != "":
		other, tail = v.TimeError, "needs a time in the future"
	case v.Conflict != "":
		other, tail = "Fix spacing to schedule", "needs spacing fixed"
	case v.CostBlock == "over":
		other, tail = "Over the monthly cap", "over the monthly cap"
	case v.CostBlock != "":
		other, tail = "Schedule is off until the cost check can run", "can't be checked against the cap"
	}
	v.Other, v.OtherTail = other, tail
	draftOther := v.TimeError
	if v.Acct.Handle == "" {
		draftOther = "Add an X account first"
	}
	v.DraftOther = draftOther
	switch {
	case text != "" && tail != "" && firstOver > 0:
		v.SchedBlock = text + " and " + tail
	case text != "":
		v.SchedBlock = text
	default:
		v.SchedBlock = other
	}
	if text != "" {
		v.DraftBlock = strings.Replace(text, "to schedule", "to save", 1)
	} else {
		v.DraftBlock = draftOther
	}
	v.Reason = v.SchedBlock
}

func (s *Server) showCompose(w http.ResponseWriter, r *http.Request, u *store.User, st *composeState, status int, msg string, confirm bool) {
	v := s.composeModel(r, st)
	v.Confirm = confirm
	p := s.newPage(r, u)
	p.Nav, p.Title, p.CancelForm = st.From, "New post", "cf"
	p.Error = msg
	p.Data = v
	s.render(w, status, "compose", "app", p)
}

// ---- after Schedule ----

func (s *Server) composeDone(w http.ResponseWriter, r *http.Request, u *store.User) {
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
	loc := s.zone(r)
	accts := s.accountMap(r)
	from := "queue"
	if r.URL.Query().Get("from") == "schedule" {
		from = "schedule"
	}
	card := s.card(r, d, accts, loc)
	lane := s.composeLane(r, d.Account, d.ScheduledAt, loc, false, false, time.Time{})
	lane.NoKey = true
	l := d.ScheduledAt.In(loc)
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = from, "Scheduled", "/queue"
	if from == "schedule" {
		p.Back = "/schedule"
	}
	p.Data = map[string]any{
		"Card": card, "Heading": "Scheduled for " + l.Format("Mon 3:04 PM"), "Date": l.Format("Mon, Jan 2, 2006 · 3:04 PM MST"),
		"Approved": d.Status == store.StatusApproved, "Lane": lane,
		"Schedule": "/schedule?from=" + l.Format("2006-01-02") + "&day=" + l.Format("2006-01-02"), "Another": "/compose?from=" + from,
		"Mine": d.TokenID == 0,
	}
	s.render(w, http.StatusOK, "compose_done", "app", p)
}
