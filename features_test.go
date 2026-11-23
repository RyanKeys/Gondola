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

// Sections 18-19 tests: wake-ahead fires only for asleep, wakeable
// machines in a wake-allowed mode; the embeddings endpoints route
// through the same resolution as chat (roles rewrite, explicit
// models pass through).

func featuresDispatcher(t *testing.T, upURL string) *Dispatcher {
	t.Helper()
	cfgText := `
backends:
  down:
    url: http://127.0.0.1:1
    default_model: m1
    mac: "AA:BB:CC:DD:EE:01"
  up:
    url: %s
    default_model: m2
    mac: "AA:BB:CC:DD:EE:02"
  nomac:
    url: http://127.0.0.1:1
    default_model: m3
roles:
  heavy: hm
  coder: cm
default_mode: home
modes:
  home:
    default: up
  wakey:
    default: down
    wake: true
    roles: {heavy: up, coder: nomac}
  sleepy:
    default: down
    wake: false
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

func TestWakeAheadDecisions(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	d := featuresDispatcher(t, up.URL)

	var sent []string
	old := sendMagicFn
	sendMagicFn = func(mac string, broadcasts []string) error {
		sent = append(sent, mac)
		return nil
	}
	defer func() { sendMagicFn = old }()

	d.wakeAhead("wakey")
	if len(sent) != 1 || sent[0] != "AA:BB:CC:DD:EE:01" {
		t.Fatalf("wakey: sent %v, want exactly the down backend's MAC", sent)
	}
	d.wakeAhead("sleepy")
	d.wakeAhead("home")
	if len(sent) != 1 {
		t.Fatalf("wake-forbidden modes must not send: sent %v", sent)
	}
}

func TestEmbeddingsRoutes(t *testing.T) {
	type seen struct {
		path  string
		model string
	}
	var reqs []seen
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/version" {
			_, _ = w.Write([]byte(`{"version":"fake"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		reqs = append(reqs, seen{r.URL.Path, body.Model})
		_ = json.NewEncoder(w).Encode(map[string]any{"model": body.Model, "embeddings": []any{[]float64{0.1}}})
	}))
	defer backend.Close()

	cfgText := `
backends:
  main:
    url: %s
    default_model: dm
roles:
  brain: rm
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
	handler := d.Routes()

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("/api/embed", `{"model":"brain","input":"hello"}`); rec.Code != http.StatusOK {
		t.Fatalf("/api/embed: got %d (%s)", rec.Code, rec.Body)
	}
	if rec := post("/api/embeddings", `{"model":"dm","input":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("/api/embeddings: got %d (%s)", rec.Code, rec.Body)
	}
	if rec := post("/v1/embeddings", `{"model":"explicit-model","input":"x"}`); rec.Code != http.StatusOK {
		t.Fatalf("/v1/embeddings: got %d (%s)", rec.Code, rec.Body)
	}
	want := []seen{
		{"/api/embed", "rm"},                 // role resolved and rewritten
		{"/api/embeddings", "dm"},            // explicit model, unchanged
		{"/v1/embeddings", "explicit-model"}, // pass-through
	}
	if len(reqs) != len(want) {
		t.Fatalf("backend saw %v, want %v", reqs, want)
	}
	for i := range want {
		if reqs[i] != want[i] {
			t.Fatalf("request %d: backend saw %v, want %v", i, reqs[i], want[i])
		}
	}
}
