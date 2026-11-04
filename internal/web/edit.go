package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
	"github.com/scott/xpost/internal/weight"
)

// The edit page works without JavaScript: the whole draft is one form, and
// structural buttons (add part, move up, remove, remove image, pick a slot,
// recount) re-render it from the submitted fields without saving. Only
// "save" and "approve" write, through store.Save.

type editPart struct {
	Text  string
	Media []store.MediaRef
}

type partView struct {
	I      int
	N      int
	Text   string
	Count  int
	Over   int
	URLs   int
	Ring   int // 0..100 for the counter ring
	Media  []store.MediaRef
	CanAdd bool
	First  bool
	Label  string
}

func partsFromDraft(d *store.Draft) []editPart {
	out := make([]editPart, len(d.Parts))
	for i, p := range d.Parts {
		out[i] = editPart{Text: p.Text, Media: append([]store.MediaRef(nil), p.Media...)}
	}
	return out
}

// partsFromForm rebuilds the parts from the submitted fields.
func partsFromForm(r *http.Request) []editPart {
	n, _ := strconv.Atoi(r.PostFormValue("parts"))
	n = min(max(n, 0), store.MaxParts+1)
	out := make([]editPart, 0, n)
	for i := 0; i < n; i++ {
		p := editPart{Text: strings.ReplaceAll(r.PostFormValue(fmt.Sprintf("text_%d", i)), "\r\n", "\n")}
		m, _ := strconv.Atoi(r.PostFormValue(fmt.Sprintf("media_%d", i)))
		for j := 0; j < min(m, store.MaxImages+1); j++ {
			id, _ := strconv.ParseInt(r.PostFormValue(fmt.Sprintf("mid_%d_%d", i, j)), 10, 64)
			if id <= 0 {
				continue
			}
			p.Media = append(p.Media, store.MediaRef{ID: id, Ref: r.PostFormValue(fmt.Sprintf("mref_%d_%d", i, j)), Alt: r.PostFormValue(fmt.Sprintf("alt_%d_%d", i, j))})
		}
		out = append(out, p)
	}
	return out
}

// hasGIF reports whether a part holds a GIF (it must stay alone, so the
// Add tile is hidden). Mime is known for stored parts; a part rebuilt from
// a refused form may not carry it, and the store refuses the mix anyway.
func hasGIF(ms []store.MediaRef) bool {
	for _, m := range ms {
		if m.Mime == "image/gif" {
			return true
		}
	}
	return false
}

func viewParts(parts []editPart) ([]partView, int) {
	firstOver := 0
	out := make([]partView, len(parts))
	for i, p := range parts {
		c := weight.Count(p.Text)
		label := "Reply in thread"
		if i == 0 {
			label = "Post"
		}
		out[i] = partView{I: i, N: i + 1, Text: p.Text, Count: c.Weighted, Over: c.Over(), URLs: c.URLs, Ring: min(100, c.Weighted*100/weight.Limit),
			Media: p.Media, CanAdd: len(p.Media) < store.MaxImages && !hasGIF(p.Media), First: i == 0, Label: label}
		if c.Over() > 0 && firstOver == 0 {
			firstOver = i + 1
		}
	}
	return out, firstOver
}

func (s *Server) editPage(w http.ResponseWriter, r *http.Request, u *store.User) {
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
	if d.IsBlog() {
		s.blogPreview(w, r, u, d)
		return
	}
	if d.IsReply() && len(d.Parts) == 1 {
		s.showReplyEdit(w, r, u, d, d.Parts[0].Text, http.StatusOK, "", false)
		return
	}
	s.showEdit(w, r, u, d, partsFromDraft(d), d.ScheduledAt, http.StatusOK, "")
}

type costLine struct {
	N     int
	Label string
	Rate  string
}

func (s *Server) showEdit(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft, parts []editPart, at time.Time, status int, msg string) {
	s.showEditV(w, r, u, d, parts, at, status, msg, false)
}

