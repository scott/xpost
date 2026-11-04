# Live updates

The queue (every tab and filter), Schedule, the Log, and the draft edit and
blog preview pages update on their own.

## How it works

- The page opens `GET /live/events`, a Server-Sent Events stream for the
  signed-in owner session only. Agent tokens get no live route. Without a
  session it answers 401 JSON, never a redirect.
- Events carry only a sequence number, a kind (`draft`, `rewrite`, `log`,
  `blog`), an id and a version. No text, tokens or other data.
- On an event the page refetches itself over its normal route and swaps
  changed cards in place. A new card gets a brief highlight and is
  announced. While the tab is hidden its title shows a count.
- A card you are using (focused field, typed text, an open Reject panel, a
  form being sent) is never replaced. It shows "Updated. Reload card". The
  draft edit page shows "This draft changed since you opened it. Reload".
  Scroll position and focus are kept.

## Change feed

The feed is the `live_changes` table, filled by SQLite triggers. Agent API
drafts, owner edits, finished or timed-out rewrites and poster status changes
show up within a second or two. The always-on sweep (every minute, posting
on or off) notes rewrite timeouts and keeps the newest 10,000 change rows.

## Limits

- Heartbeat every 25 seconds (the session is re-checked then).
- A stream ends after 30 minutes. The browser reconnects and resumes from
  `Last-Event-ID`.
- At most 4 streams per session and 64 in total (429 with `Retry-After`).
- Without EventSource, after repeated errors, or over the cap, the page
  polls `GET /live/poll?since=N` every 15 seconds (60 while hidden).
- Live requests never extend the session idle timeout. Shutdown closes every
  stream at once.

## Behind a proxy

The stream sends `Cache-Control: no-store, no-transform` and
`X-Accel-Buffering: no` and flushes every event. Caddy with `encode gzip`
passes it through as is. nginx needs nothing more thanks to the header. The
proxy read timeout must be longer than 25 seconds.
