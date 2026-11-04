-- Slice 1: owner login (password + passkey), agent tokens, the draft queue,
-- media, draft history and the post log. Times are unix seconds (UTC); the
-- display zone is a setting, never hardcoded.

CREATE TABLE users (
	id              INTEGER PRIMARY KEY CHECK (id = 1), -- one owner per install
	email           TEXT NOT NULL COLLATE NOCASE UNIQUE,
	display_name    TEXT NOT NULL,
	password_hash   TEXT NOT NULL DEFAULT '',
	webauthn_handle BLOB UNIQUE,
	created_at      INTEGER NOT NULL
);

CREATE TABLE setup_links (
	id         INTEGER PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER,
	created_at INTEGER NOT NULL
);

-- stage: password = password ok, passkey owed; enroll = setup link used,
-- passkey registration owed; full = password and passkey both done.
CREATE TABLE sessions (
	id_hash      TEXT PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	stage        TEXT NOT NULL CHECK (stage IN ('password','enroll','full')),
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL,
	ua           TEXT NOT NULL DEFAULT ''
);

-- WebAuthn: only public material is stored.
CREATE TABLE webauthn_credentials (
	id               INTEGER PRIMARY KEY,
	user_id          INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	cred_id          BLOB NOT NULL UNIQUE,
	public_key       BLOB NOT NULL,
	sign_count       INTEGER NOT NULL DEFAULT 0,
	transports       TEXT NOT NULL DEFAULT '',
	aaguid           BLOB,
	attestation_type TEXT NOT NULL DEFAULT '',
	attestation_fmt  TEXT NOT NULL DEFAULT '',
	backup_eligible  INTEGER NOT NULL DEFAULT 0,
	backup_state     INTEGER NOT NULL DEFAULT 0,
	name             TEXT NOT NULL,
	created_at       INTEGER NOT NULL,
	last_used_at     INTEGER
);

-- In-flight ceremonies, single use, bound to the session that started them.
CREATE TABLE webauthn_challenges (
	id_hash      TEXT PRIMARY KEY,
	purpose      TEXT NOT NULL CHECK (purpose IN ('login','register')),
	session_hash TEXT NOT NULL,
	data_json    TEXT NOT NULL,
	expires_at   INTEGER NOT NULL
);

CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- The X accounts drafts may target. Connecting them is slice 2.
CREATE TABLE accounts (
	handle     TEXT PRIMARY KEY,
	initials   TEXT NOT NULL,
	color      TEXT NOT NULL,
	position   INTEGER NOT NULL,
	paused     INTEGER NOT NULL DEFAULT 0
);

-- Agent tokens: only the SHA-256 is stored; last4 is for recognising one.
CREATE TABLE agent_tokens (
	id           INTEGER PRIMARY KEY,
	name         TEXT NOT NULL,
	token_hash   TEXT NOT NULL UNIQUE,
	last4        TEXT NOT NULL,
	scopes       TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked_at   INTEGER
);

CREATE TABLE media (
	id         INTEGER PRIMARY KEY,
	ref        TEXT NOT NULL UNIQUE,
	token_id   INTEGER REFERENCES agent_tokens(id), -- NULL: uploaded by the owner
	mime       TEXT NOT NULL,
	size       INTEGER NOT NULL,
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE drafts (
	id              INTEGER PRIMARY KEY,
	account         TEXT NOT NULL REFERENCES accounts(handle),
	status          TEXT NOT NULL CHECK (status IN ('pending','approved','scheduled','posting','posted','failed','rejected')),
	proposed_at     INTEGER NOT NULL,
	scheduled_at    INTEGER,
	note            TEXT NOT NULL DEFAULT '',
	reject_reason   TEXT NOT NULL DEFAULT '',
	error           TEXT NOT NULL DEFAULT '',
	token_id        INTEGER REFERENCES agent_tokens(id),
	source          TEXT NOT NULL, -- agent token name at intake, or "you"
	idempotency_key TEXT NOT NULL DEFAULT '',
	version         INTEGER NOT NULL DEFAULT 1,
	created_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL,
	approved_at     INTEGER
);
CREATE INDEX drafts_status ON drafts(status, proposed_at);
CREATE UNIQUE INDEX drafts_idem ON drafts(token_id, idempotency_key) WHERE idempotency_key != '';

CREATE TABLE draft_parts (
	id       INTEGER PRIMARY KEY,
	draft_id INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	text     TEXT NOT NULL,
	UNIQUE (draft_id, position)
);

CREATE TABLE part_media (
	part_id  INTEGER NOT NULL REFERENCES draft_parts(id) ON DELETE CASCADE,
	position INTEGER NOT NULL,
	media_id INTEGER NOT NULL REFERENCES media(id),
	alt      TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (part_id, position)
);

-- History shown on the edit page. actor: "agent:<name>", "you" or "scheduler".
CREATE TABLE draft_events (
	id       INTEGER PRIMARY KEY,
	draft_id INTEGER NOT NULL REFERENCES drafts(id) ON DELETE CASCADE,
	at       INTEGER NOT NULL,
	actor    TEXT NOT NULL,
	kind     TEXT NOT NULL,
	detail   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX draft_events_draft ON draft_events(draft_id, at);

-- What the poster did. idem_key makes a part post at most once. Rows outlive
-- their draft (draft_id is not a foreign key) so spend history stays.
CREATE TABLE post_log (
	id          INTEGER PRIMARY KEY,
	draft_id    INTEGER NOT NULL,
	account     TEXT NOT NULL,
	part        INTEGER NOT NULL,
	parts       INTEGER NOT NULL,
	at          INTEGER NOT NULL,
	status      TEXT NOT NULL CHECK (status IN ('posted','failed')),
	post_id     TEXT NOT NULL DEFAULT '',
	url         TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	has_url     INTEGER NOT NULL DEFAULT 0,
	cost_micros INTEGER NOT NULL DEFAULT 0,
	preview     TEXT NOT NULL DEFAULT '',
	example     INTEGER NOT NULL DEFAULT 0,
	idem_key    TEXT NOT NULL UNIQUE
);
CREATE INDEX post_log_at ON post_log(at);
