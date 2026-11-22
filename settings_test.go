package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Section 16 tests: mode classification and summaries, renames with
// reference fix-up, and the settings save path (token preservation,
// validation before write).

const settingsTestConfig = `
backends:
  primary:
    url: http://127.0.0.1:1
    default_model: m1
  secondary:
    url: http://127.0.0.1:2
    default_model: m2
    model_prefixes: [fam-x]
  spare:
    url: http://127.0.0.1:3
    default_model: m3
roles:
  brain:
    default: m1
    overrides:
      secondary: m2x
default_mode: home
modes:
  home:
    default: primary
  away:
    default: primary
    wake: true
    roles:
      brain: secondary
server:
  listen: 127.0.0.1:0
  state_file: %s
  admin_token: %s
`

func settingsDispatcher(t *testing.T, token string) (*Dispatcher, string) {
	t.Helper()
	dir := t.TempDir()
	cfgText := strings.Replace(settingsTestConfig, "%s", dir+"/mode.json", 1)
	cfgText = strings.Replace(cfgText, "%s", token, 1)
	cfg, err := parseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(cfg)
	cfgPath := dir + "/dispatcher.yaml"
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	d.cfgPath = cfgPath
	return d, cfgPath
}

func classOf(info modeInfo, backend string) string {
	for _, bc := range info.Backends {
		if bc.Backend == backend {
			return bc.Class
		}
	}
	return ""
}

func TestDescribeModeClassification(t *testing.T) {
	d, _ := settingsDispatcher(t, "")
	home := describeMode(d.cfg, "home")
	if got := classOf(home, "primary"); got != "default" {
		t.Fatalf("home primary: got %q, want default", got)
	}
	if got := classOf(home, "secondary"); got != "on-call" {
		t.Fatalf("home secondary: got %q, want on-call (has prefixes)", got)
	}
	if got := classOf(home, "spare"); got != "ignored" {
		t.Fatalf("home spare: got %q, want ignored", got)
	}
	away := describeMode(d.cfg, "away")
	if got := classOf(away, "secondary"); got != "role-assigned" {
		t.Fatalf("away secondary: got %q, want role-assigned", got)
	}
	// Role table resolves per-backend overrides.
	for _, rl := range away.Roles {
		if rl.Role == "brain" && (rl.Backend != "secondary" || rl.Model != "m2x") {
			t.Fatalf("away brain row: got %+v, want secondary/m2x", rl)
		}
	}
	if !strings.Contains(away.Summary, "will be woken") {
		t.Fatalf("away summary missing wake note: %q", away.Summary)
	}
	if !strings.Contains(home.Summary, "are not woken") {
		t.Fatalf("home summary missing no-wake note: %q", home.Summary)
	}
	if usedByAnyMode(d.cfg, "spare") {
		t.Fatal("spare reported used by a mode")
	}
}

func TestApplyRenamesRewritesReferences(t *testing.T) {
	d, _ := settingsDispatcher(t, "")
	cfg := d.cfg
	if err := applyRenames(cfg, map[string]string{"secondary": "Jarvis"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Backends["Jarvis"]; !ok {
		t.Fatal("backend key not renamed")
	}
	if _, ok := cfg.Backends["secondary"]; ok {
		t.Fatal("old backend key survived")
	}
	if cfg.Modes["away"].Roles["brain"] != "Jarvis" {
		t.Fatal("mode role assignment not rewritten")
	}
	if cfg.Roles["brain"].Overrides["Jarvis"] != "m2x" {
		t.Fatal("role override key not rewritten")
	}
}

func TestApplyRenamesRejectsBadTargets(t *testing.T) {
	d, _ := settingsDispatcher(t, "")
	for _, tc := range []map[string]string{
		{"secondary": "primary"},   // collides with an existing backend
		{"secondary": "brain"},     // collides with a role
		{"secondary": "bad name!"}, // invalid characters
		{"ghost": "Jarvis"},        // source does not exist
	} {
		cfg, _ := parseConfig([]byte(strings.Replace(strings.Replace(settingsTestConfig, "%s", "/tmp/m.json", 1), "%s", "", 1)))
		if err := applyRenames(cfg, tc); err == nil {
			t.Fatalf("rename %v: got nil error, want rejection", tc)
		}
	}
	_ = d
}

func TestSettingsPutRoundTripPreservesToken(t *testing.T) {
	d, cfgPath := settingsDispatcher(t, "sekrit")
	// GET the settings as the UI would (backends unreachable here, so
	// model lists come back empty — the doc must still round-trip).
	rec := doReq(t, d, "GET", "/admin/settings", "sekrit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings: got %d (%s)", rec.Code, rec.Body.String())
	}
	var got struct {
		Settings settingsDoc `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"settings": got.Settings, "renames": map[string]string{}})
	rec = doReq(t, d, "PUT", "/admin/settings", "sekrit", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT settings: got %d (%s)", rec.Code, rec.Body.String())
	}
	saved, err := d.loadSavedConfig()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Server.AdminToken != "sekrit" {
		t.Fatal("settings save did not preserve the on-disk admin token")
	}
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Fatal("settings save did not write a .bak backup")
	}
}

func TestSettingsPutRenameAndInvalid(t *testing.T) {
	d, cfgPath := settingsDispatcher(t, "sekrit")
	rec := doReq(t, d, "GET", "/admin/settings", "sekrit", "")
	var got struct {
		Settings settingsDoc `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"settings": got.Settings, "renames": map[string]string{"secondary": "Jarvis"}})
	rec = doReq(t, d, "PUT", "/admin/settings", "sekrit", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT rename: got %d (%s)", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), "Jarvis") || strings.Contains(string(raw), "secondary") {
		t.Fatal("saved file does not reflect the rename everywhere")
	}
	// An invalid document (mode default names a ghost backend) is
	// rejected and leaves the file alone.
	before, _ := os.ReadFile(cfgPath)
	bad := got.Settings
	bad.Modes["home"] = ModeCfg{Default: "ghost"}
	body, _ = json.Marshal(map[string]any{"settings": bad, "renames": map[string]string{}})
	rec = doReq(t, d, "PUT", "/admin/settings", "sekrit", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT invalid: got %d, want 400", rec.Code)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("invalid settings PUT modified the file")
	}
}

func TestSettingsRequireToken(t *testing.T) {
	d, _ := settingsDispatcher(t, "sekrit")
	if rec := doReq(t, d, "GET", "/admin/settings", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET settings without token: got %d, want 401", rec.Code)
	}
	if rec := doReq(t, d, "PUT", "/admin/settings", "", "{}"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("PUT settings without token: got %d, want 401", rec.Code)
	}
}
