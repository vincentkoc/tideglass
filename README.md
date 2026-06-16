# 🌊 Tideglass

**A local intent service for agents.** Tideglass turns your crawl archives and
assistant exports into portable, evidence-backed intent and preference profiles,
then serves them to agents through URI-addressed request envelopes over the CLI,
local HTTP, and MCP. Everything stays on your machine in a single SQLite store.

It answers the questions an agent should ask before acting on your behalf:

- what does this person actually prefer for *this* task, and how confident are
  we?
- which claims are reviewed and fresh enough to use, and which are still
  guesses?
- what is still unknown, and which gaps must be filled before the agent may act?
- what is safe to disclose to this audience, and what must be redacted?
- can the agent prove what it used without leaking the raw claim text?

The default path is local-first, evidence-linked, and review-gated. Sensitive
disclosure is opt-in and never available to untrusted HTTP or MCP callers.

## Install

From Homebrew:

```sh
brew install vincentkoc/tap/tideglass
```

From source (Go 1.26+):

```sh
go install github.com/vincentkoc/tideglass/cmd/tideglass@latest
```

The default database lives at `~/.tideglass/tideglass.db`. Override it on any
command with `--db <path>`.

## Quickstart

```sh
tideglass init
tideglass sources --probe --json
tideglass ingest codex --path ~/.codex/sessions --limit 200
tideglass ask --kind work.project.start "i am starting a new OpenClaw project"
tideglass review --kind work.project.start
tideglass context --kind work.project.start --for-agent codex
tideglass resolve tideglass://v1/intent/work.project.start --json
```

This is the full loop: discover sources, import evidence, infer claims for an
intent, review them, then hand a budgeted context block (or a resolved intent
envelope) to an agent.

## Intent service

Agents should treat Tideglass as a local intent service, not as a profile-file
parser. The stable boundary is a URI-addressed request envelope; portable files
are immutable snapshots of resolved resources, not the canonical interface.

```sh
tideglass resolve tideglass://v1/intent/work.project.start --json
```

A response envelope includes the resolved intent URI, policy-filtered accepted
claims, unresolved questions, a disclosure policy, an action decision
(`may_act`), a `sha256:` profile hash, optional commitments, and an immutable
snapshot ID. Both request envelopes and response snapshots are stored in the
same SQLite database for audit and replay.

For full control, pass a request envelope instead of a bare URI:

```sh
tideglass resolve --request ./request.json --json
```

```json
{
  "uri": "tideglass://v1/intent/social.dinner",
  "actor": { "type": "agent", "id": "codex", "trust_tier": "trusted" },
  "task": { "goal": "book dinner", "autonomy": "suggest", "stakes": "low" },
  "audience": { "type": "local" },
  "freshness": { "max_age": "720h", "require_reviewed": true },
  "disclosure": { "mode": "minimal", "allow_evidence": false },
  "contract": { "required_slots": ["preference.food.dietary_restriction"] }
}
```

`disclosure.mode` is one of `full`, `minimal`, or `existence`. Sensitive
disclosure (`allow_sensitive`) requires a capability token and is rejected for
plain HTTP and MCP callers.

### Run the local service

```sh
export TIDEGLASS_SERVICE_TOKEN="$(openssl rand -hex 32)"
tideglass serve --addr 127.0.0.1:8765
curl -s http://127.0.0.1:8765/healthz
curl -s -H "Authorization: Bearer $TIDEGLASS_SERVICE_TOKEN" \
  'http://127.0.0.1:8765/resource?uri=tideglass://v1/intent/work.project.start'
```

`serve` binds to loopback only and requires `TIDEGLASS_SERVICE_TOKEN`. Minimal
MCP-style one-shot resource reads are available for agent wrappers while the
long-running MCP server is being built:

```sh
printf '{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"tideglass://v1/intent/work.project.start"}}' |
  tideglass mcp --once
```

