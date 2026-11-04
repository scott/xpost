# Connecting X accounts

## X developer app

In the X developer console:

1. Set the app type to **Web App** (confidential client).
2. Turn on OAuth 2.0.
3. Register this exact callback URL: `<base URL>/auth/x/callback`, for
   example `https://xpost.example.com/auth/x/callback`.

Set `XPOST_X_CLIENT_ID`, and the secret with `XPOST_X_CLIENT_SECRET` or
`XPOST_X_CLIENT_SECRET_FILE` (one or the other). Without a client ID,
Connect is off and the Accounts page says so.

Scopes requested: `tweet.read users.read tweet.write media.write offline.access`.

## Add an account

A fresh install has no X accounts. Signed in as the owner, open **Settings >
X accounts** (`/settings/accounts`), type the handle under "Add an X
account" (1 to 15 letters, digits or underscores; a leading @ is dropped)
and press **Add account**. Agents can propose drafts only for accounts added
here; the API answers 400 `unknown account <handle>` for any other.
`xpost seed-demo` adds its two sample accounts on a demo database only.

## Connect

Signed in as the owner, open **Settings > X accounts**
and press **Connect** on an added handle while signed in to
X as that same account. xpost refuses a different X user and revokes what it
got.

- Tokens are encrypted with the master key.
- Access tokens refresh when within 5 minutes of expiry or when X answers
  401.
- If X refuses the refresh token, the account shows **Needs reconnect**.
- **Disconnect** revokes the tokens at X (best effort) and deletes them
  locally.

See [OAuth 2.0](https://docs.x.com/fundamentals/authentication/oauth-2-0/overview).

## Endpoint overrides

| Key | Default |
| --- | --- |
| `XPOST_X_AUTHORIZE_URL` | `https://x.com/i/oauth2/authorize` |
| `XPOST_X_TOKEN_URL` | `https://api.x.com/2/oauth2/token` |
| `XPOST_X_REVOKE_URL` | `https://api.x.com/2/oauth2/revoke` |
| `XPOST_X_ME_URL` | `https://api.x.com/2/users/me` |
| `XPOST_X_POST_URL` | `https://api.x.com/2/tweets` |
| `XPOST_X_MEDIA_URL` | `https://api.x.com/2/media/upload` |
| `XPOST_X_MEDIA_METADATA_URL` | `https://api.x.com/2/media/metadata` |
| `XPOST_X_WEB_URL` | `https://x.com` (base of post links and reply intent links) |

## Trying it without X

`cmd/xpost-fakex` is a fake X for local use. It binds to loopback only and
prints the variables to set:

```sh
go run ./cmd/xpost-fakex -addr 127.0.0.1:8099
# export the XPOST_X_* lines it prints in the shell that runs serve
```

Flags: `-addr`, `-users` (comma-separated handles it can sign in as),
`-client-id`.

Its consent page has a button per handle. It also serves create-post and
media upload:

- `GET /_fake/posts` lists what it "posted".
- `GET /_fake/media` lists uploads.
- `POST /_fake/fault` queues a failure for the next call: `kind=status&status=503`,
  `kind=ratelimit&seconds=60`, `kind=drop-after`, `kind=drop-before`,
  `kind=garbled`; `target=post` (default), `upload`, `metadata` or `status`;
  for uploads also `kind=process&checks=2` and `kind=process-fail`.

Tests use the same fake through httptest. No test or CI job reaches X.
