package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Structured settings (SPEC.md section 16): the console edits the
// config as typed data, never as raw text on the primary path. The
// admin token is deliberately not part of the settings document; a
// save always preserves the token currently on disk.

// settingsDoc is the JSON shape of a config without the admin token.
type settingsDoc struct {
	Backends    map[string]BackendCfg `json:"backends"`
	Roles       map[string]RoleTarget `json:"roles"`
	DefaultMode string                `json:"default_mode"`
	Modes       map[string]ModeCfg    `json:"modes"`
	Server      settingsServer        `json:"server"`
}

type settingsServer struct {
	Listen      string `json:"listen"`
	StateFile   string `json:"state_file"`
	WakeTimeout string `json:"wake_timeout"`
}

func docFromConfig(c *Config) settingsDoc {
	return settingsDoc{
		Backends:    c.Backends,
		Roles:       c.Roles,
		DefaultMode: c.DefaultMode,
		Modes:       c.Modes,
		Server: settingsServer{
			Listen:      c.Server.Listen,
			StateFile:   c.Server.StateFile,
			WakeTimeout: c.Server.WakeTimeout,
		},
	}
}

func (doc settingsDoc) toConfig(token string) *Config {
	c := &Config{
		Backends:    doc.Backends,
		Roles:       doc.Roles,
		DefaultMode: doc.DefaultMode,
		Modes:       doc.Modes,
	}
	c.Server.Listen = doc.Server.Listen
	c.Server.StateFile = doc.Server.StateFile
	c.Server.WakeTimeout = doc.Server.WakeTimeout
	c.Server.AdminToken = token
	return c
}

// --- mode consequences (section 16.7) ---

