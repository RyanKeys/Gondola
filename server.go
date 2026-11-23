package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const version = "0.7.0"

// Dispatcher is the running proxy.
type Dispatcher struct {
	cfg      *Config
	modes    *ModeManager
	waker    *waker
	client   *http.Client // no timeout: generation streams are long-lived
	activity *activityLog
	cfgPath  string // config file on disk, for the web UI editor (section 15)
}

func NewDispatcher(cfg *Config) *Dispatcher {
	return &Dispatcher{
		cfg:      cfg,
		modes:    NewModeManager(cfg, NewStateStore(cfg.Server.StateFile)),
		waker:    newWaker(),
		client:   &http.Client{},
		activity: newActivityLog(),
	}
}

func (d *Dispatcher) logf(format string, args ...any) {
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Routes returns the HTTP handler for the whole surface (SPEC.md 7).
func (d *Dispatcher) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/chat") })
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/generate") })
	mux.HandleFunc("POST /api/embed", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/embed") })
	mux.HandleFunc("POST /api/embeddings", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/embeddings") })
	mux.HandleFunc("POST /api/show", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/show") })
	mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/api/ps") })
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/v1/chat/completions") })
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) { d.proxy(w, r, "/v1/embeddings") })
	mux.HandleFunc("GET /api/tags", d.handleTags)
	mux.HandleFunc("GET /v1/models", d.handleV1Models)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		mode, _ := d.modes.Current()
		writeJSON(w, http.StatusOK, map[string]any{"version": version, "mode": mode})
	})
	mux.HandleFunc("GET /health", d.handleHealth)
	mux.HandleFunc("GET /mode", d.handleModeGet)
	mux.HandleFunc("POST /mode", d.handleModePost)
	mux.HandleFunc("GET /modes", d.handleModes)
	mux.HandleFunc("GET /ui", d.handleUI)
	mux.HandleFunc("GET /admin/activity", d.handleActivity)
	mux.HandleFunc("GET /admin/config", d.handleConfigGet)
	mux.HandleFunc("PUT /admin/config", d.handleConfigPut)
	mux.HandleFunc("GET /admin/settings", d.handleSettingsGet)
	mux.HandleFunc("PUT /admin/settings", d.handleSettingsPut)
	mux.HandleFunc("POST /admin/restart", d.handleRestart)
	return mux
}

func (d *Dispatcher) handleHealth(w http.ResponseWriter, r *http.Request) {
	mode, _ := d.modes.Current()
	backends := map[string]bool{}
	probe := &http.Client{Timeout: 2 * time.Second}
	for _, name := range d.cfg.BackendNames() {
		backends[name] = reachable(probe, d.cfg.Backends[name].URL)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode, "backends": backends})
}

func (d *Dispatcher) handleModeGet(w http.ResponseWriter, r *http.Request) {
	mode, _ := d.modes.Current()
	out := map[string]any{"mode": mode}
	if left := d.modes.ExpiresIn(); left >= 0 {
		out["expires_in_s"] = int(left.Seconds())
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Dispatcher) handleModePost(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"mode\": \"<name>\"}"})
		return
	}
	if err := d.modes.Set(body.Mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(), "modes": d.cfg.ModeNames(),
		})
		return
	}
	d.logf("MODE set -> %s", body.Mode)
	d.activity.add(activityEntry{Kind: "mode", Mode: body.Mode, Detail: "mode set via API"})
	go d.wakeAhead(body.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"mode": body.Mode})
}

func (d *Dispatcher) handleModes(w http.ResponseWriter, r *http.Request) {
	type modeView struct {
		Default         string            `json:"default"`
		Wake            bool              `json:"wake"`
		PinDefaultModel bool              `json:"pin_default_model"`
		TTL             string            `json:"ttl,omitempty"`
		Roles           map[string]string `json:"roles,omitempty"`
	}
	out := map[string]modeView{}
	summaries := map[string]string{}
	infos := map[string]modeInfo{}
	for _, name := range d.cfg.ModeNames() {
		m := d.cfg.Modes[name]
		out[name] = modeView{m.Default, m.Wake, m.PinDefaultModel, m.TTL, m.Roles}
		infos[name] = describeMode(d.cfg, name)
		summaries[name] = infos[name].Summary
	}
	writeJSON(w, http.StatusOK, map[string]any{"modes": out, "summaries": summaries, "info": infos})
}

