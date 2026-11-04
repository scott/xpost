package auth

import (
	"sync"
	"time"
)

// Limiter counts failures per key in a sliding window. xpost keys sign-in
// failures by client IP and by email, so neither spraying one account from
// many IPs nor many accounts from one IP gets unlimited guesses.
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	fails  map[string][]time.Time
}

// NewLimiter allows max failures per key per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// SetClock replaces the clock (tests).
func (l *Limiter) SetClock(now func() time.Time) { l.now = now }

func (l *Limiter) prune(key string, now time.Time) []time.Time {
	ts := l.fails[key]
	cut := now.Add(-l.window)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

// Blocked reports whether key has used up its failures, and when the oldest
// failure in the window expires (the Retry-After moment).
func (l *Limiter) Blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ts := l.prune(key, now)
	if len(ts) < l.max {
		return false, 0
	}
	return true, ts[0].Add(l.window).Sub(now)
}

// Fail records one failure for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(key, now)
	l.fails[key] = append(l.fails[key], now)
	if len(l.fails) > 100000 { // bound memory under a flood
		for k := range l.fails {
			l.prune(k, now)
		}
	}
}

// Reset clears key (after a successful sign-in).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
