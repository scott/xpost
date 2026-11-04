// Package sweep runs housekeeping that must happen whether or not posting
// is on. It never calls X or any blog host.
package sweep

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/scott/xpost/internal/store"
)

// Every is how often Promos sweeps by default.
const Every = time.Minute

// Promos sends approved promos whose blog post is still not live 24h past
// the promo's time back to pending (store.ExpirePromos), once at start and
// then on every tick until ctx ends. The same tick keeps the live change
// feed in shape with no tab or stream open: it notes rewrite requests that
// ran out of time (store.MarkRewriteTimeouts, one change row each, however
// many callers) and keeps the newest store.LiveKeep change rows
// (store.PruneChanges). The returned channel closes when it has stopped.
func Promos(ctx context.Context, st *store.Store, every time.Duration, lg *log.Logger) <-chan struct{} {
	done := make(chan struct{})
	lg.Printf("promo sweep every %s (also rewrite timeouts and the live feed, newest %d changes kept)", shortDur(every), store.LiveKeep)
	go func() {
		defer close(done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if n, err := st.ExpirePromos(ctx, st.Now()); err != nil && ctx.Err() == nil {
				lg.Printf("promo sweep: %v", err)
			} else if n > 0 {
				lg.Printf("promo sweep: %d promo(s) back to pending (blog post not live 24h after the promo was due)", n)
			}
			Live(ctx, st, lg)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return done
}

// Live notes rewrite timeouts and prunes the change feed once. Both are
// idempotent and safe next to the web server's live hub doing the same.
func Live(ctx context.Context, st *store.Store, lg *log.Logger) {
	if n, err := st.MarkRewriteTimeouts(ctx, st.Now()); err != nil && ctx.Err() == nil {
		lg.Printf("live sweep: rewrite timeouts: %v", err)
	} else if n > 0 {
		lg.Printf("live sweep: %d rewrite request(s) ran out of time", n)
	}
	if _, err := st.PruneChanges(ctx); err != nil && ctx.Err() == nil {
		lg.Printf("live sweep: prune: %v", err)
	}
}

// shortDur prints 1m rather than 1m0s.
func shortDur(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
