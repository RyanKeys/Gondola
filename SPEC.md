# gondola — Specification v0.1 

A mode-aware routing proxy for multiple Ollama machines, written in Go as a
single static binary. A client points at one endpoint; the dispatcher decides,
per request, which PC/GPU serves it — waking that machine over Wake-on-LAN
when the active mode allows it.

This spec supersedes the running Python prototype (dispatcher v1.3).
The prototype's routing semantics are proven in production on a live fleet;
this spec keeps those semantics, generalizes modes from four hard-coded names
into configuration, and re-implements in Go for distribution (one binary, no
runtime, runs comfortably on a Raspberry Pi).

---

## 1. Purpose and scope

### 1.1 Problem

Homelabs commonly have two classes of machine:

- an always-on, power-efficient box with a small GPU — right for a small
  operator model, wrong for heavy work;
- a powerful machine (often a gaming PC) that should sleep most of the day.

Neither Ollama nor OpenAI-compatible clients can express "use this machine
when I'm home, that machine when I'm away, and never wake the gaming PC
during a benchmark." The dispatcher holds that policy in one place: the
**mode**.

### 1.2 v1 scope

- Backends are **Ollama instances** (native `/api/*` plus the
  OpenAI-compatible `/v1/*` surface Ollama itself exposes). The backend is
  reached through a narrow internal interface (§5.4) so a non-Ollama backend
  can be added later without touching routing; v1 ships Ollama only.
- Two or more backends; exactly one active mode at a time.
- Streaming passthrough for generation endpoints.
- Wake-on-LAN for backends that declare a MAC address.

### 1.3 Non-goals (v1)

- Load balancing or round-robin between backends.
- Putting a backend to sleep (the OS idle timer does that; the dispatcher
  only wakes).
- Model downloads or management (pull/delete stay with Ollama directly).
- Multi-user auth on the proxy surface (see §10 for the admin surface).
- A web UI. Mode switching is API/CLI only in v1.

---

## 2. Concepts

**Backend** — one PC/GPU running Ollama. Declared in config with a name, a
base URL, an optional Wake-on-LAN MAC, a default model, and optional
model-family prefixes that pin matching requests to it.

**Mode** — a named routing policy. A mode assigns, for each class of
request, which backend is invoked, whether waking a sleeping backend is
allowed, and optionally a time-to-live after which the dispatcher falls
back to the configured default mode. Modes are **defined in configuration**,
not hard-coded; §4.3 ships the four starter modes the prototype proved
(home / away / test / dev) as ordinary config.

**Role** — a stable name for a job (`brain`, `heavy`, `coder`, `utility`)
that resolves to a concrete model in config. Clients may ask for a role
instead of a model; the mode then decides which backend serves that role.

**Request classes** (evaluated in §6 order): role requests, header-forced
requests, model-family requests, and everything else (mode default).

---

## 3. Architecture

- One Go binary (`gondola`), `net/http` reverse proxy; no
  external services, no database.
- State: the active mode + when it was set, in a small JSON file on disk
  (path configurable). Everything else is config, loaded at start; mode
  definitions are re-read on mode switch.
- Concurrency: request handling is fully concurrent; wake attempts per
  backend are coalesced behind a per-backend lock so 20 simultaneous
  requests send one magic packet sequence, not 20.
- The binary runs the same under systemd, Docker, or bare on Linux/macOS.
  Host networking (or equivalent) is required for WoL broadcasts.

---

## 4. Configuration

YAML file (`--config`, default `./dispatcher.yaml`), with environment
variables overriding individual fields (prefix `DISPATCHER_`). Secrets are
never part of config examples.

### 4.1 Backends

```yaml
backends:
  primary:                       # the always-on machine
    url: http://192.168.1.10:11434
    default_model: qwen3.5:9b
    model_prefixes: [qwen2.5-coder]     # families pinned to this backend
  secondary:                     # the wakeable machine
    url: http://192.168.1.20:11434
    mac: "AA:BB:CC:DD:EE:FF"            # enables Wake-on-LAN
    wol_broadcasts: [192.168.1.255, 255.255.255.255]
    default_model: qwen3:14b
    model_prefixes: [qwen3:14b]
```

### 4.2 Roles

```yaml
roles:
  brain:   hf.co/deepreinforce-ai/Ornith-1.0-9B-GGUF:Q4_K_M
  heavy:   qwen3.6:35b-a3b
  coder:   qwen3-coder:30b
  utility: qwen3.5:4b
```

