"""Routing-core tests: pick_backend and resolve are pure functions, so the
whole routing matrix is testable without any Ollama backend."""

import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

os.environ.setdefault("MODE_FILE", "/nonexistent-dir/mode")

import app


# ------------------------------------------------------------- pick_backend

def test_test_mode_pins_primary():
    assert app.pick_backend("qwen3:14b", None, "test") == "primary"
    assert app.pick_backend(None, "secondary", "test") == "primary"


def test_header_wins_over_model_and_mode():
    assert app.pick_backend("qwen3.5:9b", "secondary", "home") == "secondary"
    assert app.pick_backend("qwen3:14b", "primary", "away") == "primary"


def test_model_family_pinning():
    assert app.pick_backend("qwen3:14b", None, "home") == "secondary"
    assert app.pick_backend("qwen3.5:9b", None, "away") == "primary"
    assert app.pick_backend("qwen2.5-coder:7b", None, "away") == "primary"


def test_mode_default_for_unknown_models():
    assert app.pick_backend("some-other-model", None, "home") == "primary"
    assert app.pick_backend("some-other-model", None, "away") == "secondary"
    assert app.pick_backend("some-other-model", None, "dev") == "secondary"


# ------------------------------------------------------------- resolve

def test_explicit_model_passes_through_untouched():
    # The core guarantee: a named model is served as named, or 404s loudly.
    assert app.resolve("qwen3.6:35b-a3b", None, "home") == ("primary", "qwen3.6:35b-a3b")
    assert app.resolve("hf.co/example/some-model:Q4", None, "home") == (
        "primary",
        "hf.co/example/some-model:Q4",
    )


def test_bare_aliases_resolve_to_backend_defaults():
    assert app.resolve("secondary", None, "home") == ("secondary", app.SECONDARY_MODEL)
    assert app.resolve("primary", None, "away") == ("primary", app.PRIMARY_MODEL)


def test_model_less_request_gets_backend_default():
    assert app.resolve(None, None, "home") == ("primary", app.PRIMARY_MODEL)
    assert app.resolve(None, None, "away") == ("secondary", app.SECONDARY_MODEL)


def test_test_mode_pin_rewrites_to_primary_default():
    assert app.resolve("qwen3.6:35b-a3b", None, "test") == ("primary", app.PRIMARY_MODEL)
    # ...but a genuine primary model is left alone.
    assert app.resolve("qwen3.5:9b", None, "test") == ("primary", "qwen3.5:9b")


def test_roles_resolve_per_mode():
    assert app.resolve("heavy", None, "home") == ("primary", app.ROLES["heavy"])
    assert app.resolve("heavy", None, "test") == ("primary", app.ROLES["heavy"])
    assert app.resolve("heavy", None, "away") == ("secondary", app.ROLES["heavy"])
    assert app.resolve("heavy", None, "dev") == ("secondary", app.ROLES["heavy"])
    assert app.resolve("brain", None, "home") == ("primary", app.ROLES["brain"])
    assert app.resolve("coder", None, "dev") == ("secondary", app.ROLES["coder"])
    assert app.resolve("utility", None, "home") == ("primary", app.ROLES["utility"])


# ------------------------------------------------------------- api surface

def test_api_surface():
    from fastapi.testclient import TestClient

    client = TestClient(app.app)
    v = client.get("/api/version")
    assert v.status_code == 200 and v.json()["version"] == app.VERSION

    models = client.get("/v1/models").json()["data"]
    ids = {m["id"] for m in models}
    assert {"primary", "secondary", "brain", "heavy", "coder", "utility"} <= ids

    bad = client.post("/mode", json={"mode": "bogus"})
    assert bad.status_code == 400