The current code proves the transport and policy boundary. The next step is the
v2 agentic envelope with task mode, autonomy, required slots, commitments, and
full MCP `resources/*` + `tools/*` support. The design spec lives at
`~/.spec/tideglass-intent-service-mcp.md`.

## Commands

| Command | Purpose |
| --- | --- |
| `tideglass init` | Create the SQLite store. |
| `tideglass sources [--probe] [--json]` | List importable sources; `--probe` checks local schemas. |
| `tideglass ingest chatgpt\|claude\|codex --path <path> [--limit n]` | Import evidence from an export or session directory. |
| `tideglass ask [--kind <kind>] [--explain] <query>` | Infer claims for an intent and persist them with evidence links. |
| `tideglass review --kind <kind>\|--intent <id> [--all]` | Interactive review loop over inferred claims. |
| `tideglass profile show --kind <kind>\|--intent <id>` | Show accepted claims, optionally budgeted for an agent. |
| `tideglass profile edit <claim-id> --set <value>` | Store a correction as an edit overlay. |
| `tideglass profile export --kind <kind>\|--intent <id>` | Export a portable profile snapshot. |
| `tideglass evidence show <claim-id>` | Show the evidence backing a claim. |
| `tideglass context --kind <kind> --for-agent codex [--budget n]` | Emit a budgeted context block for an agent. |
| `tideglass resolve <uri>\|--request <file>` | Resolve an intent request envelope. |
| `tideglass serve [--addr 127.0.0.1:8765]` | Run the local HTTP intent service. |
| `tideglass mcp --once` | Handle one JSON-RPC `resources/read` request on stdin. |
| `tideglass doctor` | Report store health and configuration state. |
| `tideglass version` | Print the version. |

Every command accepts `--db <path>`, and most accept `--json` for exact,
control-character-free machine output.

## Intent kinds

`ask`, `review`, `context`, and `resolve` are scoped by intent kind. Built-in
kinds carry tailored slot planning and query expansion:

| Kind | Use |
| --- | --- |
| `work.project.start` | Starting a new project or codebase. |
| `work.release` | Cutting and shipping a release. |
| `work.new_job` | Onboarding context and working preferences. |
| `agent.delegation` | Operating boundaries when delegating to an agent. |
| `social.dinner` | Dining preferences, dietary restrictions, budget. |
| `social.dating` | Dating preferences with safety boundaries preserved. |

## Review loop

`tideglass ask` persists inferred claims with evidence links but does not trust
them. Run `tideglass review` before using a profile as agent context:

```sh
tideglass review --kind work.project.start
```

The review TUI supports accept, edit-and-accept, reject, skip, and quit.
Rejected claims are hidden from `profile show`, `context`, and exports; accepted
claims keep their evidence and edit history. Reviews use optimistic revision
checks so concurrent edits cannot silently clobber each other.

## Safety

- Source crawl databases are opened read-only.
- Raw source archives are not copied into exports by default.
- Claims are append-only; corrections are stored as edit overlays.
- Broken sources degrade independently — one bad store does not fail the rest.
- Human text output strips terminal control characters; JSON output stays exact.
- Sensitive disclosure requires a capability token and is denied to untrusted
  HTTP and MCP callers.

## Layout

```text
tideglass/
  cmd/tideglass/main.go      CLI entrypoint and command routing
  internal/app/app.go        store, ingest, intent resolver, HTTP + MCP service
  .goreleaser.yml            release packaging
  .github/workflows/         CI, CodeQL, release
```

Tideglass is a small Go program built on
[crawlkit](https://github.com/openclaw/crawlkit) for source-store access and a
pure-Go SQLite driver, so it ships as a single static binary with no system
dependencies.

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

## Status

Working local-first foundation: ingest, evidence-linked claims, the review loop,
profile export, and a URI-addressed intent service over CLI, local HTTP, and
one-shot MCP. Next is the v2 agentic envelope and a long-running MCP server.

💙 built by [Vincent Koc](https://github.com/vincentkoc).
