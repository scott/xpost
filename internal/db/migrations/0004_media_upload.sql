-- Slice 4: images go to X's media upload before the part that uses them.

-- How many images a logged part carried (for the Log page).
ALTER TABLE post_log ADD COLUMN images INTEGER NOT NULL DEFAULT 0;

-- media_uploads is the current state of one image of one part of one
-- approved version, keyed d{draft}-v{version}-p{part}-m{slot}. A row is
-- written 'uploading' (the intent) and committed BEFORE X's upload endpoint
-- is called, then moved to:
--   uploaded  X returned a media id (x_media_id, expires_at). processing=1
--             while X still processes it (check again at check_at);
--             alt_done=1 once the alt text is on X.
--   retry     not uploaded (429 or no connection); retried automatically
--   failed    X refused it
--   unknown   the call may or may not have uploaded (timeout, dropped
--             connection, 5xx, or a crash while 'uploading')
-- An upload creates nothing visible on X, so a retry, an unknown or an
-- expired id is simply uploaded again; only the post itself is guarded
-- against repeats (post_log). Parts already posted are never touched.
CREATE TABLE media_uploads (
	id          INTEGER PRIMARY KEY,
	draft_id    INTEGER NOT NULL,
	account     TEXT NOT NULL,
	part        INTEGER NOT NULL,
	slot        INTEGER NOT NULL,
	media_id    INTEGER NOT NULL, -- the local media row
	idem_key    TEXT NOT NULL UNIQUE,
	status      TEXT NOT NULL CHECK (status IN ('uploading','uploaded','retry','failed','unknown')),
	x_media_id  TEXT NOT NULL DEFAULT '',
	expires_at  INTEGER NOT NULL DEFAULT 0,
	processing  INTEGER NOT NULL DEFAULT 0,
	check_at    INTEGER NOT NULL DEFAULT 0,
	alt_done    INTEGER NOT NULL DEFAULT 0,
	attempts    INTEGER NOT NULL DEFAULT 0,
	error       TEXT NOT NULL DEFAULT '',
	at          INTEGER NOT NULL
);
CREATE INDEX media_uploads_draft ON media_uploads(draft_id, part, slot);

-- media_calls is the spend ledger: one row per billable media call, written
-- with the intent before the call. kind 'upload' is an upload (its status
-- checks ride on it: XPOST_COST_MEDIA_UPLOAD_USD); kind 'metadata' is one
-- alt text call (XPOST_COST_MEDIA_METADATA_USD). status: 'uploading' while
-- the call is in flight, 'uploaded' when X accepted it, then as for
-- media_uploads. Rows 'uploading', 'uploaded' or 'unknown' count as billed;
-- 'retry' and 'failed' are assumed not billed (unverified).
CREATE TABLE media_calls (
	id       INTEGER PRIMARY KEY,
	at       INTEGER NOT NULL,
	idem_key TEXT NOT NULL,
	kind     TEXT NOT NULL DEFAULT 'upload' CHECK (kind IN ('upload','metadata')),
	status   TEXT NOT NULL CHECK (status IN ('uploading','uploaded','retry','failed','unknown')),
	example  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX media_calls_at ON media_calls(at, kind);
