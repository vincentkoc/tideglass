# Tideglass

Local-first CLI for building portable, evidence-backed intent and preference
profiles from crawl archives and assistant exports.

## Quickstart

Install from Homebrew:

```sh
brew install vincentkoc/tap/tideglass
```

Or build from source:

```sh
go install github.com/vincentkoc/tideglass/cmd/tideglass@latest
```

```sh
go run ./cmd/tideglass init
go run ./cmd/tideglass sources --probe --json
go run ./cmd/tideglass ingest codex --path ~/.codex/sessions --limit 200
go run ./cmd/tideglass ask --kind work.project.start "i am starting a new OpenClaw project"
go run ./cmd/tideglass review --kind work.project.start
go run ./cmd/tideglass context --kind work.project.start --for-agent codex
```

The default database lives at `~/.tideglass/tideglass.db`.

## Review Loop

`tideglass ask` persists inferred claims with evidence links. Run `tideglass review`
before using a profile as agent context:

```sh
tideglass review --kind work.project.start
```

The review TUI supports accept, edit-and-accept, reject, skip, and quit. Rejected
claims are hidden from `profile show`, `context`, and exports; accepted claims keep
their evidence and edit history.

## Safety

- Source crawl databases are opened read-only.
- Raw source archives are not copied into exports by default.
- Claims are append-only; corrections are stored as edit overlays.
- Broken sources degrade independently.
- Human text output strips terminal control characters; JSON output remains exact.

## Release

Tagged releases are published by `.github/workflows/release.yml` through
GoReleaser. A release builds Darwin/Linux `amd64` and `arm64` archives, `.deb`
and `.rpm` packages, checksums, and updates `vincentkoc/homebrew-tap`.

Required release secret:

- `HOMEBREW_TAP_GITHUB_TOKEN`: token with push access to
  `vincentkoc/homebrew-tap`.

Validate release packaging locally:

```sh
goreleaser release --snapshot --clean --skip=publish
```
