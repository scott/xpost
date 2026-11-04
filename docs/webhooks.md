# Webhooks

xpost sends two kinds of signed webhook: rewrite requests to the agent that
wrote a draft, and publish wakes to a webhook blog's agent. Both use the same
signature scheme.

## Signature

| Header | Value |
| --- | --- |
| `X-Xpost-Event` | the event name |
| `X-Xpost-Timestamp` | unix seconds |
| `X-Xpost-Signature` | `sha256=` + hex HMAC-SHA256 of `timestamp + "." + body`, keyed with the sender key |
| `Authorization` | only if the owner saved one (see below) |

Verify the signature and reject old timestamps. The key itself is never
sent, and payloads carry no tokens. Any 2xx response accepts.

**Authorization header.** Optional, for receivers that require one. The
owner pastes the value, for example `Bearer ...`. One leading
`Authorization:` (any case) is removed, so a pasted full header line works;
a bare `Authorization:` is refused. `Bearer` is never added or removed. It is
sent as saved (trimmed) on every event, sealed with the master key, never
shown again (Settings shows only Set, with Replace and Clear) and never
logged. Line breaks are refused.

## Rewrite requests

The owner can send a pending agent draft back to the agent that wrote it,
with an optional note. Each agent token gets a webhook URL and a sender key
under **Settings > Rewrite webhooks** (https only; the key is sealed with the
master key and never shown again).

Events: `rewrite.requested`, and `rewrite.test` for the Settings Test button.

Body for an X draft:

```json
{"type": "rewrite.requested", "request_id": 7, "draft_id": 12,
 "parts": [{"text": "...", "images": 0}], "note": "...", "expires_at": "...",
 "callback": {"method": "POST", "url": "https://xpost.example.com/api/v1/drafts/12/revise",
              "scope": "drafts:revise", "body": "{\"request_id\": 7, \"parts\": [{\"text\": \"...\"}]}"}}
```

`images` is the number of images on that part (images can't change in a
rewrite). `callback.body` is a string template of the answer.

- A blog post request carries `kind: "blog"` and
  `post: {site, slug, title, description, tags, markdown, cover}` (`cover` is true or false) instead of
  `parts`.
- A reply request carries `kind: "reply"` and
  `reply: {in_reply_to, account, original, reply_settings}`.

The agent answers within 60 minutes with `POST /api/v1/drafts/{id}/revise`
using its own `drafts:revise` token (see [api.md](api.md)). While it works,
Approve and Edit are blocked; Reschedule and Reject stay, and Reject cancels
the request. Rewrites post nothing and do not count toward the spend cap.

A failed Settings Test names the outcome, for example `HTTP 401` (check the
Authorization header).

Blog rewrites are for Pending posts only. A publish in flight refuses both
the request and the reply. While a blog rewrite waits, Publish now and Edit
wait too. The revised post comes back Pending, intake checks and lint run
again, and blocking lint still stops Approve and Publish now.

## Blog publish wakes

For webhook blogs (see [blogs.md](blogs.md)), with `XPOST_BLOG_LIVE=1`.

Events: `blog.publish`, and `blog.test` for the Settings Test button.
Wakes also carry `X-Xpost-Delivery` (unique per send).

The body is thin and names a publish only:

```json
{"event": "blog.publish", "publish_id": 1, "version": 1,
 "delivery_id": "...", "expires_at": "..."}
```

Treat it as untrusted. Pull the post with a `blogs:publish` token from
`GET /api/v1/blog-publishes/{id}`, the cover from `/cover`, and report to
`/report` (see [api.md](api.md)).
