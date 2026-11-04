# CI workflow

GitHub Actions runs `.github/workflows/ci.yml` on pushes to `main` and
`cursor/**`, and on pull requests.

- **test**: gofmt, `go vet ./...`, `go test -race -count=1 ./...`, and the
  same tests in UTC, America/Denver, Pacific/Auckland, and Asia/Kolkata.
- **gitleaks**: a scan of history and the working tree.

The same checks can be run locally (see the top-level README).
