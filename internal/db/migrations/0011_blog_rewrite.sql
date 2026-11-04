-- Rewrite for blog posts: a replaced blog version is kept as JSON
-- {title, description, tags, markdown}; X versions keep using parts_json.
ALTER TABLE draft_revisions ADD COLUMN post_json TEXT NOT NULL DEFAULT '';
