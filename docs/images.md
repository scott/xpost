# Images

Posts and thread parts can carry up to 4 images, or 1 GIF, with alt text.

## Intake

- Agents upload with `POST /api/v1/media` and attach the returned ref (their
  own uploads only) to a part, with optional alt text up to 1000 characters.
- The type is sniffed from the bytes, never taken from the client: PNG,
  JPEG, GIF or WebP.
- Limits: 5 MB, 4 to 8192 pixels a side, at most 16.7 megapixels. PNG, JPEG
  and GIF are fully decoded, so a damaged file is refused at intake.
- Every GIF counts as animated: up to 1280x1080, and alone in its part.
- Images are stored as BLOBs in SQLite under random refs, never as files.
  They are served only to the owner's session, with `nosniff` and a
  sandboxing CSP.

## Upload at post time

Just before a part posts, each image is uploaded to X (`/2/media/upload`,
`tweet_image` or `tweet_gif`), its alt text is set (`/2/media/metadata`),
and processing is polled if X asks. Then the part posts with its media ids.
All X calls go through one client, `internal/xauth/posts.go`.

## No double posts

- Each image has its own intent row, written before the upload.
- An upload posts nothing, so an upload whose outcome is unknown, that was
  interrupted, or whose media id is within 15 minutes of expiry is uploaded
  again. It is counted as billed.
- Post rules are unchanged: unknown post outcomes go to review and are never
  re-sent. A retry reuses images still valid on X and never touches parts
  already posted.

## Failures

- A refused upload, failed processing, or refused alt text fails the part.
  xpost never posts without the alt text the owner saw.
- A 429 waits for the reset. Other transient failures back off like posts.
- Waiting for X to finish processing is not an attempt (bounded at 24
  hours).

## Cost

Each upload counts at `XPOST_COST_MEDIA_UPLOAD_USD` and each alt text call at
`XPOST_COST_MEDIA_METADATA_USD`. Both have no default and each fails closed
on its own. Calls with an unknown outcome count as billed. The cap check
covers a whole thread's posts, uploads and alt text calls before it starts.
Log & spend and Settings say which drafts wait and why.
