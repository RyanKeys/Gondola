# Gondola — design notes

Gondola (Go-Orchestrated Node Dispatch Over Local Area) routes LLM
requests across a small fleet of local machines. Clients speak to
one endpoint; gondola decides which machine serves each request
based on **roles** (brain / heavy / coder / utility, or your own),
**modes** (named topologies such as home / away / test / dev), and
each backend's configured dialect. It wakes sleeping machines with
Wake-on-LAN, tracks throughput per backend and model, and ships as
a single static binary with one YAML config file.

This document records the decisions and invariants behind the
code — the things a reader (human or agent) needs before judging
or changing it. Feature-level detail lives in SPEC.md; usage in
README.md and TUTORIAL.md.

## Intended deployment

A single operator's trusted LAN or tailnet: a home lab, a small
studio, one team's rack. Gondola is **not** multi-tenant and makes
no attempt to isolate mutually distrustful users from each other.
Several defaults below are only safe inside that boundary; each
one says so.

## Invariants

1. **Pass-through guarantee.** A request that names a model is
   forwarded to the serving backend with that model name
   untouched. Gondola rewrites only: role names, bare backend
   aliases, requests with no model, and the test-mode default pin.
   Every rewrite is logged. If a backend doesn't have the model,
   the backend's own error comes back — gondola never silently
   substitutes a different model. A wrong answer is worse than a
   loud error.
2. **No total timeout on the proxy client.** Generation streams
   are long-lived; a client-side total timeout would kill them.
   Probes, health checks, and discovery use short explicit
   timeouts (2–3 s) — only the proxy path is unbounded, on
   purpose.
3. **Wake waits are synchronous.** When a request routes to a
   sleeping machine, the request waits (up to `wake_timeout`,
   default 150 s) while the machine wakes and its backend starts
   answering. The alternative — failing the request — makes
   on-demand machines useless. The wait is the feature.
4. **The mode is the operator's signal.** Gondola never changes
   its own mode. Mode changes arrive through the API or console,
   from the operator. (One designed exception: dev mode carries a
   TTL and falls back to the default mode when it expires, so a
   forgotten dev session can't strand the fleet.)
5. **One binary, one config, one state file.** No database, no
   runtime dependencies, no plugin host. Extensibility lives at
   the API and configuration layer (see Modularity).

## Security posture

- **Admin token.** When `admin_token` is set, it is required for
  the admin API, settings changes, restarts, and mode changes.
  When it is empty — the default — those surfaces are **open**.
  That is intentional for zero-config use on a trusted network,
  and it is the operator's responsibility to set a token anywhere
  else. Gondola will never ship with a default token: a known
  default secret is worse than none.
- **Backend API keys.** OpenAI-dialect backends may need a key.
  Keys live in the config file — protect it like any credential
  file. Gondola injects the key when proxying to that backend and
  **replaces** the client's Authorization header on those routes,
  so a backend key is never mixed with a client's credentials.
- **Header transparency.** Otherwise, the proxy forwards client
  headers to backends verbatim. Gondola is a transparent router,
  not a sanitizing gateway. Deployments that need header
  isolation should terminate clients at a gateway in front of it.
- **Discovery** scans the local subnet for Ollama-shaped
  services. It runs only when the operator invokes it.

## Accepted tradeoffs

- No proxy timeout ↔ long generations are never cut off; a
  backend that accepts a request and never responds can hold it.
- Synchronous wake wait ↔ the first request to a sleeping node
  is slow once, instead of failing.
- Empty-token default ↔ zero-config on trusted networks; the
  burden of setting one sits with the operator off them.
- Header transparency ↔ backends see client headers; isolation
  is a deployment concern, not a router feature.
- YAML + a settings API ↔ no plugin runtime; new capability
  arrives as new dialects, roles, and config, not extensions.

## Modularity and integration

The seams an integrator actually touches:

- **Backends are data.** Any Ollama node works out of the box.
  Any endpoint that speaks OpenAI chat completions works by
  setting the backend's `dialect` to `openai` (plus its key and
  context size). Roles map to per-backend model overrides, so a
  heterogeneous fleet presents one set of role names.
- **Clients are unchanged.** Anything that speaks the Ollama API
  or OpenAI chat completions can point at gondola as-is. Role
  names work as model names; the `X-Backend` header pins a
  request to a specific backend when a client needs control.
- **Topologies are data.** Modes and role assignments are YAML /
  settings-API values, not code. Operators define their own.
- **Observable by default.** The activity feed, per-backend /
  per-model throughput summaries, and the health endpoint are
  plain HTTP/JSON, and the web console is served by the binary
  itself.

Known limits, stated plainly: OpenAI-dialect backends are proxied
for chat completions only — other paths to them fail loudly
rather than mistranslating. Some serving engines (e.g. Strata)
run one model per process; gondola routes to them like any other
backend, and process lifecycle on the serving host is the
operator's concern.

## Changing gondola

New behavior lands as a numbered SPEC.md section first, with the
design question stated before the mechanism. Changes that would
break an invariant above need to argue the invariant away in the
open, in the spec — not route around it in code.
