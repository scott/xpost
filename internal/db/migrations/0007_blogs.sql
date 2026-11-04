-- Blogs, slice 1: blog posts go through the same drafts queue as X posts.

-- A draft is an X post or thread ('x') or a blog post ('blog'). Every
-- existing draft is an X draft.
ALTER TABLE drafts ADD COLUMN kind TEXT NOT NULL DEFAULT 'x' CHECK (kind IN ('x','blog'));
CREATE INDEX drafts_kind ON drafts(kind, status, scheduled_at);

-- Destinations stay in accounts, so drafts.account keeps its foreign key
-- and the pause switch, colours and filters keep working. A blog's row has
-- kind 'blog' and handle 'blog:<n>'; X screens list kind 'x' only.
ALTER TABLE accounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'x' CHECK (kind IN ('x','blog'));

-- A blog xpost publishes to, set up in Settings > Blogs. method 'api' is a
-- publisher REST API (api_base plus a sealed token); 'webhook' is a signed
-- webhook to an agent (sealed URL, sender key and optional Authorization
-- header, like the rewrite webhook). Secrets are AES-GCM sealed with the
-- master key, each bound to its blog and field (AAD "blogs.<field>:<id>"),
-- and never shown again. test_state is the last dry-run Test: a blog
-- publishes only while it is 'passed'; any change to the method or a
-- connection field sets it back to 'untested'. publish_clock is the default
-- time of day (HH:MM) in the owner's display zone. Removing a blog keeps
-- the row (removed_at) so published history stays, and drops its secrets.
CREATE TABLE blogs (
	id                 INTEGER PRIMARY KEY,
	account            TEXT NOT NULL UNIQUE REFERENCES accounts(handle),
	name               TEXT NOT NULL,
	site_url           TEXT NOT NULL,
	method             TEXT NOT NULL CHECK (method IN ('api','webhook')),
	api_base           TEXT NOT NULL DEFAULT '',
	api_token_sealed   BLOB,
	hook_url_sealed    BLOB,
	hook_key_sealed    BLOB,
	hook_auth_sealed   BLOB,
	publish_clock      TEXT NOT NULL DEFAULT '07:00',
	promo_account      TEXT REFERENCES accounts(handle),
	banned_topics      TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
	lint_en            TEXT NOT NULL DEFAULT 'warn' CHECK (lint_en IN ('warn','block')),
	lint_banned        TEXT NOT NULL DEFAULT 'block' CHECK (lint_banned IN ('warn','block')),
	lint_h1            TEXT NOT NULL DEFAULT 'warn' CHECK (lint_h1 IN ('warn','block')),
	cover_alt_required INTEGER NOT NULL DEFAULT 0,
	test_state         TEXT NOT NULL DEFAULT 'untested' CHECK (test_state IN ('untested','passed','failed')),
	tested_at          INTEGER,
	test_detail        TEXT NOT NULL DEFAULT '', -- short outcome, never a response body or a secret
	test_ms            INTEGER NOT NULL DEFAULT 0,
	removed_at         INTEGER,
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL
);
CREATE UNIQUE INDEX blogs_name ON blogs(name COLLATE NOCASE) WHERE removed_at IS NULL;

-- The blog half of a 'blog' draft. The publish time is the draft's
-- scheduled_at (proposed_at at intake), so the queue, Reschedule and the
-- dispatcher treat both kinds alike. staged_at is set once the post is
-- staged on the blog (on approve); the slug is locked from then on.
-- published_url and ref (the blog's id for the post, e.g. a PR or a remote
-- slug) are what the publisher reported.
CREATE TABLE blog_posts (
	draft_id        INTEGER PRIMARY KEY REFERENCES drafts(id) ON DELETE CASCADE,
	blog_id         INTEGER NOT NULL REFERENCES blogs(id),
	slug            TEXT NOT NULL,
	title           TEXT NOT NULL,
	description     TEXT NOT NULL,
	tags            TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
	markdown        TEXT NOT NULL,
	cover_media_id  INTEGER REFERENCES media(id),
	cover_alt       TEXT NOT NULL DEFAULT '',
	staged_at       INTEGER,
	published_url   TEXT NOT NULL DEFAULT '',
	ref             TEXT NOT NULL DEFAULT '',
	live_checked_at INTEGER
);
-- A slug is used once per blog, whatever the draft's state. Deleting a
-- draft frees its slug.
CREATE UNIQUE INDEX blog_posts_slug ON blog_posts(blog_id, slug);
