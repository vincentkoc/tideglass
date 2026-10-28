# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). Release notes are also drafted on
the [GitHub releases page](https://github.com/vincentkoc/tideglass/releases).

## [Unreleased]

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

### Changed

- README rewritten; the documented Go requirement now matches `go.mod`
  (Go 1.27+).
- CI runs staticcheck; Dependabot groups Go module and Actions updates.

## [0.1.0] - 2026-06-13

Initial release: local SQLite store, source discovery for crawl archives and
assistant exports (ChatGPT, Claude, Codex), claim extraction with evidence,
review, profile export and agent context rendering.

[Unreleased]: https://github.com/vincentkoc/tideglass/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/vincentkoc/tideglass/releases/tag/v0.1.0