func (s *Server) showEditV(w http.ResponseWriter, r *http.Request, u *store.User, d *store.Draft, parts []editPart, at time.Time, status int, msg string, stale bool) {
	seq := s.liveSeqNow(r)
	ctx := r.Context()
	loc := s.zone(r)
	pv, firstOver := viewParts(parts)
	plain, links, images, alts := 0, 0, 0, 0
	var total int64
	for _, p := range parts {
		if weight.HasURL(p.Text) {
			links++
		} else {
			plain++
		}
		images += len(p.Media)
		for _, m := range p.Media {
			if strings.TrimSpace(m.Alt) != "" {
				alts++
			}
		}
		total += s.st.PartCost(weight.HasURL(p.Text))
	}
	total += s.st.UploadCost(images) + s.st.AltCost(alts)
	var lines []costLine
	if plain > 0 {
		lines = append(lines, costLine{plain, "post", s.rateText(s.st.Rates.PostMicros)})
	}
	if links > 0 {
		lines = append(lines, costLine{links, "post with link", s.rateText(s.st.Rates.LinkPostMicros)})
	}
	if images > 0 {
		lines = append(lines, costLine{images, "image upload", s.costText(s.st.Rates.UploadMicros, images, 0)})
	}
	if alts > 0 {
		lines = append(lines, costLine{alts, "alt text", s.costText(s.st.Rates.AltMicros, 0, alts)})
	}
	events, _ := s.st.Events(ctx, d.ID)
	type evView struct{ Who, What, At string }
	var evs []evView
	for _, e := range events {
		who := e.Actor
		if a, ok := strings.CutPrefix(who, "agent:"); ok {
			who = a
		}
		what := strings.ToUpper(e.Kind[:1]) + e.Kind[1:]
		if e.Detail != "" && e.Kind == "rejected" {
			what += ": " + e.Detail
		}
		evs = append(evs, evView{Who: who, What: what, At: e.At.In(loc).Format("Jan 2, 3:04 PM")})
	}
	accts := s.accountMap(r)
	p := s.newPage(r, u)
	p.Nav, p.Title, p.Back = "queue", "Edit draft", "/queue"
	p.Error = msg
	editable := d.Status == store.StatusPending || d.Status == store.StatusApproved
	p.Data = map[string]any{
		"D": d, "Acct": accts[d.Account], "Parts": pv, "NParts": len(pv), "CanAddPart": len(pv) < store.MaxParts, "FirstOver": firstOver,
		"When": s.when(at, loc), "Orig": s.when(d.ScheduledAt, loc), "Slots": s.slots(r, d, at, loc), "Spacing": s.spacingNote(r, d, at, loc),
		"Conflicts": s.conflicts(r, d, at, loc), "CostLines": lines, "Cost": s.costText(total, images, alts), "RatesKnown": s.st.Rates.Known,
		"UploadUnverified": images > 0 && !s.st.Rates.UploadKnown, "AltUnverified": alts > 0 && !s.st.Rates.AltKnown, "Events": evs,
		"Editable": editable, "Approved": d.Status == store.StatusApproved, "Deletable": d.Status != store.StatusPosting && d.Status != store.StatusPosted,
		"Max": store.MaxImages, "Limit": weight.Limit, "ZoneName": loc.String(), "V": d.Version,
	}
	// A re-render keeps the version the owner first opened, so a save can
	// never apply over changes they have not seen.
	if v := formVersion(r); r.Method == http.MethodPost && v > 0 && !stale {
		p.Data.(map[string]any)["V"] = v
	}
	setLive(r, p, status, "draft", seq)
	p.LiveDraft, p.LiveV = d.ID, d.Version
	s.render(w, status, "edit", "app", p)
}

