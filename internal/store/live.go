package store

import (
	"context"
	"time"
)

// Live change feed (migration 0009): triggers append a row for every change
// to a draft, its rewrite requests, post log rows or blog post. A Change
// carries ids, a kind and a version only, never text or secrets.
type Change struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"` // draft, rewrite, log or blog
	ID   int64  `json:"id"`   // the draft id
	V    int64  `json:"v"`    // the draft version then (0: deleted)
}

// LiveKeep is how many change rows PruneChanges keeps.
const LiveKeep = 10000

// LiveSeq returns the newest and the oldest seq in the feed (0, 0 when it
// is empty). A tab whose last seq is below oldest-1 missed rows and must
// refetch everything.
func (s *Store) LiveSeq(ctx context.Context) (newest, oldest int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0), COALESCE(MIN(seq), 0) FROM live_changes`).Scan(&newest, &oldest)
	if err == nil && newest == 0 {
		// Empty after pruning everything: the next seq continues from
		// sqlite_sequence, so report that as newest.
		s.DB.QueryRowContext(ctx, `SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'live_changes'), 0)`).Scan(&newest)
	}
	return
}

// ChangesSince returns up to limit changes after seq, oldest first.
func (s *Store) ChangesSince(ctx context.Context, seq int64, limit int) ([]Change, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, kind, ref, v FROM live_changes WHERE seq > ? ORDER BY seq LIMIT ?`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		if err := rows.Scan(&c.Seq, &c.Kind, &c.ID, &c.V); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkRewriteTimeouts notes every open request whose 60-minute window has
// ended by now and was not noted yet. Running out of time writes nothing
// else (the "No rewrite yet" state is derived), so without this an open tab
// would not hear about it. Setting timeout_noted fires live_rewrite_upd, so
// each timeout gets exactly one rewrite change row however many callers
// (sweep, live hub, several processes) run it, in any order. It returns how
// many it noted.
func (s *Store) MarkRewriteTimeouts(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE rewrite_requests SET timeout_noted = 1
		WHERE state = 'open' AND timeout_noted = 0 AND expires_at <= ?`, now.Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneChanges keeps the newest LiveKeep rows and returns how many it
// deleted. Safe to run from several places at once.
func (s *Store) PruneChanges(ctx context.Context) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM live_changes WHERE seq <= (SELECT MAX(seq) FROM live_changes) - ?`, LiveKeep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
