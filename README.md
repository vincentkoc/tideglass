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
go run ./cmd/tideglass resolve tideglass://intent/work.project.start --json
```

The default database lives at `~/.tideglass/tideglass.db`.

## Intent Service

Agents can resolve reviewed preferences through URI-addressed intent envelopes
instead of parsing profile files:

```sh
tideglass resolve tideglass://intent/work.project.start --json
```

The response includes the resolved intent URI, accepted policy-filtered claims,
unresolved questions, a disclosure policy, a `sha256:` profile hash, and an
immutable snapshot ID. Request envelopes and response snapshots are stored in the
same SQLite database for audit and replay.

Run the local service:

```sh
tideglass serve --addr 127.0.0.1:8765
curl -s http://127.0.0.1:8765/healthz
curl -s 'http://127.0.0.1:8765/resource?uri=tideglass://intent/work.project.start'
```

Minimal MCP-style one-shot resource reads are available for agent wrappers:

```sh
printf '{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"tideglass://intent/work.project.start"}}' |
  tideglass mcp --once
```

The deeper design spec lives at `~/.spec/tideglass-intent-service-mcp.md`.

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
