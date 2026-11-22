package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The web UI (SPEC.md section 15): one embedded page at /ui backed by
// /admin/* JSON endpoints. The page shell is served openly like /health;
// every data endpoint follows the section 10 admin rule — open when no
// admin token is configured, Bearer-token-gated when one is.

//go:embed ui.html
var uiPage []byte

const activityCapacity = 300

// activityEntry is one recorded routing or admin event.
type activityEntry struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"` // "route", "mode", "config"
	Mode      string    `json:"mode,omitempty"`
	Requested string    `json:"requested,omitempty"`
	Served    string    `json:"served,omitempty"`
	Backend   string    `json:"backend,omitempty"`
	Path      string    `json:"path,omitempty"`
	Status    int       `json:"status,omitempty"`
	Rewritten bool      `json:"rewritten,omitempty"`
	Ms        int64     `json:"ms,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// activityLog is a fixed-capacity ring of recent events, newest last.
// In-memory by design: it is a live view, not an audit trail, and it
// resets when the process restarts.
type activityLog struct {
	mu      sync.Mutex
	entries []activityEntry
}

func newActivityLog() *activityLog { return &activityLog{} }

func (a *activityLog) add(e activityEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	a.entries = append(a.entries, e)
	if len(a.entries) > activityCapacity {
		a.entries = a.entries[len(a.entries)-activityCapacity:]
	}
}

// recent returns up to n entries, newest first.
func (a *activityLog) recent(n int) []activityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []activityEntry{}
	for i := len(a.entries) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a.entries[i])
	}
	return out
}

// requireAdmin enforces the section 10 admin rule. It returns false
// (and writes the 401) when a token is configured and not presented.
func (d *Dispatcher) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token := d.cfg.Server.AdminToken
	if token == "" {
		return true
	}
	if r.Header.Get("Authorization") == "Bearer "+token {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "admin token required"})
	return false
}

func (d *Dispatcher) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiPage)
}

func (d *Dispatcher) handleActivity(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": d.activity.recent(200)})
}

// The admin token is the one secret a config file can hold. The editor
// never receives it: GET replaces its value with a placeholder, and
// PUT substitutes the real value back when the placeholder survives,
// so a load-edit-save round trip cannot leak or clobber it. Typing a
// new value in its place rotates the token (effective on restart).
const redactedToken = "__REDACTED__"

var adminTokenLine = regexp.MustCompile(`(?m)^(\s*admin_token:\s*).*$`)

func redactToken(text string) string {
	return adminTokenLine.ReplaceAllString(text, "${1}"+redactedToken)
}

func (d *Dispatcher) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	if d.cfgPath == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "config path unknown"})
		return
	}
	raw, err := os.ReadFile(d.cfgPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": d.cfgPath, "yaml": redactToken(string(raw))})
}

// writeConfigFile replaces the config file's contents. It prefers an
// atomic temp-file-plus-rename; when the config is a single-file bind
// mount (a common Docker shape) renaming over it is impossible
// (EBUSY), so it falls back to writing in place. The content has
// already been validated at this point, and the caller's .bak copy is
// the recovery path.
func writeConfigFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	defer os.Remove(tmp)
	return os.WriteFile(path, data, 0o644)
}

func (d *Dispatcher) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	if d.cfgPath == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "config path unknown"})
		return
	}
	var body struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"yaml\": \"<config text>\"}"})
		return
	}
	text := body.YAML
	if strings.Contains(text, redactedToken) {
		text = adminTokenLine.ReplaceAllString(text, "${1}"+d.cfg.Server.AdminToken)
	}
	// Validate through the real loader path: an invalid config is
	// rejected here and never reaches the disk.
	if _, err := parseConfig([]byte(text)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if prev, err := os.ReadFile(d.cfgPath); err == nil {
		_ = os.WriteFile(d.cfgPath+".bak", prev, 0o644)
	}
	if err := writeConfigFile(d.cfgPath, []byte(text)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	d.logf("CONFIG saved via web UI (restart to apply)")
	d.activity.add(activityEntry{Kind: "config", Detail: "config saved via web UI; restart to apply"})
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "restart_required": true})
}
