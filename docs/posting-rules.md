# Posting rules

Posting is off by default. It runs only with `XPOST_SCHEDULER=on` (the real
X poster) or `-fake-poster` (local only, never calls X). With either, the
dispatcher checks every 30 seconds.

## Fail closed

Nothing posts while:

- no monthly spend cap is set (Settings; more than $0, at most $10,000,
  every change logged, no default),
- the cost rates are unset (`XPOST_COST_POST_USD` and
  `XPOST_COST_LINK_POST_USD`; the meter shows "unverified"), or
- the month's estimated spend has reached the cap.

A draft whose cost would cross the cap waits. A thread never starts unless
all of its parts fit. The month is the owner's display-zone month. Log &
spend says why posting is held.

## Time and spacing

- An approved draft posts at its scheduled time.
- A new item on an account starts no sooner than 15 minutes after that
  account's previous item started. It waits if needed.
- Thread parts post back to back as a reply chain.

## No double posts

- Each part has an idempotency key (draft, approved version, part).
- Its intent is written to SQLite before X is called. A part is sent only if
  its key has no posted, unknown or in-flight row.
- After a restart, a thread resumes from the first part not on X, replying
  to the last posted id.

## Unknown outcomes

A timeout, dropped connection, 5xx, unreadable reply, or crash during a call
marks the part **needs review**. xpost never re-sends it on its own. Check X,
then on the Log page either paste the post link ("It posted") or choose "It
did not post", and press Retry.

## Failures and retries

- A 4xx refusal fails the draft. **Retry** on the Log page posts only the
  remaining parts.
- A 429 waits until X's `x-rate-limit-reset` (or the 24-hour user limit
  reset).
- A request that never reached X backs off 1, 2, 4 ... 30 minutes. After 8
  attempts the draft fails.
- The log keeps each part's X post id, link and error.

Agent tokens can never approve, edit approved items, retry, resolve, change
the cap, or post.

## Review flow

After Approve (from the queue, the edit page or reschedule), the queue stays
on Pending while anything is left, at the next card, with "Approved. N left
to review." After the last one it lands on the approved card.

The bare `/queue` shows Approved, today first, when nothing is pending and
something was approved or is due today in the owner's zone. A `?status=` or
filter in the URL shows exactly that.

## Posted tab

The queue has a **Posted** chip next to Pending, Approved and Rejected. It
lists posted drafts, newest first by when their last part posted. Each card
links every part's live post at `<XPOST_X_WEB_URL>/<handle>/status/<id>`
(default `https://x.com`). Ids that are not decimal are never linked ("No
post id recorded"). A handle X would not issue falls back to
`/i/status/<id>`.

With `XPOST_XSTATS_URL` set, each part also links to `<base>/#post/<id>`.

Posted drafts are read-only. The server refuses edit, approve, reject,
reschedule and delete on them.

Posting and failed drafts (failed includes needs review) have no queue tab.
They are on the **Log** page (Failed filter), and the queue links there
whenever any exist.

## Cost rates

| Key | Meaning |
| --- | --- |
| `XPOST_COST_POST_USD` | one post. No default. Set it and the link rate together, or neither |
| `XPOST_COST_LINK_POST_USD` | one post with a link. No default |
| `XPOST_COST_MEDIA_UPLOAD_USD` | one image upload. No default. Unset: drafts with images wait |
| `XPOST_COST_MEDIA_METADATA_USD` | one alt text call. No default. Unset: drafts with alt text wait |

`0` is a valid rate (free). xpost ships no prices. Check them against your X
developer console and [X API pricing](https://docs.x.com/x-api/getting-started/pricing).
