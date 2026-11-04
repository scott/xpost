# Replies

Agents can propose replies to other people's posts. xpost never posts a
reply through the X API. You approve it in xpost, then post it yourself on
x.com through an intent link.

## Proposing a reply

`POST /api/v1/drafts` with `in_reply_to` (`kind: "reply"` is optional;
`drafts:write` covers it):

`{kind, account, in_reply_to, original: {author_name, author_handle, text, created_at, url, unavailable}, reply_settings, parts: [{text}], note, idempotency_key}`

- `in_reply_to` is the post id (1 to 30 digits) or its status link
  (`https://x.com/<handle>/status/<id>`, also `www.` and `mobile.`; query and
  fragment are ignored). It is stored as digits.
- `original` is what the agent read. xpost never fetches it (no paid read).
  `author_handle` is 1 to 15 letters, digits or `_`; `created_at` is RFC
  3339; `url` must be that post's link by that handle; `text` is up to 4000
  bytes.
- Send `unavailable: true`, or leave out the handle or text, when the post
  could not be read. The reply is stored, and Approve stays blocked until the
  original loads.
- `reply_settings` is X's value for the original: `everyone`,
  `mentionedUsers`, `following`, `subscribers` or `verified` (empty for
  unknown). Anything but `everyone` warns that X may refuse the reply. It
  never blocks.
- One part, text only (no `media`), and no `proposed_at`.

Example:

```json
{"account": "example", "in_reply_to": "https://x.com/example_two/status/1234567890",
 "original": {"author_name": "Example Two", "author_handle": "example_two",
              "text": "Example original post.", "created_at": "2026-11-01T15:00:00Z",
              "url": "https://x.com/example_two/status/1234567890"},
 "reply_settings": "everyone", "parts": [{"text": "Example reply."}]}
```

The draft comes back with `kind: "reply"` and
`reply: {in_reply_to, original, reply_settings, posts_by: "hand", approve_block, warnings, lint, opened_at, posted_url}`.

`approve_block` says why Approve is off: the original did not load, the text
is over 280 weighted characters, or the account's blocking lint matched. A
second reply from the same account to the same post only warns.

## Approve, open, mark posted

1. Approve never posts.
2. **Open on X** opens the intent link
   (`<XPOST_X_WEB_URL>/intent/post?text=...&in_reply_to=...`) with the text
   and target filled in. You press Post on x.com.
3. Back in xpost, mark it posted, with an optional link to the reply
   (`x.com/<account>/status/<digits>`, format check only).

After 24 hours opened but not marked, the queue shows "Not marked yet". It
never reverts on its own.

The poster and scheduler skip replies. They never reach the post log and add
$0 to the spend estimate.

Open on X is a form POST that redirects to x.com, so the queue and a reply's
page add the X web origin to the CSP `form-action`. Every other page keeps
`form-action 'self'`. With JavaScript, the click opens the intent link
directly, records the open with a fetch POST, and updates the card without a
reload; other tabs follow over [live updates](live-updates.md). If the
browser blocks the new tab, it falls back to the form POST.

## Errors

400 `{error}`, for example:

- `in_reply_to must be a post id (digits) or an x.com status link`
- `a reply needs in_reply_to: the post id or its x.com link`
- `original and reply_settings are for replies: send in_reply_to too`
- `original.url must be the x.com link of the post in in_reply_to`
- `original.url is not a post by original.author_handle`
- `reply_settings must be one of ...`
- `a reply has exactly one part`, `a reply can't have images`
- `a reply has no proposed_at: ...`, `a reply can't promote a blog post`

Rewrite works for replies too. See [webhooks.md](webhooks.md).
