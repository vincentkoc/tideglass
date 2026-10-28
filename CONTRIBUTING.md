# Contributing

Thanks for helping improve Tideglass.

## Setup

You need Go 1.27 or newer (see `go.mod`).

```sh
git clone https://github.com/vincentkoc/tideglass.git
cd tideglass
go build ./...
```

## Checks

CI runs the following; please run them before opening a pull request:

```sh
gofmt -l .                      # must print nothing
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@2026.2.2 ./...
go test -count=1 ./...
go mod tidy && git diff --exit-code -- go.mod go.sum
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
```

Tests must not touch your real crawl databases: use `t.TempDir()` for the
database and set `HOME` / `TIDEGLASS_CRAWLBAR` as the existing tests do.

## Pull requests

- Keep each pull request to one concern and use a Conventional Commits title
  (`fix:`, `feat:`, `docs:`, `test:`, `chore:`), which release-drafter uses
  for the release notes.
- Add or update tests for behaviour changes, especially anything that affects
  disclosure, review or action gating; those paths must fail closed.
- Note user-visible changes under `[Unreleased]` in `CHANGELOG.md`.

## Security

Please report vulnerabilities privately; see [SECURITY.md](SECURITY.md).
