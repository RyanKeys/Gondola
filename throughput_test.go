package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Section 20 tests: throughput observation from native eval counters
// (exact) and OpenAI usage (approximate), plus the end-to-end path
// where a proxied response completes its activity entry.

func TestObserveNativeExact(t *testing.T) {
	tail := []byte("{\"response\":\"hi\",\"done\":false}\n{\"response\":\"\",\"done\":true,\"eval_count\":100,\"eval_duration\":2000000000,\"prompt_eval_count\":12,\"prompt_eval_duration\":500000000}\n")
	tp := observeThroughput("/api/generate", tail, 2600, 600)
	if tp.evalCount != 100 || tp.promptEvalCount != 12 {
		t.Fatalf("counters: %+v", tp)
	}
	if math.Abs(tp.tps-50) > 0.01 || tp.approx {
		t.Fatalf("tps: %+v, want exact 50", tp)
	}
}

func TestObserveV1Approx(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":40}}`)
	// Streamed shape: first byte at 1000ms of 3000ms total -> the
	// generation window is 2000ms -> 20 tok/s.
	tp := observeThroughput("/v1/chat/completions", body, 3000, 1000)
	if !tp.approx || math.Abs(tp.tps-20) > 0.01 || tp.evalCount != 40 || tp.promptEvalCount != 10 {
		t.Fatalf("v1 streamed: %+v, want approx 20 tok/s", tp)
	}
	// Single-flush shape: first byte is effectively the last, so the
	// window falls back to the total -> 40 / 3s.
	tp = observeThroughput("/v1/chat/completions", body, 3000, 3000)
	if !tp.approx || math.Abs(tp.tps-40.0/3.0) > 0.01 {
		t.Fatalf("v1 single-flush: %+v, want approx 13.3 tok/s", tp)
	}
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":30}}\n\ndata: [DONE]\n")
	tp = observeThroughput("/v1/chat/completions", sse, 2500, 1000)
	if !tp.approx || math.Abs(tp.tps-20) > 0.01 {
		t.Fatalf("v1 sse: %+v, want approx 20 tok/s", tp)
	}
}

func TestObserveGarbage(t *testing.T) {
	if tp := observeThroughput("/api/chat", []byte("not json at all"), 100, 10); tp != (throughput{}) {
		t.Fatalf("garbage: %+v, want zero", tp)
	}
	if tp := observeThroughput("/api/embed", []byte(`{"embeddings":[[0.1]]}`), 100, 10); tp != (throughput{}) {
		t.Fatalf("embeddings carry no throughput: %+v", tp)
	}
}

func TestProxyCompletesThroughput(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"fake"}`))
			return
		}
		_, _ = w.Write([]byte("{\"response\":\"x\",\"done\":true,\"eval_count\":100,\"eval_duration\":2000000000,\"prompt_eval_count\":7}\n"))
	}))
	defer backend.Close()
	cfgText := `
backends:
  main:
    url: %s
    default_model: dm
default_mode: home
modes:
  home:
    default: main
server:
  listen: 127.0.0.1:0
  state_file: %s
`
	cfgText = strings.Replace(cfgText, "%s", backend.URL, 1)
	cfgText = strings.Replace(cfgText, "%s", filepath.Join(t.TempDir(), "mode.json"), 1)
	cfg, err := parseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(cfg)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", strings.NewReader(`{"model":"dm","prompt":"hi","stream":false}`))
	rec := httptest.NewRecorder()
	d.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy: got %d", rec.Code)
	}
	entries := d.activity.recent(10)
	if len(entries) != 1 {
		t.Fatalf("entries: %d", len(entries))
	}
	e := entries[0]
	if e.EvalCount != 100 || e.PromptEvalCount != 7 || math.Abs(e.TPS-50) > 0.01 || e.TPSApprox {
		t.Fatalf("completed entry: %+v", e)
	}
	sum := throughputSummary(entries)
	groups := sum["by_backend_model"].([]map[string]any)
	if len(groups) != 1 || groups[0]["avg_tok_per_sec"].(float64) != 50 {
		t.Fatalf("summary: %+v", sum)
	}
}
