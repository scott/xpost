-- Blogs, slice 1b: promo X posts that link to a blog post.

-- promotes is the blog draft an X draft promotes; its text may carry
-- {{blog_url}}, replaced by the blog post's live URL when it posts. A promo
-- never posts before its blog post is published. promo_offset is minutes
-- after the blog's publish time (the promo moves with the blog); NULL is a
-- fixed time. promo_returned marks a promo sent back to pending because its
-- blog post was rejected or not live 24h after the promo was due.
ALTER TABLE drafts ADD COLUMN promotes INTEGER REFERENCES drafts(id);
ALTER TABLE drafts ADD COLUMN promo_offset INTEGER;
ALTER TABLE drafts ADD COLUMN promo_returned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX drafts_promotes ON drafts(promotes) WHERE promotes IS NOT NULL;
