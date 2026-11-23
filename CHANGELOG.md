# Changelog

All notable changes to gondola are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/), and versions match the
`version` constant in the code and the GitHub release tags.

## [0.9.0] — 2026-11-23

### Added

- **OpenAI-dialect backends** (SPEC §23): a backend can now declare
  `dialect: openai`, letting gondola route to OpenAI-compatible servers
  (not just Ollama) — `/v1/chat/completions` is proxied to them, other
  paths fail loudly instead of being silently mishandled, readiness is
  probed per dialect, and throughput reporting uses the server's own
  timing counters when it provides them.
- `DESIGN.md` — the invariants, threat model, and accepted tradeoffs,
  written for reviewers and contributors.
- `AGENTS.md` — working rules for automated contributors.

## [0.5.0] — 2026-11-22

### Added

- **Apply settings from the web console** (SPEC §17): when a settings
  save leaves the running configuration behind, the console shows a
  restart-pending banner and can restart gondola in place; the active
  mode is preserved across the restart, and an unloadable saved config
  is refused before restarting.

## Earlier versions

Releases up to 0.4.3 — including the first prebuilt Linux amd64/arm64
binaries — are on the
[releases page](https://github.com/RyanKeys/Gondola/releases).
