-- Live UI updates: a change feed for open browser tabs (issue #36).
--
-- Triggers append one row per change to a draft, its rewrite requests, its
-- post log rows or its blog post, whoever made it (agent API, owner, poster,
-- sweeper, another process). The web server tails this table and tells open
-- tabs which draft changed; the tab then refetches its page over the normal
-- owner-only routes. A row carries only ids, a kind and the draft version:
-- never post text, tokens or anything secret. seq never repeats
-- (AUTOINCREMENT), so a tab can resume from the last seq it saw. Old rows
-- are pruned by count.
--   kind  draft    the draft row was created, changed or deleted (v = 0)
--         rewrite  a rewrite request was sent, answered, cancelled or ran
--                  out of time
--         log      the poster recorded an attempt
--         blog     the blog post fields changed (publish, live check)
--   ref   the draft id
--   v     the draft's version when the row was written (0 if it is gone)
CREATE TABLE live_changes (
	seq  INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL CHECK (kind IN ('draft','rewrite','log','blog')),
	ref  INTEGER NOT NULL,
	v    INTEGER NOT NULL DEFAULT 0
);

-- timeout_noted: set once when an open request's window has ended, by
-- store.MarkRewriteTimeouts (from the always-on sweep and the live hub).
-- The UPDATE fires live_rewrite_upd, so a timeout is announced exactly once
-- whoever notices it first. Requests already past their window when this
-- migration runs count as noted.
ALTER TABLE rewrite_requests ADD COLUMN timeout_noted INTEGER NOT NULL DEFAULT 0;
UPDATE rewrite_requests SET timeout_noted = 1 WHERE state = 'open' AND expires_at <= CAST(strftime('%s', 'now') AS INTEGER);

CREATE TRIGGER live_draft_ins AFTER INSERT ON drafts BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('draft', NEW.id, NEW.version);
END;
CREATE TRIGGER live_draft_upd AFTER UPDATE ON drafts BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('draft', NEW.id, NEW.version);
END;
CREATE TRIGGER live_draft_del AFTER DELETE ON drafts BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('draft', OLD.id, 0);
END;
CREATE TRIGGER live_rewrite_ins AFTER INSERT ON rewrite_requests BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('rewrite', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
CREATE TRIGGER live_rewrite_upd AFTER UPDATE ON rewrite_requests BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('rewrite', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
CREATE TRIGGER live_log_ins AFTER INSERT ON post_log BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('log', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
CREATE TRIGGER live_log_upd AFTER UPDATE ON post_log BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('log', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
CREATE TRIGGER live_blog_upd AFTER UPDATE ON blog_posts BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('blog', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
