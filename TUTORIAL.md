# Routing your agent through gondola

Gondola sits between your agent and your GPU machines. The agent
keeps talking to one OpenAI-compatible endpoint; gondola decides
which machine — and which model — actually serves each request,
based on the **mode** you set and the **role** the request names.

This tutorial wires up [Hermes Agent](https://github.com/NousResearch)
as the main example, then covers any other OpenAI-compatible client.

## Prerequisites

- Gondola running (see the README) with a config that defines your
  backends, roles, and modes. A minimal shape:

```yaml
backends:
  big-box:
    url: http://192.168.1.50:11434   # Ollama
    default_model: qwen3:14b
  always-on:
    url: http://192.168.1.60:11434
    default_model: qwen3.5:9b

roles:
  brain: qwen3.5:9b        # everyday work
  heavy: qwen3:32b         # hard reasoning
  coder: qwen3-coder:30b   # code
  utility: qwen3.5:4b      # cheap and quick

default_mode: home
modes:
  home:
    default: always-on
```

- Your agent pointed at gondola's address, e.g.
  `http://192.168.1.70:11435/v1`.

Two ideas carry everything below:

- **Roles are model names.** A request that names `heavy` is served
  by whatever machine and model the current mode assigns to `heavy`.
  A request that names an exact model is passed through untouched —
  gondola never silently substitutes a different model.
- **Modes are policy.** Switching modes changes which machines serve
  and whether sleeping machines may be woken. Clients don't change
  modes; you do, in the console or with the admin API.

## Any OpenAI-compatible client

Point the client's base URL at gondola and pick a model:

```bash
curl http://192.168.1.70:11435/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "heavy",
       "messages": [{"role": "user", "content": "Plan a migration."}]}'
```

- **Base URL:** `http://<gondola-host>:11435/v1`
- **API key:** anything non-empty — gondola doesn't check client keys
  (protect the endpoint with the admin token and your network, below).
- **Model:** a role name (`brain`, `heavy`, `coder`, `utility`, or
  your own) or any exact model name the backends serve.

Open WebUI, Continue, aider, and friends all work this way: add an
OpenAI connection with that base URL and choose a role as the model.

## Hermes Agent

### 1. Point Hermes at gondola

In Hermes's `config.yaml`:

```yaml
model:
  provider: "custom"
  base_url: "http://192.168.1.70:11435/v1"
  default: "brain"
```

Setting the default to a role means Hermes's own turns follow the
mode: at home they land on the always-on box; in an away mode they
can land on the big GPU. An exact model name works too — it just
pins you to that model wherever it lives.

### 2. Switch strength mid-chat

A Hermes session's model is fixed when the session starts, but you
can switch it live:

```text
/model heavy     # this session now runs on the heavy role
/model coder     # ...or the coder role
/model brain     # ...and back
```

Every later turn in that session requests the role you named, and
gondola resolves it against the current mode.

### 3. Let Hermes pick the strength per subtask

Hermes's `delegate_task` spawns subagents. With the
**advanced-delegation** plugin, each subtask can ride a different
route — and the routes can be gondola's roles, so the delegating
agent chooses how much GPU a subtask deserves:

```bash
hermes plugins install advanced-delegation
hermes plugins enable advanced-delegation
```

Then in `config.yaml`:

```yaml
delegation:
  backend: "model_map"
  backends:
    model_map:
      options:
        - name: brain
          description: "Everyday work: conversation, status checks, lookups, summaries, simple questions. The default for most subtasks."
          provider: "custom"
          model: "brain"
        - name: heavy
          description: "Hard reasoning: planning, debugging, design, analysis, math - anything a small model might get wrong. Slow and careful."
          provider: "custom"
          model: "heavy"
        - name: coder
          description: "Writing and refactoring code; running tests and builds."
          provider: "custom"
          model: "coder"
        - name: utility
          description: "Cheap and quick: bulk extraction, classification, formatting, simple edits."
          provider: "custom"
          model: "utility"
      default: brain
```

Only the route names and descriptions reach the model — those
one-liners are how the agent decides. Write them for the way you
work. Ask for a delegation in chat ("delegate this with the heavy
option: …") and the subagent runs on that role.

One honest caveat: the delegating model has to actually pass the
option. If a delegation lands on the default route when you asked
for another, check the console (below) — the feed shows what was
really requested — and name the option explicitly.

### 4. Verify in the console

Open `http://<gondola-host>:11435/ui`. The activity feed shows every
routed request: what was **input**, which model was **requested**,
what **served** it, on which **backend**, and at what speed. If a
request didn't go where you expected, the feed is the ground truth —
not the agent's account of it.

## Set an admin token

The console's data endpoints, the mode switch, settings, and restart
are gated by an admin token **if one is configured** — and open to
your LAN if one isn't. Set one in your config:

```yaml
server:
  admin_token: "a-long-random-string"
```

The console will ask for it once and remember it. Mode changes are
meant to be a human decision; the token is what keeps them that way.

## Gotchas

- **A session keeps its model.** Restart the session (or use
  `/model`) after changing Hermes's default — running sessions don't
  re-read config.
- **Roles bypass mode pins.** If a mode pins non-role requests to a
  default model, role requests still resolve by role. That's by
  design: naming a role is an explicit choice.
- **Sleeping backends wake on work** when the mode allows waking and
  the backend has a MAC configured; the first request just takes
  longer.
- **Embeddings route too:** `POST /v1/embeddings` resolves models
  and roles exactly like chat.
