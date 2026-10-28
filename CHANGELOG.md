# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). Release notes are also drafted on
the [GitHub releases page](https://github.com/vincentkoc/tideglass/releases).

## [Unreleased]

## [0.2.0] - 2026-10-28

### Added

- Intent service: URI-addressed request/response envelopes over the CLI
  (`tideglass resolve`), local HTTP (`tideglass serve`) and MCP
  (`tideglass mcp --once`).
- Action gating with persisted, revision-bound snapshots, scoped action
  constraints and commitments (response hash, claim root).
- Review flow that surfaces conflicts, pending blockers and legacy claim
  revisions atomically.
- MIT license.

### Security

- Fail closed when an action snapshot cannot be bound consistently, on stale
  action snapshots, on tied singleton or action-constraint claims, and on
  legacy claim ties.
- Sensitive disclosure is scoped per audience and purpose; existence of
  sensitive claims is hidden from untrusted callers, and transport leaks of
  sensitive values were closed.
- Reject oversized or malformed HTTP and MCP input, unknown policy fields and
  unsupported proof requests.

### Fixed

- Corrupt persisted source JSON is reported instead of being silently
  dropped. (#28)
- CLI usage text indentation. (#25)

### Changed

- README rewritten; the documented Go requirement now matches `go.mod`
  (Go 1.27+).
- CI runs staticcheck; Dependabot groups Go module and Actions updates.
- Release archives include LICENSE. (#27)
- Homebrew now installs tideglass as a cask (`brew install
  vincentkoc/tap/tideglass`); the old formula migrates to the cask. (#31)
- crawlkit 0.11.0 → 0.17.2. (#19)

## [0.1.0] - 2026-06-13

Initial release: local SQLite store, source discovery for crawl archives and
assistant exports (ChatGPT, Claude, Codex), claim extraction with evidence,
review, profile export and agent context rendering.

[Unreleased]: https://github.com/vincentkoc/tideglass/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/vincentkoc/tideglass/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/vincentkoc/tideglass/releases/tag/v0.1.0
