# ollama-dispatcher

A mode-aware routing proxy for **two Ollama machines**: an always-on
**primary** (a home server) and a **secondary** that sleeps when idle (a
gaming PC), woken over Wake-on-LAN only when a request actually needs it.

Point your agent, app, or OpenAI-compatible client at one endpoint; the
dispatcher decides which machine serves each request and wakes the
secondary when required.

## Why

Two common home-lab shapes don't fit a single Ollama host:

- The always-on box is power-efficient but has a small GPU — fine for a
  9B operator model, wrong for 35B work.
- The big GPU lives in a machine that should sleep most of the day.

The dispatcher lets a client ask for a *role* (`brain`, `heavy`, `coder`,
`utility`) or a specific model, and applies a *mode* — where you are and
what the machines should be doing — to pick the backend.

## Modes

| Mode   | Behaviour |
|--------|-----------|
| `home` | Primary by default; the secondary is used only when a request names one of its models. |
| `away` | Secondary by default (it's the stronger machine; it may stay awake for the window). |
| `test` | Everything pinned to the primary and rewritten to the primary's default model. The secondary is never woken. For benchmarks you don't want contaminated. |
| `dev`  | Secondary by default, wake allowed, full route logging. Auto-expires back to `home` after `DEV_TTL_HOURS` (default 8). |

Set the mode at runtime — it persists in a small JSON state file:

```bash
curl -X POST http://localhost:11435/mode -H 'Content-Type: application/json' -d '{"mode": "away"}'
curl http://localhost:11435/mode
curl http://localhost:11435/health
```

## Routing rules (in order)

1. **Role names** (`brain`, `heavy`, `coder`, `utility`) resolve to the
   configured model for that role, on the mode's preferred machine.
2. **`X-Backend: primary|secondary`** header forces a backend.
3. **Model families** configured as primary/secondary prefixes pin the
   request to that machine.
4. **Mode default**: `home`/`test` → primary, `away`/`dev` → secondary.
5. **Bare aliases**: asking for the model `primary` or `secondary` serves
   that machine's configured default model.

### The one rule that matters most

An explicitly named model is **passed through untouched**. If the chosen
backend doesn't have it, Ollama's 404 comes back to the client — loudly.
The dispatcher never silently substitutes a different model.

An earlier version of this code rewrote any unrecognized model name to
the backend default. It benchmarked two models against each other for a
week before anyone noticed both arms were being served by the same
default model the whole time. Only four things are ever rewritten now —
role names, bare aliases, model-less requests, and the explicit test-mode
pin — and every rewrite is written to the log.

## Endpoints

Ollama-native: `POST /api/chat`, `POST /api/generate`, `POST /api/show`,
`GET /api/ps`, `GET /api/tags` (merged from both backends, without waking
the secondary), `GET /api/version`.

OpenAI-compatible: `POST /v1/chat/completions`, `GET /v1/models`
(advertises both defaults, both aliases, and all four roles).

Plus `GET /health`, `GET|POST /mode`. Streaming responses are passed
through as-is.

## Run it

```bash
cp .env.example .env   # set your two Ollama URLs, models, and the WoL MAC
docker compose up -d --build
```

Host networking is used so Wake-on-LAN broadcasts reach the LAN. The
secondary is only ever woken, never slept — putting it back to sleep is
the operating system's job (idle sleep timers work well).

Or without Docker:

```bash
pip install -r requirements.txt
PRIMARY_URL=http://192.168.1.10:11434 SECONDARY_URL=http://192.168.1.20:11434 \
  uvicorn app:app --host 0.0.0.0 --port 11435
```

## Configuration

Everything is environment variables — see `.env.example` for the full
list: backend URLs and default models, the WoL MAC and broadcast
addresses, model-family prefixes, the four role models, the mode-file
path, the dev-mode TTL, and the wake timeout.

## Tests

```bash
pip install pytest fastapi httpx
pytest
```

The routing core (`pick_backend`, `resolve`) is pure, so the full routing
matrix — including the pass-through guarantee — is covered without any
Ollama backend running.

## License

MIT
