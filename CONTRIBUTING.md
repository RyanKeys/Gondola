# Contributing to gondola

Thanks for your interest. Gondola is a small, single-purpose tool and
the contribution rules follow from that.

## Before a larger change

Read [DESIGN.md](DESIGN.md) and [SPEC.md](SPEC.md) first. DESIGN.md
lists the invariants a change must not break — the pass-through
guarantee, the deliberate absence of a total proxy timeout, the
synchronous wake wait, and modes as the operator's signal. A pull
request that reverses one of those needs to argue the reversal, not
just make the edit.

## Build and test

```bash
go build ./...
go vet ./...
go test ./...
```

All three must pass. New behavior needs tests; the routing core is
pure, so routing changes can be tested without any Ollama backend
running.

## Shape of the project

- One static binary, one YAML config, one small state file. New
  runtime dependencies, sidecar processes, and config formats need a
  strong reason.
- Documentation conforms to the code, never the reverse: if the code
  and a document disagree, fix the document in the same change.
- Bug fixes and behavior changes update SPEC.md (and CHANGELOG.md for
  anything user-visible) in the same pull request.

## Reporting bugs

Use the bug report template and include your mode configuration (the
relevant part of `dispatcher.yaml`), what you asked gondola to route,
and what the activity log in the console showed. Redact anything you
consider private first — configs routinely contain LAN addresses.
