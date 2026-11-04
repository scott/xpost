package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/scott/xpost/internal/auth"
	"github.com/scott/xpost/internal/store"
)

// Live UI updates (issue #36). Open owner tabs subscribe to
// GET /live/events (Server-Sent Events) or, when that is unavailable, poll
// GET /live/poll. Both are owner-session only (an agent token is never a
// session) and both carry only store.Change values: a draft id, a kind and
// a version. The tab then refetches its own page over the normal owner
// routes and swaps the cards that changed (static/live.js).
//
// The feed is the live_changes table that triggers fill, so it sees every
// writer: the agent API, the owner, the poster, the sweeper. One hub
// goroutine tails it while anyone is subscribed.

const (
	liveHeader      = "X-Xpost-Live" // set by live.js on its refetches
	liveReplayLimit = 1000           // more missed changes than this: resync
)

// isLive reports a request from the live machinery (the stream, the poll,
// or live.js refetching the page). Those never count as owner activity for
// the session idle timeout.
func isLive(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.URL.Path == "/live/events" || r.URL.Path == "/live/poll" || r.Header.Get(liveHeader) == "1"
}

var (
	errLiveFull   = errors.New("live: too many connections")
	errLiveClosed = errors.New("live: shutting down")
)

type liveSub struct {
	ch   chan store.Change
	lag  chan struct{} // closed when the hub drops a slow subscriber
	sess string
}

type liveHub struct {
	st       *store.Store
	log      *log.Logger
	poll     time.Duration
	maxPer   int
	maxTotal int

	mu      sync.Mutex
	subs    map[*liveSub]struct{}
	perSess map[string]int
	running bool
	closed  bool
	last    int64 // newest seq broadcast
	pruned  time.Time
	done    chan struct{}
	stopped chan struct{} // closed when the tail loop exits (nil if never ran)
	once    sync.Once
}

func newLiveHub(st *store.Store, lg *log.Logger, cfg Config) *liveHub {
	h := &liveHub{st: st, log: lg, poll: cfg.LivePoll, maxPer: cfg.LiveMaxPerSession, maxTotal: cfg.LiveMaxTotal,
		subs: map[*liveSub]struct{}{}, perSess: map[string]int{}, done: make(chan struct{})}
	if h.poll <= 0 {
		h.poll = time.Second
	}
	if h.maxPer <= 0 {
		h.maxPer = 4
	}
	if h.maxTotal <= 0 {
		h.maxTotal = 64
	}
	return h
}

// subscribe registers a tab. The tail loop starts with the newest seq read
// before the caller replays, so a change is either in the replay or
// broadcast (the handler drops repeats by seq).
func (h *liveHub) subscribe(ctx context.Context, sess string) (*liveSub, error) {
	h.mu.Lock()
	needStart := !h.running && !h.closed
	h.mu.Unlock()
	var newest int64
	if needStart {
		var err error
		if newest, _, err = h.st.LiveSeq(ctx); err != nil {
			return nil, err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, errLiveClosed
	}
	if len(h.subs) >= h.maxTotal || h.perSess[sess] >= h.maxPer {
		return nil, errLiveFull
	}
	if !h.running {
		if !needStart { // lost a race with a loop that just stopped
			h.mu.Unlock()
			n, _, err := h.st.LiveSeq(ctx)
			h.mu.Lock()
			if err != nil {
				return nil, err
			}
			newest = n
		}
		if !h.running && !h.closed {
			h.running, h.last = true, newest
			h.stopped = make(chan struct{})
			go h.loop(h.stopped)
		}
	}
	sub := &liveSub{ch: make(chan store.Change, 256), lag: make(chan struct{}), sess: sess}
	h.subs[sub] = struct{}{}
	h.perSess[sess]++
	return sub, nil
}

func (h *liveHub) unsubscribe(sub *liveSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(sub)
}

func (h *liveHub) dropLocked(sub *liveSub) {
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	if h.perSess[sub.sess]--; h.perSess[sub.sess] <= 0 {
		delete(h.perSess, sub.sess)
	}
}

// count is the number of open streams (tests).
func (h *liveHub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Close ends every stream and stops the tail loop. It never blocks on a
// client: handlers see done and return, so the server's graceful stop is
// not held up by open tabs.
func (h *liveHub) Close() {
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		stopped := h.stopped
		h.mu.Unlock()
		close(h.done)
		if stopped != nil {
			<-stopped
		}
	})
}

func (h *liveHub) loop(stopped chan struct{}) {
	defer close(stopped)
	t := time.NewTicker(h.poll)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-t.C:
		}
		if !h.tick() {
			return
		}
	}
}

// tick broadcasts new changes; false when nobody is subscribed any more
// (the loop then stops until the next subscriber).
func (h *liveHub) tick() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.mu.Lock()
	if len(h.subs) == 0 {
		h.running = false
		h.mu.Unlock()
		return false
	}
	last := h.last
	h.mu.Unlock()

	now := h.st.Now()
	// The sweep notes timeouts and prunes too; both are idempotent, so this
	// only makes an open tab hear about a timeout within a second.
	if _, err := h.st.MarkRewriteTimeouts(ctx, now); err != nil {
		h.log.Printf("live: rewrite timeouts: %v", err)
	}
	var batch []store.Change
	for {
		cs, err := h.st.ChangesSince(ctx, last, 500)
		if err != nil {
			h.log.Printf("live: read changes: %v", err)
			break
		}
		batch = append(batch, cs...)
		if len(cs) < 500 {
			break
		}
		last = cs[len(cs)-1].Seq
	}
	if time.Since(h.pruned) > time.Hour {
		if _, err := h.st.PruneChanges(ctx); err != nil {
			h.log.Printf("live: prune: %v", err)
		}
		h.pruned = time.Now()
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range batch {
		if c.Seq <= h.last {
			continue
		}
		h.last = c.Seq
		for sub := range h.subs {
			select {
			case sub.ch <- c:
			default: // too slow: drop it; the tab reconnects and resumes by seq
				h.dropLocked(sub)
				close(sub.lag)
			}
		}
	}
	return true
}

