-- Blogs slice 3: the webhook-to-agent publisher (issue #40).
--
-- zone: the blog's IANA time zone for its default publish time and the
-- post's local date ('' = xpost's own zone).
-- publish_token_id: the agent token that may pull and report this blog's
-- publishes (scope blogs:publish). NULL: none picked, nothing publishes.
ALTER TABLE blogs ADD COLUMN zone TEXT NOT NULL DEFAULT '';
ALTER TABLE blogs ADD COLUMN publish_token_id INTEGER REFERENCES agent_tokens(id);

-- One publish of a blog post through its agent: the payload frozen when
-- publishing started (canonical rendered content, never the cover bytes)
-- and its hash. A Retry of unchanged content reuses the row (attempt + 1,
-- same idempotency key); an edit starts a new row. state:
--   waking  the wake is being sent
--   sent    the agent answered 2xx; waiting for its report or the page
--   opened  the agent reported its pull request
--   merged  the agent reported the merge; live checks run
--   live    xpost found the marker on the live page
--   failed  this attempt ended without a confirmed live page
-- code is the agent's whitelisted failure code; pr, head_sha and
-- merge_sha are what it reported (numbers and hex only, never a URL).
CREATE TABLE blog_publishes (
	id             INTEGER PRIMARY KEY,
	draft_id       INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
	blog_id        INTEGER NOT NULL REFERENCES blogs(id),
	version        INTEGER NOT NULL,
	attempt        INTEGER NOT NULL DEFAULT 1,
	content_key    TEXT NOT NULL,
	content_sha256 TEXT NOT NULL,
	payload        TEXT NOT NULL,
	cover_media_id INTEGER REFERENCES media(id),
	live_url       TEXT NOT NULL,
	state          TEXT NOT NULL CHECK (state IN ('waking','sent','opened','merged','live','failed')),
	delivery_id    TEXT NOT NULL DEFAULT '',
	started_at     INTEGER NOT NULL,
	sent_at        INTEGER,
	expires_at     INTEGER NOT NULL,
	merged_at      INTEGER,
	checks         INTEGER NOT NULL DEFAULT 0,
	pr             INTEGER NOT NULL DEFAULT 0,
	head_sha       TEXT NOT NULL DEFAULT '',
	merge_sha      TEXT NOT NULL DEFAULT '',
	code           TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL
);
CREATE INDEX blog_publishes_draft ON blog_publishes(draft_id, id);

-- Open tabs hear about publish progress (PR opened, merged) like any
-- other blog change.
CREATE TRIGGER live_blog_publish_ins AFTER INSERT ON blog_publishes BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('blog', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
CREATE TRIGGER live_blog_publish_upd AFTER UPDATE ON blog_publishes BEGIN
	INSERT INTO live_changes(kind, ref, v) VALUES ('blog', NEW.draft_id, COALESCE((SELECT version FROM drafts WHERE id = NEW.draft_id), 0));
END;
