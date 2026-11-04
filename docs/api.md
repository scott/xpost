# Agent API

Create a token under **Log & spend > Agent tokens**. It is shown once and
stored only as a SHA-256 hash. Send it as `Authorization: Bearer <token>`.

| Scope | Allows |
| --- | --- |
| `drafts:write` | create, list, read and delete your own drafts; upload media |
| `drafts:revise` | answer rewrite requests |
| `log:read` | read the post log |
| `blogs:publish` | pull and report the publishes of one bound webhook blog (only with `XPOST_BLOGS_ENABLED=1`) |

No scope can approve, edit an approved draft, or post.

## Routes

| Method | Path | Scope | Notes |
| --- | --- | --- | --- |
| POST | `/api/v1/media` | drafts:write | multipart `file`; PNG, JPEG, GIF or WebP up to 5 MB; returns `media_ref` |
| POST | `/api/v1/drafts` | drafts:write | create a draft (below); 201 new, 200 replay of the same `idempotency_key` |
| GET | `/api/v1/drafts` | drafts:write | the token's own drafts; `?status=`, `?account=` |
| GET | `/api/v1/drafts/{id}` | drafts:write | includes `reject_reason` when rejected, and `rewrite` (`request_id`, `state`, `note`, `sent_at`, `expires_at`) once requested |
| DELETE | `/api/v1/drafts/{id}` | drafts:write | own and still pending only; otherwise 409 or 404 |
| POST | `/api/v1/drafts/{id}/revise` | drafts:revise | answer a rewrite request (below) |
| GET | `/api/v1/log` | log:read | post log; `status` is posting, posted, failed, retry or unknown; `cost_estimate_usd` is empty while `rates_configured` is false |
| GET | `/api/v1/blog-publishes/{id}` | blogs:publish | the bound blog's in-flight publish only (404 otherwise, 409 `not_in_flight` or `expired`) |
| GET | `/api/v1/blog-publishes/{id}/cover` | blogs:publish | the cover bytes for that publish; 404 when it has none |
| POST | `/api/v1/blog-publishes/{id}/report` | blogs:publish | report progress (below) |

`GET /healthz` needs no token and answers `{"ok":true,"version":"..."}`, or
503 when the database does not respond.

## X drafts

```sh
curl -H "Authorization: Bearer $XPOST_TOKEN" -H 'Content-Type: application/json' \
  -d '{"account":"example","proposed_at":"2026-12-01T16:00:00Z","parts":[{"text":"Example"}]}' \
  http://localhost:8090/api/v1/drafts
```

Body: `{account, parts: [{text, media: [{ref, alt}]}], proposed_at, note, idempotency_key}`.
`account` must be added under Settings > X accounts first (otherwise 400
`unknown account <handle>`).
More than one part makes a thread. See [images.md](images.md) for media.

## Blog drafts

`kind: "blog"` (needs `XPOST_BLOGS_ENABLED=1` on the server; otherwise 400):
`{kind, site, slug, title, description, tags, markdown, cover: {ref, alt}, proposed_at, note, idempotency_key}`.

- `site` is the blog's name in Settings.
- `proposed_at` is optional (default: the blog's publish time, 7:00 AM in the
  owner's display zone).
- Title: one line, up to 200 characters. Description: one line, up to 1000
  (over is 400). Over 300 warns (`long_description`) but never blocks.
- Blocking lint (for example an em dash or a banned topic) is 422
  `{error, lint: [{rule, level, where, message, excerpt}]}`. Warnings come
  back in `blog.lint`.

## Replies

Send `in_reply_to` (`kind: "reply"` optional). See [replies.md](replies.md).

## Revise

Answer a rewrite request within 60 minutes. Own pending drafts only (404
otherwise).

- X draft or reply: `{request_id, parts: [{text}]}`. Text and part count may
  change; images can't (a `media` field, or dropping a part with images, is
  400).
- Blog post: `{request_id, post: {title, description, tags, markdown}}`. All
  four required. Slug and cover can't change; other fields are 400. The draft
  comes back with its lint.

Response 200 `{result: "revised" | "unchanged", draft}`. 409 if the request
was cancelled, timed out, replaced or already answered.

## Blog publishes

`GET /api/v1/blog-publishes/{id}` returns:
`publish_id, post_id, version, attempt, state, idempotency_key, site, slug, title, description, tags, markdown, html, date_utc, date_local, zone, published_at, live_url, cover, content_sha256, canonical, marker, expires_at`.

`POST /api/v1/blog-publishes/{id}/report`:
`{status: "opened" | "merged" | "failed", attempt, idempotency_key, content_sha256, pr, head_sha, merge_sha, code}`.
Unknown fields are 400. A stale attempt, key or hash is 409.
