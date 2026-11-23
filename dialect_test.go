package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Section 23 tests: an openai-dialect backend serves chat
// completions through the same model resolution as Ollama backends,
// gondola's api_key replaces any client Authorization, Ollama-native
// paths to an openai backend fail loudly, server-reported timings
// are exact throughput, and readiness probes speak each backend's
// own dialect.

func dialectDispatcher(t *testing.T, upURL string) *Dispatcher {
	t.Helper()
	cfgText := `
backends:
  strata:
    url: %s
    default_model: strata-model
    dialect: openai
    api_key: test-key
    max_context: 65536
roles:
  heavy:
    default: ollama-heavy
    overrides: {strata: strata-model}
default_mode: home
modes:
  home:
    default: strata
server:
  listen: 127.0.0.1:0
  state_file: %s
`
	cfgText = strings.Replace(cfgText, "%s", upURL, 1)
	cfgText = strings.Replace(cfgText, "%s", filepath.Join(t.TempDir(), "mode.json"), 1)
	cfg, err := parseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	return NewDispatcher(cfg)
}

func TestOpenAIProxyRoleRewriteAndAuth(t *testing.T) {
	var gotPath, gotModel, gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		gotModel = body.Model
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
			"timings": map[string]any{"prompt_n": 10, "predicted_n": 5, "predicted_per_second": 128.3},
		})
	}))
	defer backend.Close()

	d := dialectDispatcher(t, backend.URL)
	handler := d.Routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"heavy","messages":[{"role":"user","content":"x"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != "strata-model" {
		t.Fatalf("upstream model = %q, want strata-model (role override)", gotModel)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("upstream Authorization = %q, want gondola's key to replace the client's", gotAuth)
	}
}

func TestOpenAIProxyPassThrough(t *testing.T) {
	var gotModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		gotModel = body.Model
		_ = json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}})
	}))
	defer backend.Close()

	d := dialectDispatcher(t, backend.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"explicit-name","messages":[]}`))
	rec := httptest.NewRecorder()
	d.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", rec.Code, rec.Body)
	}
	if gotModel != "explicit-name" {
		t.Fatalf("upstream model = %q, want pass-through of explicit-name", gotModel)
	}
}

func TestDialectMismatchIsLoud(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		t.Errorf("upstream must not be called for a mismatched path, got %s", r.URL.Path)
	}))
	defer backend.Close()

	d := dialectDispatcher(t, backend.URL)
	handler := d.Routes()
	for _, path := range []string{"/api/chat", "/api/generate", "/v1/embeddings"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"heavy"}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400 (%s)", path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "openai") {
			t.Fatalf("%s: error should name the dialect: %s", path, rec.Body)
		}
	}
}

func TestDialectValidation(t *testing.T) {
	base := `
backends:
  b:
    url: http://127.0.0.1:1
    default_model: m
    dialect: %s
roles:
  heavy: hm
default_mode: home
modes:
  home:
    default: b
server:
  listen: 127.0.0.1:0
  state_file: %s
`
	state := filepath.Join(t.TempDir(), "mode.json")
	for _, dialect := range []string{"ollama", "openai"} {
		cfg := strings.Replace(strings.Replace(base, "%s", dialect, 1), "%s", state, 1)
		if _, err := parseConfig([]byte(cfg)); err != nil {
			t.Fatalf("dialect %q should parse: %v", dialect, err)
		}
	}
	cfg := strings.Replace(strings.Replace(base, "%s", "bogus", 1), "%s", state, 1)
	if _, err := parseConfig([]byte(cfg)); err == nil {
		t.Fatal("dialect bogus should be rejected")
	}
}

func TestThroughputTimingsExact(t *testing.T) {
	tail := []byte(`{"usage":{"prompt_tokens":1576,"completion_tokens":4055},"timings":{"prompt_n":1576,"predicted_n":4055,"predicted_per_second":128.3}}`)
	tp := observeThroughput("/v1/chat/completions", tail, 34400, 2900)
	if tp.approx {
		t.Fatal("server timings must be recorded as exact, not approximate")
	}
	if tp.tps != 128.3 {
		t.Fatalf("tps = %v, want 128.3 from server timings", tp.tps)
	}
	if tp.evalCount != 4055 || tp.promptEvalCount != 1576 {
		t.Fatalf("counts = %d/%d, want 4055/1576", tp.evalCount, tp.promptEvalCount)
	}
	// Usage without timings stays an estimate.
	tail2 := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":50}}`)
	tp2 := observeThroughput("/v1/chat/completions", tail2, 2000, 1000)
	if !tp2.approx || tp2.tps != 50 {
		t.Fatalf("usage-only: got %+v, want approx 50 tok/s", tp2)
	}
}

func TestReachablePerDialect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/api/version":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	client := &http.Client{}
	if !reachable(client, BackendCfg{URL: srv.URL, Dialect: "openai"}) {
		t.Fatal("openai backend answering /health should be reachable")
	}
	if !reachable(client, BackendCfg{URL: srv.URL}) {
		t.Fatal("ollama backend answering /api/version should be reachable")
	}
	down := BackendCfg{URL: "http://127.0.0.1:1", Dialect: "openai"}
	if reachable(client, down) {
		t.Fatal("dead openai backend should not be reachable")
	}
}
