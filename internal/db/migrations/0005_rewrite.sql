-- Rewrite: the owner sends a pending agent draft back to the agent that
-- wrote it, through that agent's rewrite webhook.

-- One rewrite webhook per agent token. The sender key is AES-GCM sealed
-- with the master key (like X tokens); only its last 4 characters are kept
-- in the clear for display.
CREATE TABLE agent_webhooks (
	token_id    INTEGER PRIMARY KEY REFERENCES agent_tokens(id) ON DELETE CASCADE,
	url         TEXT NOT NULL,
	key_sealed  BLOB NOT NULL,
	key_last4   TEXT NOT NULL,
	tested_at   INTEGER,
	test_ok     INTEGER NOT NULL DEFAULT 0,
	test_detail TEXT NOT NULL DEFAULT '', -- e.g. "HTTP 500" or "timeout"; never a response body
	test_ms     INTEGER NOT NULL DEFAULT 0,
	updated_at  INTEGER NOT NULL
);

-- A rewrite request. The draft stays 'pending' while it is open; an open
-- request blocks approve and edit. "Rewriting" is state 'open' before
-- expires_at; after expires_at an 'open' row is the "No rewrite yet" state
-- (no background job is needed). 'failed' means the webhook did not accept
-- it. dismissed = 1 once the owner edits the draft (the card stops showing
-- the request's outcome).
CREATE TABLE rewrite_requests (
	id              INTEGER PRIMARY KEY,
	draft_id        INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
	token_id        INTEGER NOT NULL REFERENCES agent_tokens(id),
	note            TEXT NOT NULL DEFAULT '',
	sent_at         INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL,
	state           TEXT NOT NULL CHECK (state IN ('open','revised','unchanged','cancelled','failed')),
	answered_at     INTEGER,
	revised_version INTEGER,
	dismissed       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX rewrite_requests_draft ON rewrite_requests(draft_id, id);

-- Every version a rewrite replaced, oldest first by id: the full history.
-- parts_json is [{"text": "...", "images": n}].
CREATE TABLE draft_revisions (
	id          INTEGER PRIMARY KEY,
	draft_id    INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
	request_id  INTEGER REFERENCES rewrite_requests(id) ON DELETE SET NULL,
	version     INTEGER NOT NULL, -- the draft version these parts were
	parts_json  TEXT NOT NULL,
	author      TEXT NOT NULL,    -- who wrote that version (agent name or "you")
	authored_at INTEGER NOT NULL, -- when that version was written
	created_at  INTEGER NOT NULL  -- when it was replaced
);
CREATE INDEX draft_revisions_draft ON draft_revisions(draft_id, id);
