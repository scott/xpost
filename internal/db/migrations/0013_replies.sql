-- Replies: an X draft (kind 'x', one part, no images) that answers
-- another post. xpost never posts a reply through the API: the owner
-- approves it, opens x.com with the text filled in, presses Post there and
-- marks it posted here. A draft with a row here is a reply; the scheduler
-- and the spend estimate skip it.
-- in_reply_to is the original post's id (digits only). The original's
-- fields are what the agent supplied; xpost never reads X for them.
-- available = 0 when the agent could not load the original (deleted, or X
-- did not return it): Approve is blocked. tried_at is when it was last
-- read (the agent's submit, or the owner's Retry load).
-- reply_settings is the original's X setting as the agent reported it
-- ('' when unknown). opened_at is when the owner last opened x.com for it;
-- posted_url is the owner's optional link, normalized ('' when none).
CREATE TABLE draft_replies (
  draft_id INTEGER PRIMARY KEY REFERENCES drafts(id) ON DELETE CASCADE,
  in_reply_to TEXT NOT NULL CHECK (length(in_reply_to) BETWEEN 1 AND 30 AND in_reply_to NOT GLOB '*[^0-9]*'),
  author_name TEXT NOT NULL DEFAULT '',
  author_handle TEXT NOT NULL DEFAULT '',
  orig_text TEXT NOT NULL DEFAULT '',
  orig_created_at INTEGER,
  orig_url TEXT NOT NULL DEFAULT '',
  reply_settings TEXT NOT NULL DEFAULT '',
  available INTEGER NOT NULL DEFAULT 1 CHECK (available IN (0, 1)),
  tried_at INTEGER NOT NULL,
  opened_at INTEGER,
  posted_url TEXT NOT NULL DEFAULT ''
);
CREATE INDEX draft_replies_to ON draft_replies(in_reply_to);