func (s *Server) liveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /live/events", s.liveEvents)
	mux.HandleFunc("GET /live/poll", s.livePoll)
}

// liveAuth is requireOwner for the live endpoints: a 401 (not a redirect to
// the sign-in page, which EventSource and fetch would follow) when there is
// no full owner session. It never moves the session's last_seen_at.
func (s *Server) liveAuth(w http.ResponseWriter, r *http.Request) (string, bool) {
	if _, _, err := s.fullSession(r); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"sign in"}`)
		return "", false
	}
	return auth.HashToken(sessionToken(r)), true
}

// liveSince reads the tab's last seen seq: Last-Event-ID (EventSource sets
// it on reconnect) or ?since=. -1 when absent.
func liveSince(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("since")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// liveSeqNow is the feed position a page was rendered at; live.js resumes
// from it. Only GET pages carry it.
func (s *Server) liveSeqNow(r *http.Request) int64 {
	n, _, err := s.st.LiveSeq(r.Context())
	if err != nil {
		return -1
	}
	return n
}

type livePollOut struct {
	Seq     int64          `json:"seq"`
	Changes []store.Change `json:"changes"`
	Resync  bool           `json:"resync,omitempty"`
}

// livePoll is the fallback: changes after ?since=, newest seq, and resync
// when the tab missed more than the feed keeps.
func (s *Server) livePoll(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.liveAuth(w, r); !ok {
		return
	}
	ctx := r.Context()
	newest, oldest, err := s.st.LiveSeq(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := livePollOut{Seq: newest, Changes: []store.Change{}}
	if since := liveSince(r); since >= 0 {
		if since > newest || (oldest > 0 && since < oldest-1) {
			out.Resync = true
		} else if cs, err := s.st.ChangesSince(ctx, since, liveReplayLimit); err != nil {
			s.internalError(w, err)
			return
		} else if len(cs) == liveReplayLimit {
			out.Resync = true
		} else {
			out.Changes = append(out.Changes, cs...)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// liveEvents is the SSE stream. Events: hello (the current seq), change
// (one store.Change, id = seq), resync (refetch everything), signedout,
// bye (server stopping). A comment line every heartbeat keeps proxies from
// timing the stream out; the session is re-checked each time.
func (s *Server) liveEvents(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.liveAuth(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	sub, err := s.live.subscribe(ctx, sess)
	switch {
	case errors.Is(err, errLiveFull):
		w.Header().Set("Retry-After", "30")
		http.Error(w, "Too many live connections for this session.", http.StatusTooManyRequests)
		return
	case errors.Is(err, errLiveClosed):
		http.Error(w, "Shutting down.", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	defer s.live.unsubscribe(sub)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("X-Accel-Buffering", "no") // nginx-style proxies: don't buffer
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Time{}) // the server's ReadTimeout would cut a long stream
	w.WriteHeader(http.StatusOK)
	send := func(format string, a ...any) bool {
		rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := fmt.Fprintf(w, format, a...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	newest, oldest, err := s.st.LiveSeq(ctx)
	if err != nil {
		return
	}
	since, sent := liveSince(r), newest
	if !send("retry: 5000\n\nevent: hello\ndata: {\"seq\":%d}\n\n", newest) {
		return
	}
	if since >= 0 {
		replay, err := s.st.ChangesSince(ctx, since, liveReplayLimit)
		if since > newest || (oldest > 0 && since < oldest-1) || len(replay) == liveReplayLimit || err != nil {
			if !send("event: resync\ndata: {\"seq\":%d}\n\n", newest) {
				return
			}
		} else {
			sent = since
			for _, c := range replay {
				if !s.sendChange(send, c) {
					return
				}
				sent = c.Seq
			}
		}
	}
	beat := time.NewTicker(s.cfg.LiveHeartbeat)
	defer beat.Stop()
	maxAge := time.NewTimer(s.cfg.LiveMaxAge)
	defer maxAge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.live.done:
			send("event: bye\ndata: {}\n\n")
			return
		case <-sub.lag:
			send("event: resync\ndata: {}\n\n")
			return
		case <-maxAge.C:
			return // EventSource reconnects with Last-Event-ID
		case <-beat.C:
			if _, _, err := s.fullSession(r); err != nil {
				send("event: signedout\ndata: {}\n\n")
				return
			}
			if !send(": hb\n\n") {
				return
			}
		case c := <-sub.ch:
			if c.Seq <= sent {
				continue
			}
			if !s.sendChange(send, c) {
				return
			}
			sent = c.Seq
		}
	}
}

func (s *Server) sendChange(send func(string, ...any) bool, c store.Change) bool {
	b, _ := json.Marshal(c)
	return send("id: %d\nevent: change\ndata: %s\n\n", c.Seq, b)
}
