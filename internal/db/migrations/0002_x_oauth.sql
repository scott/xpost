-- Slice 2: X OAuth 2.0 connections.

-- One pending authorization per Connect click. The state is stored hashed;
-- the PKCE verifier is sealed with the server key. Rows are single use
-- (deleted when the callback takes them) and expire after 10 minutes.
CREATE TABLE oauth_states (
	state_hash   TEXT PRIMARY KEY,
	session_hash TEXT NOT NULL,
	account      TEXT NOT NULL REFERENCES accounts(handle),
	verifier_enc BLOB NOT NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);

-- The X connection for an xpost account. Tokens are AES-GCM sealed with the
-- server key, bound to the account and token kind. generation increases on
-- every token change so concurrent refreshes cannot overwrite each other.
CREATE TABLE x_connections (
	account      TEXT PRIMARY KEY REFERENCES accounts(handle),
	x_user_id    TEXT NOT NULL,
	x_username   TEXT NOT NULL,
	access_enc   BLOB NOT NULL,
	refresh_enc  BLOB,
	expires_at   INTEGER NOT NULL,
	scope        TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL CHECK (status IN ('connected', 'reconnect')),
	last_error   TEXT NOT NULL DEFAULT '',
	connected_at INTEGER NOT NULL,
	refreshed_at INTEGER,
	generation   INTEGER NOT NULL DEFAULT 1
);
