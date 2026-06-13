# Tideglass

Local-first CLI for building portable, evidence-backed intent and preference
profiles from crawl archives and assistant exports.

## Quickstart

```sh
go run ./cmd/tideglass init
go run ./cmd/tideglass sources --probe --json
go run ./cmd/tideglass ingest codex --path ~/.codex/sessions --limit 200
go run ./cmd/tideglass ask --kind work.project.start "i am starting a new OpenClaw project"
```

The default database lives at `~/.tideglass/tideglass.db`.

## Safety

- Source crawl databases are opened read-only.
- Raw source archives are not copied into exports by default.
- Claims are append-only; corrections are stored as edit overlays.
- Broken sources degrade independently.

