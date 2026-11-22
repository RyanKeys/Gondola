# gondola

*A gondola carries its passengers between stations and stops only where
it's sent — this one carries requests between machines.*
***G**o-**O**rchestrated **N**ode **D**ispatch **O**ver **L**ocal **A**rea.*

A mode-aware routing proxy for **multiple Ollama machines**, written in
Go as a single static binary. Point your agent or app at one endpoint;
the dispatcher decides, per request, which PC/GPU serves it — waking that
machine over Wake-on-LAN when the active mode allows it.

A **mode** is a named assignment of machines to jobs: `home` might keep
everything on the always-on server and never wake the gaming PC, `away`
might prefer the big GPU, `test` pins every request to one machine and
model so benchmarks stay honest, and `dev` prefers the strong machine
but expires back to `home` after a few hours. Modes are configuration,
not code — define your own in `dispatcher.yaml`.

Full design: [SPEC.md](SPEC.md). The Python prototype this replaces
lives in [prototype-python/](prototype-python/).

## Console

A running gondola serves a small web console at `/ui`: switch modes,
watch backend health, see what each mode actually uses or ignores,
review the most recent routing decisions (what was delegated where,
and whether a rewrite applied), and change settings through
structured controls — including renaming a device, which rewrites
every mode and role that references it. Settings saves are validated
through the real loader before they touch disk, keep a `.bak` of the
previous file, and apply on restart; a raw config editor remains
available behind an Advanced section as an escape hatch. When an
admin token is configured, the console's data endpoints require it;
the token itself is never shown in the editor.

## The one rule that matters most

An explicitly named model is **passed through untouched**. If the chosen
backend doesn't have it, Ollama's 404 comes back to the client — loudly.
The dispatcher never silently substitutes a different model. (An earlier
version did. It invalidated a week of benchmarks before anyone noticed.)
Only four rewrites exist — role names, bare backend aliases, model-less
requests, and the test-mode pin — and every one is logged.

## Quick start

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

Ollama-native: `POST /api/chat`, `POST /api/generate`, `POST /api/show`,
`GET /api/ps`, `GET /api/tags` (merged from all reachable backends,
without waking any), `GET /api/version`. OpenAI-compatible:
`POST /v1/chat/completions`, `GET /v1/models` (both backends' defaults,
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
