"""ollama-dispatcher: a mode-aware routing proxy for two Ollama backends.

Sits in front of an always-on "primary" machine and a wakeable "secondary"
machine (Wake-on-LAN) and routes each request by mode, model name, role
alias, or an explicit X-Backend header. Ollama-native (/api/*) and
OpenAI-compatible (/v1/*) endpoints are proxied, streaming included.

Design rule learned the hard way: an explicitly named model is passed
through untouched. If the chosen backend does not have it, Ollama's 404
surfaces loudly - the dispatcher never silently substitutes a different
model (an earlier version did, and quietly invalidated a week of model
benchmarks). Only bare aliases, model-less requests, role names, and the
explicit test-mode pin are rewritten, and every rewrite is logged.
"""

import asyncio
import json
import os
import socket
import time

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse

PRIMARY = os.environ.get("PRIMARY_URL", "http://127.0.0.1:11434")
SECONDARY = os.environ.get("SECONDARY_URL", "http://127.0.0.1:11435")
SECONDARY_MAC = os.environ.get("SECONDARY_MAC", "")
WOL_BROADCASTS = [
    a.strip()
    for a in os.environ.get("WOL_BROADCASTS", "255.255.255.255").split(",")
    if a.strip()
]

PRIMARY_MODEL = os.environ.get("PRIMARY_MODEL", "qwen3.5:9b")
SECONDARY_MODEL = os.environ.get("SECONDARY_MODEL", "qwen3:14b")

# Model-name prefixes that pin a request to a backend, for model families
# that only exist (or only make sense) on one machine.
SECONDARY_MODEL_PREFIXES = [
    p.strip()
    for p in os.environ.get("SECONDARY_MODEL_PREFIXES", "qwen3:14b").split(",")
    if p.strip()
]
PRIMARY_MODEL_PREFIXES = [
    p.strip()
    for p in os.environ.get("PRIMARY_MODEL_PREFIXES", "qwen2.5-coder").split(",")
    if p.strip()
]

# Role aliases: a role names a job; the mode names the machine preference
# (home/test -> primary, away/dev -> secondary). Resolved in resolve().
ROLES = {
    "brain": os.environ.get("ROLE_BRAIN", PRIMARY_MODEL),
    "heavy": os.environ.get("ROLE_HEAVY", "qwen3.6:35b-a3b"),
    "coder": os.environ.get("ROLE_CODER", "qwen3-coder:30b"),
    "utility": os.environ.get("ROLE_UTILITY", "qwen3.5:4b"),
}

MODE_FILE = os.environ.get("MODE_FILE", "/data/mode")
WAKE_TIMEOUT = int(os.environ.get("WAKE_TIMEOUT", "150"))
DEV_TTL = int(os.environ.get("DEV_TTL_HOURS", "8")) * 3600
MODES = ("home", "away", "test", "dev")
VERSION = "1.3.0"

app = FastAPI(title="ollama-dispatcher")


# ---------------------------------------------------------------- modes

def _read_mode():
    try:
        with open(MODE_FILE) as f:
            d = json.load(f)
        return d.get("mode", "home"), float(d.get("set_at", 0))
    except Exception:
        return "home", 0.0


def get_mode():
    m, set_at = _read_mode()
    if m not in MODES:
        return "home"
    if m == "dev" and set_at and (time.time() - set_at > DEV_TTL):
        set_mode("home")
        return "home"
    return m


def set_mode(m):
    with open(MODE_FILE, "w") as f:
        json.dump({"mode": m, "set_at": time.time()}, f)


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


# ---------------------------------------------------------------- wake

def wol(mac):
    parts = mac.replace(":", "").replace("-", "")
    magic = b"\xff" * 6 + bytes.fromhex(parts) * 16
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    for addr in WOL_BROADCASTS:
        try:
            s.sendto(magic, (addr, 9))
        except Exception:
            pass
    s.close()


async def reachable(base, timeout=2.0):
    try:
        async with httpx.AsyncClient(timeout=timeout) as c:
            r = await c.get(base + "/api/version")
            return r.status_code == 200
    except Exception:
        return False


_wake_lock = asyncio.Lock()


async def ensure_secondary():
    if await reachable(SECONDARY):
        return True
    if not SECONDARY_MAC:
        return False
    async with _wake_lock:
        if await reachable(SECONDARY):
            return True
        wol(SECONDARY_MAC)
        start = time.time()
        while time.time() - start < WAKE_TIMEOUT:
            await asyncio.sleep(5)
            if await reachable(SECONDARY, 3.0):
                return True
            if int(time.time() - start) % 30 < 5:
                wol(SECONDARY_MAC)
        return False


# ---------------------------------------------------------------- routing

def is_secondary_model(model):
    return bool(model) and (
        model == "secondary"
        or model.startswith(SECONDARY_MODEL)
        or any(model.startswith(p) for p in SECONDARY_MODEL_PREFIXES)
    )


def is_primary_model(model):
    return bool(model) and (
        model == "primary"
        or model.startswith(PRIMARY_MODEL)
        or any(model.startswith(p) for p in PRIMARY_MODEL_PREFIXES)
    )


def pick_backend(model, header, mode):
    if mode == "test":
        return "primary"
    if header in ("primary", "secondary"):
        return header
    if is_secondary_model(model):
        return "secondary"
    if is_primary_model(model):
        return "primary"
    if mode in ("away", "dev"):
        return "secondary"
    return "primary"


