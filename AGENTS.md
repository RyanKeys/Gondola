# Working in this repo

Gondola is a single-binary LLM request router for local fleets
(Go, no runtime dependencies). Before reviewing or changing
anything:

1. Read **DESIGN.md** — the invariants, security posture, and
   accepted tradeoffs. Several things that look like defects
   (no proxy timeout, synchronous wake waits, empty admin token
   default, header pass-through) are documented decisions.
   Argue with the document if you disagree; don't "fix" the code
   against it silently.
2. Read the relevant **SPEC.md** section for the feature you're
   touching. New behavior is specced there first, as a numbered
   section.
3. README.md and TUTORIAL.md describe the operator-facing
   surface (config, API, console).

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
```

All three must pass before a change is proposed. The version
constant in server.go must match the release tag when one is
cut.

## Rules

- The pass-through guarantee (DESIGN.md, invariant 1) is the
  product's core promise. Any change to model resolution needs
  tests proving explicitly named models are never substituted.
- Config validation rejects unknown backends, modes, and roles
  with clear errors — keep it that way; no silent fallbacks.
- Errors at API boundaries are loud and specific. A request that
  cannot be served as asked fails; it is never quietly rerouted.
- Documentation in this repo describes the code as it is. If the
  code and a document disagree, the document is the bug — fix it
  in the same change.
