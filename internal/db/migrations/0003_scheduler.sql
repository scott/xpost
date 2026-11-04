-- Slice 3: the scheduler.

-- A draft in 'posting' is owned by the dispatcher until lease_until. A
-- crashed run's lease lapses, so the draft is picked up again and
-- reconciled. A rate-limited draft waits by setting lease_until to the
-- time X said to come back. attempts counts automatic retries.
ALTER TABLE drafts ADD COLUMN lease_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE drafts ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

-- post_log gains intent and outcome states. A row is written as 'posting'
-- (the intent) and committed BEFORE X is called, then moved to:
--   posted   X returned the post id
--   failed   X refused it; it was not posted
--   retry    not posted (429 or no connection); retried automatically
--   unknown  the call may or may not have posted (timeout, dropped
--            connection, 5xx, or a crash while 'posting'); never retried
--            automatically, the owner reviews it
-- No table references post_log, so rebuilding it to widen the CHECK is safe.
CREATE TABLE post_log_new (
	id          INTEGER PRIMARY KEY,
	draft_id    INTEGER NOT NULL,
	account     TEXT NOT NULL,
	part        INTEGER NOT NULL,
	parts       INTEGER NOT NULL,
	at          INTEGER NOT NULL,
	status      TEXT NOT NULL CHECK (status IN ('posting','posted','failed','retry','unknown')),
	post_id     TEXT NOT NULL DEFAULT '',
	url         TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	has_url     INTEGER NOT NULL DEFAULT 0,
	cost_micros INTEGER NOT NULL DEFAULT 0,
	preview     TEXT NOT NULL DEFAULT '',
	example     INTEGER NOT NULL DEFAULT 0,
	idem_key    TEXT NOT NULL UNIQUE,
	reply_to    TEXT NOT NULL DEFAULT '',
	attempts    INTEGER NOT NULL DEFAULT 0,
	retry_at    INTEGER NOT NULL DEFAULT 0
);
INSERT INTO post_log_new (id, draft_id, account, part, parts, at, status, post_id, url, error, has_url, cost_micros, preview, example, idem_key)
	SELECT id, draft_id, account, part, parts, at, status, post_id, url, error, has_url, cost_micros, preview, example, idem_key FROM post_log;
DROP TABLE post_log;
ALTER TABLE post_log_new RENAME TO post_log;
CREATE INDEX post_log_at ON post_log(at);
CREATE INDEX post_log_draft ON post_log(draft_id, part);