// backendClass is one device's role in one mode.
type backendClass struct {
	Backend  string   `json:"backend"`
	Class    string   `json:"class"` // default | role-assigned | on-call | ignored
	Dialect  string   `json:"dialect"`
	Roles    []string `json:"roles,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
}

// roleLine is one row of a mode's role table.
type roleLine struct {
	Role    string `json:"role"`
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// modeInfo is the computed consequence of one mode.
type modeInfo struct {
	Summary  string         `json:"summary"`
	Backends []backendClass `json:"backends"`
	Roles    []roleLine     `json:"roles"`
}

// describeMode classifies every backend in a mode and summarises the
// mode in one line, from the config alone (section 16.7).
func describeMode(c *Config, modeName string) modeInfo {
	mode := c.Modes[modeName]
	assigned := map[string][]string{} // backend -> roles the mode assigns to it
	for role, backend := range mode.Roles {
		assigned[backend] = append(assigned[backend], role)
	}
	info := modeInfo{}
	for _, name := range c.BackendNames() {
		bc := backendClass{Backend: name, Dialect: c.Backends[name].Dialect}
		if bc.Dialect == "" {
			bc.Dialect = "ollama"
		}
		switch {
		case name == mode.Default:
			bc.Class = "default"
		case len(assigned[name]) > 0:
			bc.Class = "role-assigned"
			bc.Roles = assigned[name]
			sort.Strings(bc.Roles)
		case len(c.Backends[name].ModelPrefixes) > 0:
			bc.Class = "on-call"
			bc.Prefixes = c.Backends[name].ModelPrefixes
		default:
			bc.Class = "ignored"
		}
		info.Backends = append(info.Backends, bc)
	}
	roleNames := make([]string, 0, len(c.Roles))
	for role := range c.Roles {
		roleNames = append(roleNames, role)
	}
	sort.Strings(roleNames)
	for _, role := range roleNames {
		backend := mode.Default
		if b, ok := mode.Roles[role]; ok {
			backend = b
		}
		info.Roles = append(info.Roles, roleLine{Role: role, Backend: backend, Model: c.Roles[role].Model(backend)})
	}
	info.Summary = modeSummary(c, modeName, mode, info)
	return info
}

func modeSummary(c *Config, modeName string, mode ModeCfg, info modeInfo) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%s serves all unassigned traffic", mode.Default))
	for _, bc := range info.Backends {
		switch bc.Class {
		case "role-assigned":
			parts = append(parts, fmt.Sprintf("%s serves %s", bc.Backend, strings.Join(bc.Roles, ", ")))
		case "on-call":
			parts = append(parts, fmt.Sprintf("%s is on-call for %s", bc.Backend, strings.Join(bc.Prefixes, ", ")))
		case "ignored":
			parts = append(parts, fmt.Sprintf("%s is ignored", bc.Backend))
		}
	}
	if mode.Wake {
		parts = append(parts, "asleep devices will be woken when needed")
	} else {
		parts = append(parts, "asleep devices are not woken")
	}
	if mode.PinDefaultModel {
		parts = append(parts, "requests that do not name a role are pinned to the serving backend's default model")
	}
	if mode.TTL != "" {
		parts = append(parts, fmt.Sprintf("returns to %s after %s", c.DefaultMode, mode.TTL))
	}
	return strings.Join(parts, ". ") + "."
}

// usedByAnyMode reports whether any mode references the backend at
// all (as a default or a role assignment).
func usedByAnyMode(c *Config, backend string) bool {
	for _, m := range c.Modes {
		if m.Default == backend {
			return true
		}
		for _, b := range m.Roles {
			if b == backend {
				return true
			}
		}
	}
	return false
}

// --- renames (section 16.4) ---

var backendNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// applyRenames renames backends in a config, rewriting every
// reference: mode defaults, mode role assignments, and role override
// keys. All renames are validated before any is applied.
func applyRenames(c *Config, renames map[string]string) error {
	targets := map[string]string{} // new name -> old name
	for old, newName := range renames {
		if old == newName {
			continue
		}
		if _, ok := c.Backends[old]; !ok {
			return fmt.Errorf("rename: backend %q does not exist", old)
		}
		if !backendNameRe.MatchString(newName) {
			return fmt.Errorf("rename: %q is not a valid backend name (letters, digits, dashes, underscores)", newName)
		}
		if prev, dup := targets[newName]; dup {
			return fmt.Errorf("rename: %q and %q both rename to %q", prev, old, newName)
		}
		targets[newName] = old
		if _, exists := c.Backends[newName]; exists {
			if _, alsoRenamed := renames[newName]; !alsoRenamed {
				return fmt.Errorf("rename: backend %q already exists", newName)
			}
		}
		if _, isRole := c.Roles[newName]; isRole {
			return fmt.Errorf("rename: %q is a role name", newName)
		}
	}
	mapName := func(n string) string {
		if nn, ok := renames[n]; ok {
			return nn
		}
		return n
	}
	backends := make(map[string]BackendCfg, len(c.Backends))
	for name, b := range c.Backends {
		backends[mapName(name)] = b
	}
	c.Backends = backends
	roles := make(map[string]RoleTarget, len(c.Roles))
	for name, r := range c.Roles {
		if len(r.Overrides) > 0 {
			ov := make(map[string]string, len(r.Overrides))
			for backend, model := range r.Overrides {
				ov[mapName(backend)] = model
			}
			r.Overrides = ov
		}
		roles[name] = r
	}
	c.Roles = roles
	for name, m := range c.Modes {
		m.Default = mapName(m.Default)
		if len(m.Roles) > 0 {
			mr := make(map[string]string, len(m.Roles))
			for role, backend := range m.Roles {
				mr[role] = mapName(backend)
			}
			m.Roles = mr
		}
		c.Modes[name] = m
	}
	return nil
}

// --- handlers ---

// backendModels fetches one backend's model names in its own
// dialect (best-effort): Ollama's /api/tags, or an openai backend's
// /v1/models (section 23.5).
func backendModels(b BackendCfg) []string {
	probe := &http.Client{Timeout: 2 * time.Second}
	path := "/api/tags"
	if b.IsOpenAI() {
		path = "/v1/models"
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(b.URL, "/")+path, nil)
	if err != nil {
		return nil
	}
	if b.IsOpenAI() && b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := probe.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	names := []string{}
	if b.IsOpenAI() {
		var list struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			return nil
		}
		for _, m := range list.Data {
			names = append(names, m.ID)
		}
	} else {
		var tags struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
			return nil
		}
		for _, m := range tags.Models {
			names = append(names, m.Name)
		}
	}
	sort.Strings(names)
	return names
}

func (d *Dispatcher) loadSavedConfig() (*Config, error) {
	if d.cfgPath == "" {
		return nil, fmt.Errorf("config path unknown")
	}
	raw, err := os.ReadFile(d.cfgPath)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

func (d *Dispatcher) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	saved, err := d.loadSavedConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	models := map[string][]string{}
	unused := map[string]bool{}
	for _, name := range saved.BackendNames() {
		models[name] = backendModels(saved.Backends[name])
		unused[name] = !usedByAnyMode(saved, name)
	}
	infos := map[string]modeInfo{}
	for _, name := range saved.ModeNames() {
		infos[name] = describeMode(saved, name)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":            d.cfgPath,
		"settings":        docFromConfig(saved),
		"models":          models,
		"unused":          unused,
		"mode_info":       infos,
		"restart_pending": configFingerprint(saved) != configFingerprint(d.cfg),
		"restart_note":    "Settings edit the saved config; the running dispatcher applies them on restart.",
	})
}

func (d *Dispatcher) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	if d.cfgPath == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "config path unknown"})
		return
	}
	var body struct {
		Settings settingsDoc       `json:"settings"`
		Renames  map[string]string `json:"renames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"settings\": {...}, \"renames\": {...}}"})
		return
	}
	// The token on disk is preserved across every settings save.
	saved, err := d.loadSavedConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	cfg := body.Settings.toConfig(saved.Server.AdminToken)
	if len(body.Renames) > 0 {
		if err := applyRenames(cfg, body.Renames); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	// Validate by rendering and running the real loader path: exactly
	// the bytes that would land on disk must parse as a valid config.
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if _, err := parseConfig(raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if prev, err := os.ReadFile(d.cfgPath); err == nil {
		_ = os.WriteFile(d.cfgPath+".bak", prev, 0o644)
	}
	if err := writeConfigFile(d.cfgPath, raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	d.logf("SETTINGS saved via web UI (restart to apply)")
	d.activity.add(activityEntry{Kind: "config", Detail: "settings saved via web UI; restart to apply"})
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "restart_required": true})
}
