# Contributing

Thanks for helping. Keep changes small and focused.

## Build

You need Go (the version in `go.mod`). No cgo.

```sh
go build -o xpost ./cmd/xpost
```

Run it locally with the demo in the [README](README.md#quickstart-local-demo).
Tests and the demo use local fakes (`cmd/xpost-fakex`,
`cmd/xpost-fakegander`, and the packages under `internal/.../xfake`,
`gfake`, `hfake`). No test may call X or any real host.

## Checks

Run these before you open a pull request. CI runs the same.

```sh
gofmt -l .                      # must print nothing
go vet ./...
go test -race -count=1 ./...
```

CI also runs the tests in other time zones. Run them if you touch anything
with dates or times:

```sh
for tz in UTC America/Denver Pacific/Auckland Asia/Kolkata; do
  TZ=$tz go test -count=1 ./...
done
```

Secret scan with [gitleaks](https://github.com/gitleaks/gitleaks):

```sh
gitleaks git --redact .
gitleaks dir --redact .
```

## Pull requests

1. Fork, then branch from `main`.
2. Add a test that fails without your change.
3. Open the pull request as a **draft** while you work. Mark it ready for
   review when CI is green.
4. Describe what changed, why, and how you tested it.

Maintainers merge. Do not include secrets, tokens, real account handles or
private hostnames in code, tests, fixtures, screenshots or pull request text.
Use `example.com` and `@example`.

## Style

- `gofmt`. Standard library first; add a dependency only with a reason.
- Posting stays fail-closed. A change must never make xpost post, publish or
  call X where it did not before without an explicit switch.
- No em dashes in user-facing copy. Use periods or commas.

## License

By contributing you agree your contribution is licensed under the MIT
License in [LICENSE](LICENSE).
