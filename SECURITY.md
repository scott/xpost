# Security policy

## Reporting a vulnerability

Report it privately through GitHub security advisories: open the
repository's **Security** tab and choose **Report a vulnerability**, or go to
`https://github.com/scott/xpost/security/advisories/new`. Only the
maintainers can see it.

This is the only reporting channel. There is no security email address.

Do not open a public issue, discussion or pull request for a vulnerability.

Include:

- the xpost version (`xpost version`) or commit
- what an attacker can do, and what they need first
- steps to reproduce, using `example.com` and placeholder data

We aim to acknowledge a report within 7 days and will keep you updated until
it is fixed. Please give us a reasonable time to ship a fix before you
disclose it.

## Never post secrets

Never put any of these in an issue, pull request, log excerpt or screenshot:
the master key (`XPOST_KEY`, `XPOST_KEY_FILE` contents), X client secrets,
access or refresh tokens, agent tokens, webhook signing keys, blog API tokens,
setup links, or a database file. If you posted one by mistake, rotate it
right away and tell us so we can remove the post.

## Supported versions

Only the latest release gets security fixes.

## Scope

In scope: the `xpost` binary, its web UI, its agent API, and the docs in this
repository. Out of scope: X itself, your proxy or server setup, and the demo
fakes (`cmd/xpost-fakex`, `cmd/xpost-fakegander`), which are for loopback
testing only.
