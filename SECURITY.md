# Security Policy

## Supported versions

Only the latest release and the `main` branch receive security fixes.

## Reporting a vulnerability

Do not open a public issue for a vulnerability. Report it privately through
GitHub's
[private vulnerability reporting](https://github.com/vincentkoc/tideglass/security/advisories/new)
for this repository, with the affected version or commit, the impact, and
steps to reproduce.

## Scope

Tideglass stores personal preference profiles and evidence locally and serves
them to agents. High-impact classes include:

- disclosure of sensitive claims, or of their existence, to an audience or
  caller that should not see them
- an agent being told it may act (`may_act: true`) when a required claim is
  missing, stale, unreviewed, tied or not bound to a consistent snapshot
- bypassing the service token, host or write checks on `tideglass serve`, or
  binding it to a non-loopback address
- tampering with snapshots, commitments or review history
- path traversal or resource exhaustion through ingest, HTTP or MCP input