def resolve(model, header, mode):
    """Return (backend, served_model) for a request.

    Only four things are ever rewritten, and each is deliberate: a role
    name, a bare backend alias ("primary"/"secondary"), a model-less
    request, and the test-mode pin. Every other explicit model name is
    passed through exactly as given.
    """
    if model in ROLES:
        backend = "primary" if mode in ("home", "test") else "secondary"
        return backend, ROLES[model]
    backend = pick_backend(model, header, mode)
    if model == "secondary":
        return backend, SECONDARY_MODEL
    if model == "primary":
        return backend, PRIMARY_MODEL
    if not model:
        return backend, SECONDARY_MODEL if backend == "secondary" else PRIMARY_MODEL
    if mode == "test" and backend == "primary" and not is_primary_model(model):
        return backend, PRIMARY_MODEL
    return backend, model


# ---------------------------------------------------------------- api

@app.get("/health")
async def health():
    return {
        "ok": True,
        "mode": get_mode(),
        "primary_up": await reachable(PRIMARY),
        "secondary_up": await reachable(SECONDARY),
    }


@app.get("/mode")
async def mode_get():
    _, set_at = _read_mode()
    eff = get_mode()
    out = {"mode": eff}
    if eff == "dev" and set_at:
        out["dev_expires_in_s"] = int(DEV_TTL - (time.time() - set_at))
    return out


@app.post("/mode")
async def mode_post(req: Request):
    body = await req.json()
    m = body.get("mode", "")
    if m in MODES:
        set_mode(m)
        log(f"MODE set -> {m}")
        return {"mode": m}
    return JSONResponse(
        {"error": "mode must be one of " + "|".join(MODES)}, status_code=400
    )


@app.get("/api/tags")
async def tags():
    out = {"models": []}
    for base in (PRIMARY, SECONDARY):
        try:
            async with httpx.AsyncClient(timeout=3) as c:
                r = await c.get(base + "/api/tags")
                if r.status_code == 200:
                    out["models"].extend(r.json().get("models", []))
        except Exception:
            pass
    return out


async def proxy(req: Request, path: str):
    t0 = time.time()
    mode = get_mode()
    raw = await req.body()
    model = None
    try:
        model = json.loads(raw).get("model")
    except Exception:
        pass
    backend, served = resolve(model, req.headers.get("x-backend"), mode)
    if served != model:
        log(f"REWRITE model={model} -> {served} (backend={backend} mode={mode})")
        try:
            b = json.loads(raw)
            b["model"] = served
            raw = json.dumps(b).encode()
        except Exception:
            pass
    if backend == "secondary":
        if not await ensure_secondary():
            log(f"ROUTE mode={mode} model={model} -> secondary FAILED (unreachable after wake)")
            return JSONResponse(
                {"error": "secondary unreachable after wake attempt"},
                status_code=502,
            )
        base = SECONDARY
    else:
        base = PRIMARY
    headers = {
        k: v
        for k, v in req.headers.items()
        if k.lower() not in ("host", "content-length", "x-backend")
    }
    client = httpx.AsyncClient(timeout=None)
    try:
        upstream = await client.send(
            client.build_request(req.method, base + path, content=raw, headers=headers),
            stream=True,
        )
    except Exception as e:
        await client.aclose()
        log(f"ROUTE mode={mode} model={model} -> {backend} CONNECT-FAIL {e}")
        return JSONResponse({"error": f"backend connect failed: {e}"}, status_code=502)
    log(
        f"ROUTE mode={mode} model={model} -> {backend} path={path} "
        f"status={upstream.status_code} ttfb_ms={int((time.time() - t0) * 1000)}"
    )

    async def gen():
        try:
            async for chunk in upstream.aiter_raw():
                yield chunk
        finally:
            await upstream.aclose()
            await client.aclose()

    resp_headers = {
        k: v
        for k, v in upstream.headers.items()
        if k.lower() not in ("content-length", "transfer-encoding", "connection")
    }
    return StreamingResponse(gen(), status_code=upstream.status_code, headers=resp_headers)


@app.api_route("/api/chat", methods=["POST"])
async def api_chat(req: Request):
    return await proxy(req, "/api/chat")


@app.api_route("/api/generate", methods=["POST"])
async def api_generate(req: Request):
    return await proxy(req, "/api/generate")


@app.api_route("/api/show", methods=["POST"])
async def api_show(req: Request):
    return await proxy(req, "/api/show")


@app.api_route("/v1/chat/completions", methods=["POST"])
async def v1_chat(req: Request):
    return await proxy(req, "/v1/chat/completions")


@app.api_route("/v1/models", methods=["GET"])
async def v1_models():
    data = [
        {"id": m, "object": "model"}
        for m in [PRIMARY_MODEL, SECONDARY_MODEL, "primary", "secondary"]
        + list(ROLES.keys())
    ]
    return {"object": "list", "data": data}


@app.api_route("/api/version", methods=["GET"])
async def api_version():
    return {"version": VERSION, "mode": get_mode()}


@app.api_route("/api/ps", methods=["GET"])
async def api_ps(req: Request):
    return await proxy(req, "/api/ps")
