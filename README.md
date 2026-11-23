<p align="center">
  <img src="gondola.png" alt="Three gondola cabins carrying Go gophers between two computer towers at dusk" width="760">
</p>

<h1 align="center">gondola</h1>

<p align="center">
  <em>A gondola carries its passengers between stations and stops only where<br>
  it's sent — this one carries requests between machines.</em><br>
  <strong>G</strong>o-<strong>O</strong>rchestrated <strong>N</strong>ode <strong>D</strong>ispatch <strong>O</strong>ver <strong>L</strong>ocal <strong>A</strong>rea.
</p>

<p align="center">
  <img src="https://github.com/RyanKeys/Gondola/actions/workflows/ci.yml/badge.svg" alt="CI">
  <img src="https://img.shields.io/github/v/release/RyanKeys/Gondola" alt="Latest release">
  <img src="https://img.shields.io/badge/go-1.24-00ADD8" alt="Go 1.24">
  <img src="https://img.shields.io/badge/license-MIT-green" alt="MIT License">
</p>

A mode-aware routing proxy for **multiple Ollama machines**, written in
Go as a single static binary. Point your agent or app at one endpoint;
the dispatcher decides, per request, which PC/GPU serves it — waking that
machine over Wake-on-LAN when the active mode allows it.

A **mode** is a named assignment of machines to jobs: `home` might keep
everything on the always-on server and never wake the gaming PC, `away`
might prefer the big GPU, `test` pins unassigned requests to one machine
and model so benchmarks stay honest (roles still route), and `dev`
prefers the strong machine but expires back to `home` after a few hours.
Modes are configuration, not code — define your own in `dispatcher.yaml`.

- **Modes are policy** — which machines serve, which are ignored, who may be woken, optional TTLs that fall back to a default mode
- **Roles, not just models** — clients ask for `brain`, `heavy`, `coder`, `utility`; each role can name a different model per machine
- **Pass-through, always** — an explicitly named model is never silently substituted (see below)
- **Wake-on-LAN** — asleep machines are woken when the mode sends them work
- **Wake-ahead** — switching modes wakes the machines the new mode uses, before the first request arrives
- **Live throughput** — tokens/sec per request in the console, from the backends' own counters
- **Discovery** — `gondola discover` scans your LAN and writes a starter config
- **A web console** — modes, live activity, and structured settings at `/ui`
- **One static binary** — no runtime dependencies; Ollama- and OpenAI-compatible endpoints

Full design: [SPEC.md](SPEC.md). The Python prototype this replaces
lives in [prototype-python/](prototype-python/).

## Why gondola

Two problems show up as soon as a second machine runs Ollama.

The first is policy. Which machine *should* answer depends on where
you are and what you are doing: the always-on server is fine until you
leave the house, the gaming PC is faster but should not be woken for
work the server handles, and a benchmark is meaningless if requests
quietly land on whichever machine felt like it. Gondola makes that
policy explicit and switchable — a mode states which machines serve,
which are ignored, and which may be woken, and switching modes is the
whole control surface.

The second is honesty. A router that silently substitutes a different
model when the requested one is missing makes every measurement
downstream of it unreliable. Gondola's rule is pass-through: a named
model is requested exactly, or the backend's own error comes back.
The rewrites that do exist are deliberate, documented, and logged.

|  | **gondola** | **LiteLLM** | **Ollama alone** |
|---|---|---|---|
| Built for | a handful of your own machines on a LAN | many models and providers, mostly cloud | a single machine |
| You route by | modes and roles — "at home the server takes it; the big GPU only for heavy work" | model names, API keys, budgets, fallbacks | whichever model you name |
| A missing model | the backend's 404, returned as-is | a configured fallback serves instead | Ollama's 404 |
| Wakes a sleeping machine | yes, when the mode allows | no | no |
| You run | one static binary and a YAML file | a Python service | nothing extra |

## Console

A running gondola serves a small web console at `/ui`: switch modes,
watch backend health, see what each mode actually uses or ignores,
review the most recent routing decisions (what was delegated where,
and whether a rewrite applied), and change settings through
structured controls — including renaming a device, which rewrites
every mode and role that references it. Settings saves are validated
through the real loader before they touch disk, keep a `.bak` of the
previous file, and apply on restart — when a save leaves the running
config behind, the console says so and offers the restart itself.
A raw config editor remains available behind an Advanced section as
an escape hatch. When an
admin token is configured, the console's data endpoints require it;
the token itself is never shown in the editor.

## Pass-through guarantee

Gondola never silently substitutes models. Name a model and that exact
model is requested from the serving backend; if the backend doesn't
have it, you get its 404 — not a different model. The only rewrites are
the deliberate ones: role names, bare backend aliases, model-less
requests, and pin modes. Each one is recorded in the activity log.

## Quick start

```bash
./gondola discover        # scan the LAN, write a starter dispatcher.yaml
```

or by hand:

```bash
cp dispatcher.example.yaml dispatcher.yaml   # your machines, models, modes
go build -o gondola .
./gondola serve --config dispatcher.yaml
```

Then point any Ollama or OpenAI-compatible client at
`http://localhost:11435` and switch modes with the same binary:

```bash
./gondola mode          # show the active mode
./gondola mode dev      # switch
./gondola modes         # which PC/GPU each mode invokes
./gondola health
```

Or over HTTP: `GET|POST /mode`, `GET /modes`, `GET /health`.

## Endpoints

Ollama-native: `POST /api/chat`, `POST /api/generate`, `POST /api/embed`,
`POST /api/embeddings`, `POST /api/show`,
`GET /api/ps`, `GET /api/tags` (merged from all reachable backends,
without waking any), `GET /api/version`. OpenAI-compatible:
`POST /v1/chat/completions`, `POST /v1/embeddings`, `GET /v1/models` (both backends' defaults,
backend aliases, and all role names). Streaming passes through as-is.

Roles let clients ask for a job instead of a model — `brain`, `heavy`,
`coder`, `utility` by default — and the mode decides which machine
serves the role.

## Tests

```bash
go test ./...
```

The routing core is pure, so the full routing matrix — including the
pass-through guarantee — is covered without any Ollama backend running.

## License

MIT