func (s *Server) editSubmit(w http.ResponseWriter, r *http.Request, u *store.User) {
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
	if d.IsBlog() {
		s.notFound(w, r) // blog posts are edited at /drafts/{id}/edit
		return
	}
	if d.IsReply() && len(d.Parts) == 1 {
		s.replyEditSubmit(w, r, u, d)
		return
	}
	if d.Status != store.StatusPending && d.Status != store.StatusApproved {
		s.showEdit(w, r, u, d, partsFromDraft(d), d.ScheduledAt, http.StatusConflict, "This draft can't be edited in its current state.")
		return
	}
	loc := s.zone(r)
	parts := partsFromForm(r)
	op := r.PostFormValue("op")
	clock := r.PostFormValue("time")
	if c, ok := strings.CutPrefix(op, "slot:"); ok {
		clock = c
	}
	at, perr := parseWhen(r.PostFormValue("date"), clock, loc)
	if perr != nil {
		at = d.ScheduledAt
	}
	msg := ""
	// New images first, so they survive any op.
	for i := range parts {
		files, err := readUpload(r, fmt.Sprintf("add_%d", i))
		if err != nil {
			s.internalError(w, err)
			return
		}
		for _, b := range files {
			if len(parts[i].Media) >= store.MaxImages {
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
			parts[i].Media = append(parts[i].Media, store.MediaRef{ID: mid, Ref: ref, Mime: mime})
		}
	}
	switch {
	case op == "addpart":
		if len(parts) < store.MaxParts {
			parts = append(parts, editPart{})
		}
	case strings.HasPrefix(op, "rmpart:"):
		if i, err := strconv.Atoi(op[len("rmpart:"):]); err == nil && i >= 0 && i < len(parts) && len(parts) > 1 {
			parts = append(parts[:i], parts[i+1:]...)
		}
	case strings.HasPrefix(op, "up:"):
		if i, err := strconv.Atoi(op[len("up:"):]); err == nil && i > 0 && i < len(parts) {
			parts[i-1], parts[i] = parts[i], parts[i-1]
		}
	case strings.HasPrefix(op, "rmimg:"):
		var i, j int
		if _, err := fmt.Sscanf(op, "rmimg:%d:%d", &i, &j); err == nil && i >= 0 && i < len(parts) && j >= 0 && j < len(parts[i].Media) {
			parts[i].Media = append(parts[i].Media[:j], parts[i].Media[j+1:]...)
		}
	case op == "save" || op == "approve":
		if perr != nil {
			s.showEdit(w, r, u, d, parts, at, http.StatusBadRequest, "Pick a date and a time.")
			return
		}
		e := store.Edit{When: at}
		for _, p := range parts {
			in := store.PartIn{Text: p.Text}
			for _, m := range p.Media {
				in.Media = append(in.Media, store.MediaIn{ID: m.ID, Alt: m.Alt})
			}
			e.Parts = append(e.Parts, in)
		}
		var next int64
		if op == "approve" {
			next = s.nextPending(ctx, back(r), id)
		}
		err := s.st.Save(ctx, u, id, formVersion(r), e, op == "approve")
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.notFound(w, r)
				return
			}
			code, m := reviewError(err)
			if code == 0 {
				s.internalError(w, err)
				return
			}
			stale := false
			if fresh, e2 := s.st.Draft(ctx, id); e2 == nil && errors.Is(err, store.ErrStale) {
				d, parts, at, stale = fresh, partsFromDraft(fresh), fresh.ScheduledAt, true
				m = "This draft changed since you opened it. This is the current version; make your edit again."
			}
			s.showEditV(w, r, u, d, parts, at, code, m, stale)
			return
		}
		flash := "saved"
		switch {
		case op == "approve":
			if fresh, e := s.st.Draft(ctx, id); e == nil {
				http.Redirect(w, r, s.approvedLanding(ctx, r, fresh, next), http.StatusSeeOther)
				return
			}
			flash = "approved"
		case d.Status == store.StatusApproved:
			flash = "pending"
		}
		http.Redirect(w, r, withFlash("/queue", flash), http.StatusSeeOther)
		return
	}
	s.showEdit(w, r, u, d, parts, at, http.StatusOK, msg)
}
