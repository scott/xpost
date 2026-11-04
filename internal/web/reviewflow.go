package web

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Review flow: after Approve the owner keeps reviewing while anything is
// pending, and lands on the approved card only when nothing is left.

// landingFilter is the pending view a /queue return URL shows: its kind
// and account filters, nothing else.
func landingFilter(ret string) (string, store.Filter) {
	u, err := url.Parse(ret)
	if err != nil || u.Path != "/queue" {
		return "/queue", store.Filter{Status: store.StatusPending}
	}
	q := u.Query()
	v := url.Values{}
	f := store.Filter{Status: store.StatusPending}
	switch k := q.Get("kind"); k {
	case store.KindX, store.KindReply, store.KindBlog:
		f.Kind = k
		v.Set("kind", k)
	}
	if a := q.Get("account"); a != "" {
		f.Account = a
		v.Set("account", a)
	}
	if b := q.Get("blog"); b != "" {
		v.Set("blog", b)
	}
	if len(v) == 0 {
		return "/queue", f
	}
	return "/queue?" + v.Encode(), f
}

// nextPending is the pending card after id in the landing view, read
// before the action (the one before it when id was last; 0 if none).
func (s *Server) nextPending(ctx context.Context, ret string, id int64) int64 {
	_, f := landingFilter(ret)
	list, err := s.st.Queue(ctx, f)
	if err != nil {
		return 0
	}
	for i, d := range list {
		if d.ID != id {
			continue
		}
		if i+1 < len(list) {
			return list[i+1].ID
		}
		if i > 0 {
			return list[i-1].ID
		}
		return 0
	}
	if len(list) > 0 {
		return list[0].ID
	}
	return 0
}

// approvedLanding is where Approve goes for d. Pending left: the pending
// view it came from (all of pending when that filter is now empty), at
// the next card. Nothing left: the approved card.
func (s *Server) approvedLanding(ctx context.Context, r *http.Request, d *store.Draft, next int64) string {
	all, _ := s.st.QueueCounts(ctx, "", "")
	id := strconv.FormatInt(d.ID, 10)
	if all.ByStatus[store.StatusPending] == 0 {
		if d.IsReply() {
			return replyApproved(d.ID)
		}
		return "/queue?status=approved&done=lastapproved#d" + id
	}
	to, f := landingFilter(back(r))
	if to != "/queue" {
		if c, _ := s.st.QueueCounts(ctx, f.Account, f.Kind); c.ByStatus[store.StatusPending] == 0 {
			to, next = "/queue", 0
		}
	}
	if d.IsReply() {
		to = withFlash(to, "replyleft") + "&reply=" + id
	} else {
		to = withFlash(to, "approved")
	}
	if next != 0 {
		to += "#d" + strconv.FormatInt(next, 10)
	}
	return to
}

// replyFlash is the pending queue's note after approving a reply with
// others still pending: Open on X right there, and a link to its card.
type replyFlash struct {
	ID       int64
	V        int
	Left     int
	ViewHref string
	Intent   string // the x.com intent URL, for xpost.js to open
	Opened   string // set once opened: "2:31 PM MST" in the owner's zone
	MarkHref string // the approved card with Mark posted open
}

func (s *Server) replyFlashFor(ctx context.Context, q url.Values, left int, loc *time.Location) *replyFlash {
	if q.Get("done") != "replyleft" {
		return nil
	}
	id, err := strconv.ParseInt(q.Get("reply"), 10, 64)
	if err != nil {
		return nil
	}
	d, err := s.st.Draft(ctx, id)
	if err != nil || !d.IsReply() || d.Status != store.StatusApproved {
		return nil
	}
	sid := strconv.FormatInt(d.ID, 10)
	f := &replyFlash{ID: d.ID, V: d.Version, Left: left, ViewHref: "/queue?status=approved&kind=reply#d" + sid,
		MarkHref: "/queue?status=approved&kind=reply&mark=" + sid + "#d" + sid}
	if len(d.Parts) > 0 {
		f.Intent = s.webCfg().IntentURL(d.Parts[0].Text, d.Reply.InReplyTo)
	}
	if d.Reply.OpenedAt != nil {
		f.Opened = clock(*d.Reply.OpenedAt, loc)
	}
	return f
}

// approvedToday is true for an approved draft approved, or due to post,
// today in the owner's zone.
func approvedToday(d *store.Draft, start, end time.Time) bool {
	in := func(t time.Time) bool { return !t.Before(start) && t.Before(end) }
	if d.ApprovedAt != nil && in(*d.ApprovedAt) {
		return true
	}
	return !d.IsReply() && in(d.ScheduledAt)
}

// todayBounds is today in loc.
func todayBounds(now time.Time, loc *time.Location) (time.Time, time.Time) {
	l := now.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

// todayFirst moves today's approved drafts to the front, keeping order.
func todayFirst(list []*store.Draft, start, end time.Time) {
	sort.SliceStable(list, func(i, j int) bool {
		return approvedToday(list[i], start, end) && !approvedToday(list[j], start, end)
	})
}
