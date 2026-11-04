-- Blogs, slice 5: posts imported from a blog's live site. imported_at is
-- when the owner imported it (NULL: xpost published it, or an agent or the
-- owner wrote it here). An imported post keeps no body: the text stays on
-- the site, and xpost never publishes, promotes or rewrites it.
-- adopted_at is when the import adopted a blocked draft of xpost's as the
-- live post on its slug (the draft keeps its own text).
ALTER TABLE blog_posts ADD COLUMN imported_at INTEGER;
ALTER TABLE blog_posts ADD COLUMN adopted_at INTEGER;
