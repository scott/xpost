# Blogs (optional)

xpost can queue blog posts for review next to X posts, publish them, and
promote them on X. The whole feature is optional and off by default. Turn it
on with `XPOST_BLOGS_ENABLED=1`, then add blogs under **Settings > Blogs**.
Nothing reaches a blog host or agent unless `XPOST_BLOG_LIVE=1` too.

| Key | Effect |
| --- | --- |
| `XPOST_BLOGS_ENABLED` | `1` turns on blog drafts, the Blog queue kind, Settings > Blogs and its import, and the blog publisher. Unset or `0` (the default): blog pages 404; blogs are hidden from the queue, counts, schedule, sidebar and Settings; the `blogs:publish` scope is hidden; an API `kind: "blog"` proposal gets 400; no publisher or import is wired. Stored blog rows are kept. Other values refuse to start |
| `XPOST_BLOG_LIVE` | `1` turns on live publishing and the live Settings Test. Unset or `0`: Test says "Live publishing is disabled on this server" and makes no call. Other values refuse to start |
| `XPOST_BLOG_IMPORT` | `1` turns on the read-only import, separately. Unset or empty: off. Other values, including `0`, refuse to start |

`XPOST_BLOG_LIVE=1` or `XPOST_BLOG_IMPORT=1` without `XPOST_BLOGS_ENABLED=1`
refuses to start.

Approved posts publish when the scheduler runs (`XPOST_SCHEDULER=on`, or
`-fake-poster`). With `-fake-poster` and live publishing off, a fake
publisher marks posts published and sends nothing.

Agents propose posts through the [agent API](api.md#blog-drafts).

## Publisher API blogs

The blog host exposes a publisher API. The API base URL is the site root
(https only, no path); xpost adds `/api/blog/posts` and uses a sealed token.

- **Test** lists published posts. One read, never writes.
- **At publish time** xpost checks the post locally (slug, title,
  description, body up to 256 KiB, a PNG, JPEG, WebP or GIF cover up to 2
  MiB), then reads the slug. If someone else's post is there, it refuses and
  the card shows Blocked. Otherwise it stages a hidden draft dated the UTC
  day of the publish, publishes it, and confirms it is live: the API shows it
  published and the public page answers 200 with no token. Only then is it
  marked Published.
- At most 4 API calls per post, no polling. A 429 is not retried; the post
  fails with a "rate limited" note.
- Tags and cover alt text stay in xpost.
- Every call goes through a guarded client: checked dialer, no proxy, no
  redirects. Errors name only the HTTP status and the host's short error
  code, never a URL, token or response text.

Local try: `go run ./cmd/xpost-fakegander -token-file FILE` serves a fake
publisher API over TLS on `127.0.0.1:8743` (token of 32 or more bytes). It
writes a throwaway certificate (`-cert-out`, default `fakegander.pem`). Set
`SSL_CERT_FILE` to it for the demo `serve` process only. It also offers
`POST /_fake/fault`, `POST /_fake/clear`, `POST /_fake/seed` and
`GET /_fake/calls`.

## Webhook blogs

Your agent publishes the post, for example by opening a pull request on the
site's repo.

- Settings needs the webhook URL and signing key (Authorization header
  optional), a **Publishing token** (an agent token with only
  `blogs:publish`, made on the Log page; it sees only its own blog's
  publishes), and optionally a time zone.
- **Test** sends a signed `blog.test` wake. Nothing is published.
- **At publish time** xpost sends a thin signed `blog.publish` wake. The
  agent pulls the post, the cover, and reports `opened`, `merged` or
  `failed`. See [webhooks.md](webhooks.md#blog-publish-wakes) and
  [api.md](api.md#blog-publishes).
- After a merge, xpost checks the exact live URL (trailing slash, no
  redirects, no credentials) at +1, +3, +10 and +30 minutes for the content
  marker. Without a merge it checks at +10, +30 and +60 minutes, then fails.
- A Retry on the same day in the blog's zone reuses the same publish, key,
  hash and dates with the next attempt number. A Retry on a later day starts
  a new publish with new dates, key and hash.
- Images inside the markdown are not published; lint warns. Use the cover.

## Importing existing posts

**Settings > Blogs > Import existing posts** adds posts already live on a
blog as Published, so their slugs are known. Owner only, read-only.

- A dry run comes first and lists every post with what would happen and why
  (import, adopt or skip). Import does exactly that plan. If the site or
  xpost changed in between, nothing is imported and a new dry run is shown.
- Publisher API blogs: one authenticated `GET` of the published posts.
  Webhook blogs: one `GET` of the site's `/blog/posts.json`, then one `GET`
  of each listed post's page (no credentials, no redirects). A post counts as
  live only when its page answers 200 with HTML. All calls use an
  import-only client that stays on the blog's own site. xpost never writes
  to the site.
- Imported posts keep slug, title, description, day and live URL, not the
  text. They never publish, wake an agent, start a promo or post to X.
- A post already in xpost is never changed. Differences are shown in the dry
  run. A rerun creates nothing new.
- A draft blocked because its slug was taken is adopted (becomes Published)
  only when the live post has the same title, the draft has no promos, and
  the post is live.