A role's value is normally a single model name. It may instead be a
mapping with a `default` model and per-backend `overrides`, for fleets
where the best model for a job differs by machine:

```yaml
roles:
  heavy:
    default: qwen3.6:35b-a3b
    overrides:
      secondary: hf.co/deepreinforce-ai/Ornith-1.0-35B-GGUF:Q4_K_M
```

Resolution (§6) picks the serving backend first (the mode's assignment);
the served model is then that backend's override when one is set, else
the role's default. Validation rejects a mapping with no default, an
override naming an undefined backend, and an empty override model. The
scalar form is exactly equivalent to a mapping with only a default, so
existing configurations behave unchanged.

### 4.3 Modes

A mode assigns backends per request class. `default` is required; `roles`
may override individual roles; `wake` gates WoL for the whole mode;
`pin_default_model` rewrites any model to the chosen backend's default
(the benchmark-isolation behaviour); `ttl` auto-returns to the default
mode on expiry.

```yaml
default_mode: home
modes:
  home:
    default: primary
    wake: false
  away:
    default: secondary
    wake: true
  test:
    default: primary
    wake: false
    pin_default_model: true
  dev:
    default: secondary
    wake: true
    ttl: 8h
    roles:
      brain: primary        # example: keep the operator brain on the
                            # always-on box even while devving
```

Mode names are arbitrary strings from config. Switching to an undefined
mode is an error (§7.2). This is the generalization over the prototype:
the prototype's four modes become starter config, and a user with three
machines or different habits defines their own (e.g. `guest`, `night`,
`render`).

### 4.4 Server

```yaml
server:
  listen: 0.0.0.0:11435
  state_file: /data/mode.json
  wake_timeout: 150s          # how long to wait for a woken backend
  admin_token: ""             # optional; see §10
```

---

## 5. Backend behaviour

### 5.1 Reachability

A backend is "up" when `GET /api/version` returns 200 (2 s timeout). Health
checks never wake a backend.

### 5.2 Wake-on-LAN

When a request routes to a backend that is down, has a MAC configured, and
the active mode has `wake: true`: send the magic packet to each configured
broadcast address, then poll reachability every 5 s (re-sending the packet
every ~30 s) until `wake_timeout`. If the backend never comes up, the
request fails with 502 (§8). If the mode has `wake: false`, a down backend
fails immediately with 502 — the dispatcher must never wake a machine the
mode says to leave alone.

### 5.3 Model inventory

`GET /api/tags` is answered by merging the tag lists of all *reachable*
backends, without waking any. Per-backend duplicates are kept distinct by
backend name in a dispatcher-specific field; the Ollama-shaped `models`
array is the union.

### 5.4 Backend interface (internal)

Routing talks to backends through a small Go interface — base URL,
reachability, tag list, and a proxied request — so a future non-Ollama
backend (llama.cpp, vLLM) is an adapter, not a rewrite. v1 ships the
Ollama adapter only.

---

## 6. Routing algorithm

For each proxied request, in order:

1. **Parse** the requested model from the JSON body (absent is allowed).
2. **Role**: if the model equals a configured role name, the backend is
   the mode's `roles` override for that role if set, else the mode
   default; the served model is the role's per-backend override for
   that backend (§4.2) if set, else the role's default model.
3. **Header**: `X-Backend: <name>` forces that backend (unknown name →
   400). The header is stripped before proxying.
4. **Family pin**: if the model matches a backend's `model_prefixes`, that
   backend serves it.
5. **Mode default**: otherwise the mode's `default` backend serves it.
6. **Served model** is then determined by the rewrite rules (§6.1), the
   backend is woken if required and allowed (§5.2), and the request is
   proxied with streaming.

### 6.1 The pass-through rule (hard requirement)

An explicitly named model is proxied **unchanged**. If the chosen backend
does not have it, the backend's 404 is returned to the client as-is. The
dispatcher never silently substitutes a different model.

Only four rewrites exist, and each is logged with requested → served:

- a role name → the role's model;
- a bare backend name as the model (`primary`, `secondary`) → that
  backend's default model;
- a model-less request → the chosen backend's default model;
- `pin_default_model` modes → the chosen backend's default model.

Rationale, from production: the Python prototype's first version rewrote
any unrecognized model to the backend default, which silently invalidated
a week of two-model benchmark trials — both arms were served by the same
default model. This rule is the project's founding lesson and is covered
by tests (§11) that may not be weakened.

---

## 7. API surface

### 7.1 Proxy endpoints (to the routed backend)

- `POST /api/chat`, `POST /api/generate`, `POST /api/show`, `GET /api/ps`
- `POST /v1/chat/completions`
- `GET /api/tags` — merged, per §5.3 (dispatcher-answered)
- `GET /v1/models` — dispatcher-answered: both backends' default models,
  all backend names (aliases), and all role names
- `GET /api/version` — dispatcher-answered: `{version, mode}`

### 7.2 Admin endpoints (dispatcher-answered)

- `GET /health` → `{ok, mode, backends: {<name>: up|down}}`
- `GET /mode` → `{mode}` plus `expires_in_s` when the mode has a TTL
- `POST /mode` with `{"mode": "<name>"}` → sets and persists the mode.
  Unknown name → 400 with the list of configured modes.
- `GET /modes` → all configured modes with their assignments (the
  "which PC/GPU does each mode invoke" view).

### 7.3 CLI

The same binary doubles as the admin client:

```bash
gondola serve [--config dispatcher.yaml]
gondola mode            # show active mode
gondola mode dev        # switch
gondola modes           # table of modes → backends
gondola health
```

---

## 8. Failure behaviour

| Condition | Result |
|---|---|
| Routed backend down, mode forbids wake | 502, log line naming mode + backend |
| Wake attempted, backend still down at timeout | 502 after `wake_timeout` |
| Backend lacks the named model | Backend's 404, passed through untouched |
| Mode state file missing/corrupt | Start in `default_mode`, log a warning |
| Active mode's TTL expired | Atomically return to `default_mode` (checked per request) |
| Config invalid at startup | Exit non-zero with the offending field; never start half-configured |
| Upstream dies mid-stream | Stream terminates; log line with bytes served |

---

## 9. Logging

One line per request on stdout: time, mode, requested model, served model
(when different), backend, path, upstream status, time-to-first-byte.
Plus event lines: `MODE set`, `REWRITE`, `WAKE`, `ROUTE ... FAILED`.
Plain text by default; `--log-format json` for structured logs. No request
bodies are ever logged.

---

## 10. Security posture

The dispatcher is a LAN tool: the proxy surface has no auth in v1
and the docs must say plainly to bind it to trusted networks only, exactly
as Ollama itself is deployed. The admin surface (`POST /mode`) accepts an
optional `admin_token` (Bearer) when configured; the spec recommends
setting one whenever the listen address leaves the loopback. Mode changes
and token failures are logged.

---

## 11. Testing requirements

- **Routing matrix (unit)**: table-driven tests over mode × request class
  × header × family, asserting backend + served model — including every
  §6.1 pass-through case. A test that permits silent substitution fails
  the suite by design.
- **Mode lifecycle (unit)**: TTL expiry, corrupt state file, unknown mode
  rejection, per-role overrides.
- **Integration**: two fake Ollama backends (`httptest` servers) — one
  that 404s unknown models — verifying passthrough of the 404, streaming,
  tag merging without wake, and the wake-forbidden 502.
- **Config (unit)**: invalid configs fail startup with the field named.

---

## 12. Migration from the Python prototype

Same port, same endpoints, same four starter modes, same log vocabulary,
so existing clients (an agent pointing at `127.0.0.1:11435`) switch
by swapping the binary/container. Fleet-specific values (IPs, MAC, model
names) move from environment variables into `dispatcher.yaml`; the file
for the reference fleet is generated from the live v1.3 settings at cutover.

---

## 13. Decisions — resolved 2026-11-21

1. **Config**: YAML file primary, as specced in §4. (PoC: file + CLI
   flags; env field overrides deferred.)
2. **Starter modes**: home/away/test/dev ship as the example config;
   modes are fully user-definable, as specced in §4.3.
3. **Admin token**: optional; recommended whenever bound beyond loopback.
4. **Project name**: `gondola` — Go-Orchestrated Node Dispatch Over
   Local Area (final pick, 2026-11-22, after the working names
   `ollama-dispatcher` and `frog`).
5. **Adapters**: backend interface in v1, Ollama adapter only;
   llama.cpp/vLLM adapters deferred. A PAIR endpoint is an
   Ollama-shaped backend and can sit behind the dispatcher unchanged.

Addendum (same day, after NVIDIA PAIR research): PAIR (NVIDIA's Personal
AI Router, Go, v1.0) already does discovery and eligibility routing
across engines, but has no mode policy — no wake permissions, no
benchmark pinning, no roles. This project is therefore positioned as the
**mode/policy layer**, complementary to PAIR rather than competing with
it. Live model-availability checks and discovery (ideas surfaced via
parallel research) are future work, not v1.

---

## 14. Discovery — `gondola discover` (specified 2026-11-22; not yet built)

Status: specified at the owner's direction — spec first, build after. Not
part of v1 as shipped. This section takes up the discovery thread the §13
addendum deferred.

### 14.1 Purpose

Writing `dispatcher.yaml` by hand — backend URLs, Wake-on-LAN MACs,
broadcast addresses — is the main barrier to adopting gondola. Discovery
removes it: one command inventories the Ollama backends on the operator's
own LAN and drafts the config. The operator names machines and confirms
roles; discovery does the tedious 90%.

### 14.2 Command surface

```text
gondola discover [--timeout 300ms] [--port 11434] [--json]
gondola discover --write-config dispatcher.yaml [--force]
```

Default behaviour is report-only: nothing is written, nothing is changed.
`--write-config` writes a starter config to the given path and refuses to
overwrite an existing file unless `--force` is passed.

### 14.3 Scan scope — hard limits

- Only subnets directly attached to the host's up, non-loopback
  interfaces are scanned. Discovery never probes beyond the local
  network, and never the internet.
- Only the Ollama port is probed (default 11434, overridable with
  `--port`).
- Probing is concurrent with a bounded worker pool (default 64) and a
  short per-host dial timeout (default 300ms), so a /24 sweep completes
  in seconds and stays gentle on the LAN.

### 14.4 What it collects — from Ollama's own API, read-only

For each host that answers:

- reachability and probe latency;
- `GET /api/version` — the backend's Ollama version;
- `GET /api/tags` — installed models with sizes, parameter counts, and
  quantizations;
- `GET /api/ps` — currently loaded models and their VRAM footprint
  (`size_vram`);
- the host's MAC address, read from the OS neighbour (ARP) table after
  probing — the Wake-on-LAN input nobody knows by heart. If no neighbour
  entry exists (e.g. the backend sits across a VLAN), the field is
  omitted and the report says why;
- a hostname via reverse DNS / mDNS where the resolver provides one
  (best-effort; may be empty);
- the subnet broadcast address, computed from the interface netmask, for
  `wol_broadcasts`.

Discovery is strictly read-only against the network: it sends no
Wake-on-LAN packets, no POSTs, and never causes a model to load. It is a
CLI tool only — v1 adds no `/discover` endpoint to the proxy surface, so
the unauthenticated API surface (§10) is unchanged.

### 14.5 What it cannot know

Ollama's API exposes no host hardware: no GPU model, no total VRAM, no
CPU or RAM. Discovery therefore never invents hardware facts. Generated
configs use placeholder backend names (`backend-1`, `backend-2`, or the
discovered hostname when one resolves) and leave hardware description to
the operator.

### 14.6 Generated config rules

- One backend block per discovered host: `url`, `wol_mac` (when found),
  `wol_broadcasts` (computed per subnet).
- Each backend's `default_model` is set to its smallest installed model
  and marked with a `# guessed by discover — review` comment.
- Roles are emitted as a commented template (brain / heavy / coder /
  utility), never guessed: role quality is a judgement call, and a
  wrong-but-confident role map is worse than none.
- The file header states it was generated by `gondola discover`, when,
  and that it must be reviewed before use.
- Modes are emitted as the four starter modes from §4.3, pointing at the
  first discovered backend, ready to be edited.

### 14.7 Report and exit behaviour

The human report is a table: address, name, version, model count,
largest model, loaded models, MAC. `--json` emits the same data for
tooling. Exit code is 0 when at least one backend is found, 1 when none
is — with a hint listing the subnets that were scanned and the port that
was probed, so an empty result explains itself.

### 14.8 Testing requirements

- Subnet mathematics: broadcast computation and host enumeration,
  including /31 and /32 edge cases.
- Neighbour-table parsing against fixtures (Linux `/proc/net/arp`,
  macOS `arp -a` output).
- Config generation from fixture scan results, checked against a golden
  file.
- The read-only guarantee: a test backend that fails the test if any
  non-GET request arrives during a scan.

## 15. Web UI

Status: shipped in 0.3.0 (2026-11-22), after the owner's review of
this section and the code's technical summary.

### 15.1 Surface

- `GET /ui` — one embedded HTML page: current mode and mode switching,
  backend health, the recent-activity table, and the config editor.
- `GET /admin/activity` — the activity ring as JSON, newest first.
- `GET /admin/config` — the config file's text (see 15.3).
- `PUT /admin/config` — replace the config file's text (see 15.3).
- Mode switching from the page uses the existing `POST /mode` (§7).

### 15.2 Activity

Every proxied request records an entry (mode, requested model, served
model, backend, path, status, time-to-first-byte, whether a rewrite
applied), as do mode changes and config saves. The log is an in-memory
ring holding the most recent 300 entries: it is a live view of what the
dispatcher just delegated, not an audit trail, and it resets on
restart. The stdout log vocabulary (§11) is unchanged.

### 15.3 Config editing

- The editor works on the file the running instance was started with
  (`serve --config`). A save validates the new text through the real
  config loader first; invalid text is rejected with the loader's
  error and the file on disk is untouched. A valid save keeps the
  previous file as `<config>.bak` and replaces the file atomically.
- Config is loaded at startup (§8), so a saved config takes effect on
  restart; the API says so (`restart_required: true`) and the page
  repeats it. There is no hot reload.
- The admin token is the one secret a config can hold. `GET` replaces
  its value with `__REDACTED__`; `PUT` substitutes the running token
  back when the placeholder survives the round trip, so editing can
  neither leak nor clobber it. Typing a different value rotates the
  token, effective on restart like everything else.

### 15.4 Authentication

The `/ui` page shell is served openly, like `/health`. Every data
endpoint behind it (`/admin/*`, and `POST /mode` as before) follows
the §10 admin rule: open when no admin token is configured,
Bearer-token-gated when one is. The page asks for the token on a 401
and keeps it in `sessionStorage` only.

### 15.5 Tests

`webui_test.go` covers the ring's wrap and ordering, the admin gate
open and closed, token redaction on GET, rejection of an invalid save
(file and backup untouched), and a valid save's token round trip and
`.bak` backup.

## 16. Structured settings

Status: specified 2026-11-22 at the owner's direction and approved the
same day. Supersedes the raw config editor's placement in section
15.3: the console changes settings through structured controls, never
by editing config text as the primary path.

### 16.1 Principle

The config file is the dispatcher's source of truth, but the console
treats it as data, not text. Every change is a typed field edit,
validated through the real config loader before it reaches disk.
Hand-editing the file on the host remains the operator's escape
hatch.

### 16.2 Surface

- `GET /admin/settings` — the saved config as structured JSON
  (backends, roles, modes, default mode, server settings minus the
  token), plus, per backend, the model names that backend currently
  serves (live from its /api/tags, best-effort) to populate pickers.
- `PUT /admin/settings` — replace the saved config with a structured
  document of the same shape, plus a `renames` map (section 16.4).
  The server builds a Config from it, validates it through the same
  loader path as startup, and only then writes.
- The raw editor (GET/PUT /admin/config, section 15.3) stays, but
  leaves the main console: it moves behind an Advanced section at the
  bottom of the page, collapsed by default, revealed only by an
  explicit click, and headed with a warning that structured settings
  are the supported path. Its validation, .bak, and restart semantics
  are unchanged.

### 16.3 What the console can change

- Backends: rename; URL; default model (picker from that backend's
  live models, free text allowed); MAC; WoL broadcasts; model
  prefixes.
- Roles: default model; per-backend override models.
- Modes: default backend (picker); wake; pin-default-model; TTL;
  per-role backend assignments (pickers).
- Fleet: default mode (picker); server wake timeout.
- The admin token is not a settings field — it stays settable only in
  the file itself, so the console can never lock anyone out of it. A
  settings save always preserves the token currently on disk.

### 16.4 Rename

Renaming a backend rewrites every reference in the same save: the
backends key, every mode's default, every mode's role assignments,
and every role's per-backend overrides. The new name must be
non-empty, use only letters, digits, dashes, and underscores, and
collide with no existing backend or role name. A rename is a breaking
change for anything addressing the old name (bare-alias model
requests, X-Backend headers) and, like all settings, applies on
restart.

### 16.5 Saving

Same guarantees as section 15.3: validate first, and an invalid
change writes nothing; the previous file is kept as .bak; the replace
is atomic where the filesystem allows it — when the config is a
single-file bind mount (a common Docker shape) renaming over it is
impossible, so the writer falls back to replacing the contents in
place (the content was validated before the write, and the .bak is
the recovery path); the response reports restart_required. One difference: the
server renders the file as canonical YAML from the structured
document, so comments in a hand-maintained file do not survive a
console save (the .bak preserves them).

### 16.6 Tests

Rename rewrites all references and rejects collisions; a settings
round trip (GET then PUT unchanged) reproduces an equivalent config
with the on-disk token preserved; an invalid settings document is
rejected with the file untouched; mode classification and summaries
match the routing rules.

### 16.7 Say what each mode does

The console derives, from the config alone, what every mode means for
every device, and shows it wherever a mode is displayed or edited:

- Per-device classification in a mode: **Default** (serves all
  traffic the mode does not assign elsewhere), **Role-assigned**
  (serves the named roles, listed), **On-call** (not assigned, but
  reachable when a request names one of its model families or a
  header asks for it; the display states whether this mode would wake
  it if asleep), **Ignored** (not assigned and no family pins reach
  it; this mode will not use the device).
- A one-line summary per mode, generated server-side from its actual
  assignments, including pin and TTL behaviour where set.
- A role table per mode: each role, the device that serves it, and
  the model it resolves to there (per-backend overrides applied).
- In the settings editor, a device no mode references at all is
  flagged "unused by every mode".

The classification is computed server-side from the config and served
with the settings and modes payloads, so every client states the same
truth; the page never re-derives routing logic in JavaScript.

## 17. Applying saved settings

The console edits the saved config; the running dispatcher serves the
config it started with. This section closes that gap.

### 17.1 Restart-pending detection

The server fingerprints a config as the SHA-256 of the canonical JSON
of its settings document (section 16) plus the admin token. Canonical
JSON makes the fingerprint immune to file formatting: only a real
change — a model, a mode, a rename, a token rotation — can differ.
`GET /admin/settings` reports `restart_pending`, true when the saved
config's fingerprint differs from the running config's.

### 17.2 Restart endpoint

`POST /admin/restart` (admin-gated, section 10) applies the saved
config:

1. The saved config is loaded through the real loader. If it does not
   load, the endpoint answers 400 and nothing happens — a config that
   would fail at startup can never take the dispatcher down.
2. Otherwise it answers 200 and, after a brief delay to let the
   response reach the client, re-executes the process in place:
   `syscall.Exec` with the same binary, arguments, and environment.
   The PID is unchanged, so in the Docker deployment (the dispatcher
   is the container's PID 1) the container itself never restarts.
3. The current mode survives: it lives in the state file (section 8),
   which the new process image reads at startup like any other start.

### 17.3 Console behaviour

The banner appears whenever `restart_pending` is true — on page load
and immediately after any save (structured or raw) — with a restart
button. Clicking it calls the endpoint, polls `/health` until the
dispatcher answers again, and reloads the page.

### 17.4 Tests

The fingerprint is stable across formatting and changes on any real
edit; pending flips when the file on disk diverges; the restart
endpoint refuses an unloadable saved config and does not exec; the
endpoint is token-gated. The exec itself is behind an injectable
function so tests can observe it without re-executing.

## 18. Wake-ahead

When a mode is set explicitly (`POST /mode` or the console), the
dispatcher immediately sends one Wake-on-LAN packet to each machine
the new mode can route to — its default plus its role assignments —
that is asleep and has a MAC configured, provided the mode allows
waking. Machines already answering are left alone; machines the mode
forbids waking are never touched. Wake-ahead does not wait for the
machines to come up: requests still pass through `ensureUp`
(section 5.2), which waits and retries as before. It does not run at
startup or when a TTL expires into the default mode — only on an
explicit switch, because the switch is the operator's statement of
intent. Each wake-ahead is logged and recorded in the activity feed
as kind `wake`.

## 19. Embeddings endpoints

`POST /api/embed`, `POST /api/embeddings`, and `POST /v1/embeddings`
are proxied exactly like chat: the body's model field goes through
the same resolution (section 6) — roles resolve, explicit models pass
through untouched — and the response streams back as-is. Any Ollama
or OpenAI-compatible client can use the dispatcher for retrieval
workloads, not just generation.

## 20. Live throughput

The proxy observes response counters while streaming; the byte
stream is never altered. Ollama-native responses carry `eval_count`
and `eval_duration` in the final chunk, giving exact tokens per
second. OpenAI-compatible responses report token usage but no
durations, so their tok/s is estimated — completion tokens over the
measured generation window (first byte to last byte; a single-flush
response's window is its total time) —
and marked approximate everywhere it appears, so production traffic
is never dressed up as bench data. When a stream completes, its
route entry gains `eval_count`, `prompt_eval_count`, `tok_per_sec`,
and `tok_per_sec_approx`, and its `ms` becomes the total duration
(an aborted request keeps its time-to-headers). `GET
/admin/activity` also returns a throughput summary: average tok/s
per backend and model over the current ring. Everything lives in
the ring — a live view that resets on restart, not an audit trail
(section 15).

## 21. Input preview

So a feed row can be matched to the request that produced it, each
route entry also carries `input`: a short preview of what was asked.
The router already parses the request body for its model field
(section 6); the preview is one more field kept from that same parse.
Chat-shaped bodies yield the text of the last user message (system
prompts are never included); generate-shaped bodies yield the
prompt; anything else yields an empty preview. Content parts in an
array are concatenated. The preview is whitespace-collapsed and
capped at 160 characters. It lives only in the in-memory activity
ring (section 15) — never written to disk, gone on restart — and the
console shows it beside the requested and served models. The body
forwarded to the backend stays byte-identical; like section 20's
counters, this is read-only observation.

## 23. OpenAI-dialect backends (specified and built 2026-11-23)

Motivation: the Anton A/B (identical heavy fix job, both engines)
measured Ollama serving Qwen3.6-35B-A3B Q4_K_M at 15/15 quality,
40 tok/s generation, 281/263 s wall, against PolyStrata serving
the same model at UD-Q3_K_M: 15/15, 128/126 tok/s generation,
35/49 s wall. PolyStrata speaks the OpenAI API; gondola's backends
spoke only Ollama's. This section adds the second dialect so a
role can be served by an OpenAI-compatible backend where it wins.
Brain, utility, and coder stay on Ollama — PolyStrata is an MoE
engine with no build for those dense models.

23.1 A backend gains `dialect`: `"ollama"` (the default when
absent; existing configuration is untouched) or `"openai"`. Any
other value is a config error. OpenAI backends accept two further
optional fields: `api_key` (gondola's Bearer credential to the
backend) and `max_context` (informational, shown in the console;
gondola does not enforce it — an openai backend's context length
is its launch property, not a per-request option).

23.2 For an openai backend, `POST /v1/chat/completions` forwards
to the backend's own endpoint, byte-identical except the model
field, which follows the same resolution as sections 6 and 19 —
roles resolve, explicit models pass through untouched, a backend
404 returns loudly. A configured `api_key` sets the Authorization
header, replacing any client-supplied one. No Ollama-specific
fields are injected. Any other path routed to an openai backend
fails loudly with an error naming the dialect mismatch — never a
silent translation. Embeddings remain Ollama-dialect only.

23.3 Streams pass through unaltered. Throughput (section 20)
follows its OpenAI rule — usage-based estimate, marked
approximate — except when a non-streamed response carries a
server `timings` object (Strata sends one): its
`predicted_per_second` is recorded as exact.

23.4 `ensureUp` probes per dialect: Ollama `GET /api/version`,
openai `GET /health`; up is HTTP 200 whether or not a model is
loaded (a Strata server that idle-unloaded its model is up; the
chat request triggers the reload). Wake-on-LAN is host-level and
unchanged; wake-ahead uses the same per-dialect probe.

23.5 The settings page lists an openai backend's models from its
`/v1/models`; the console shows a backend's dialect in the mode
overview and edits dialect, api_key, and max_context alongside
the other backend fields.

Deployment on Anton (server side): the PolyStrata server runs the
UD-Q3_K_M model at 65536 context (Hermes's floor; verified by
re-running the A/B harness direct against the 64K config before
wiring: 15/15, generation above the Ollama floor), bound to the
LAN address with an install-generated API key and
`--idle-unload 300`, so the card returns to the brain between
heavy jobs. Rollout: a backend `AntonStrata` serves test mode's
heavy first (Phase 1); home heavy's Anton target switches through
settings alone only on the operator's word after Phase 1 evidence
(Phase 2), with Ollama's build kept installed as the instant
rollback. Non-goals: moving brain/utility/coder, multi-model
Strata serving, Jarvis's Strata, embeddings through Strata.