func (d *Dispatcher) handleTags(w http.ResponseWriter, r *http.Request) {
	merged := []json.RawMessage{}
	probe := &http.Client{Timeout: 3 * time.Second}
	for _, name := range d.cfg.BackendNames() {
		base := strings.TrimRight(d.cfg.Backends[name].URL, "/")
		resp, err := probe.Get(base + "/api/tags")
		if err != nil {
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var tags struct {
				Models []json.RawMessage `json:"models"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&tags); err == nil {
				merged = append(merged, tags.Models...)
			}
		}()
	}
	if merged == nil {
		merged = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": merged})
}

func (d *Dispatcher) handleV1Models(w http.ResponseWriter, r *http.Request) {
	ids := []string{}
	for _, name := range d.cfg.BackendNames() {
		ids = append(ids, d.cfg.Backends[name].DefaultModel)
	}
	for _, name := range d.cfg.BackendNames() {
		ids = append(ids, name)
	}
	roles := make([]string, 0, len(d.cfg.Roles))
	for role := range d.cfg.Roles {
		roles = append(roles, role)
	}
	// deterministic order for roles
	for i := 0; i < len(roles); i++ {
		for j := i + 1; j < len(roles); j++ {
			if roles[j] < roles[i] {
				roles[i], roles[j] = roles[j], roles[i]
			}
		}
	}
	ids = append(ids, roles...)
	data := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		data = append(data, map[string]string{"id": id, "object": "model"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// proxy routes one generation request (SPEC.md sections 5-6).
func (d *Dispatcher) proxy(w http.ResponseWriter, r *http.Request, path string) {
	t0 := time.Now()
	modeName, mode := d.modes.Current()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unreadable body"})
		return
	}
	model := ""
	if len(raw) > 0 {
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &probe)
		model = probe.Model
	}
	dec, err := d.cfg.Resolve(model, r.Header.Get("X-Backend"), mode)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if dec.Rewritten {
		d.logf("REWRITE model=%s -> %s (backend=%s mode=%s)", model, dec.ServedModel, dec.Backend, modeName)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err == nil {
			body["model"] = dec.ServedModel
			if nb, err := json.Marshal(body); err == nil {
				raw = nb
			}
		}
	}
	if !d.ensureUp(dec.Backend, mode) {
		d.logf("ROUTE mode=%s model=%s -> %s FAILED (unreachable, wake allowed=%v)", modeName, model, dec.Backend, mode.Wake)
		d.activity.add(activityEntry{Kind: "route", Mode: modeName, Requested: model, Served: dec.ServedModel, Backend: dec.Backend, Path: path, Status: http.StatusBadGateway, Rewritten: dec.Rewritten, Ms: time.Since(t0).Milliseconds(), Detail: "backend unreachable"})
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "backend unreachable: " + dec.Backend})
		return
	}
	base := strings.TrimRight(d.cfg.Backends[dec.Backend].URL, "/")
	upReq, err := http.NewRequestWithContext(r.Context(), r.Method, base+path, bytes.NewReader(raw))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "content-length" || lk == "x-backend" {
			continue
		}
		for _, v := range vs {
			upReq.Header.Add(k, v)
		}
	}
	upReq.ContentLength = int64(len(raw))
	resp, err := d.client.Do(upReq)
	if err != nil {
		d.logf("ROUTE mode=%s model=%s -> %s CONNECT-FAIL %v", modeName, model, dec.Backend, err)
		d.activity.add(activityEntry{Kind: "route", Mode: modeName, Requested: model, Served: dec.ServedModel, Backend: dec.Backend, Path: path, Status: http.StatusBadGateway, Rewritten: dec.Rewritten, Ms: time.Since(t0).Milliseconds(), Detail: "backend connect failed"})
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "backend connect failed"})
		return
	}
	defer resp.Body.Close()
	ttfbMs := time.Since(t0).Milliseconds()
	d.logf("ROUTE mode=%s model=%s -> %s path=%s status=%d ttfb_ms=%d",
		modeName, model, dec.Backend, path, resp.StatusCode, ttfbMs)
	entryID := d.activity.add(activityEntry{Kind: "route", Mode: modeName, Requested: model, Served: dec.ServedModel, Backend: dec.Backend, Path: path, Status: resp.StatusCode, Rewritten: dec.Rewritten, Ms: ttfbMs})
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	tail := &tailBuf{max: 64 * 1024}
	firstByteMs := int64(-1)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if firstByteMs < 0 {
				firstByteMs = time.Since(t0).Milliseconds()
			}
			tail.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			totalMs := time.Since(t0).Milliseconds()
			d.activity.complete(entryID, totalMs, observeThroughput(path, tail.Bytes(), totalMs, firstByteMs))
			return
		}
	}
}
