package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Section 15 tests: the web UI's activity log, admin gating, and the
// config editor's safety properties (redaction round-trip, validation
// before write, backup on save).

const webUITestConfig = `
backends:
  primary:
    url: http://127.0.0.1:1
    default_model: m1
default_mode: home
modes:
  home:
    default: primary
server:
  listen: 127.0.0.1:0
  state_file: %s
  admin_token: %s
`

func webUIDispatcher(t *testing.T, token string) (*Dispatcher, string) {
	t.Helper()
	dir := t.TempDir()
	cfgText := strings.Replace(webUITestConfig, "%s", filepath.Join(dir, "mode.json"), 1)
	cfgText = strings.Replace(cfgText, "%s", token, 1)
	cfg, err := parseConfig([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDispatcher(cfg)
	cfgPath := filepath.Join(dir, "dispatcher.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	d.cfgPath = cfgPath
	return d, cfgPath
}

func doReq(t *testing.T, d *Dispatcher, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	d.Routes().ServeHTTP(rec, req)
	return rec
}

func TestActivityLogWrapsAndOrders(t *testing.T) {
	a := newActivityLog()
	for i := 0; i < activityCapacity+5; i++ {
		a.add(activityEntry{Kind: "route", Backend: "b", Status: i})
	}
	got := a.recent(3)
	if len(got) != 3 {
		t.Fatalf("recent(3) returned %d entries", len(got))
	}
	if got[0].Status != activityCapacity+4 || got[2].Status != activityCapacity+2 {
		t.Fatalf("newest-first order wrong: %d, %d", got[0].Status, got[2].Status)
	}
	if all := a.recent(1000); len(all) != activityCapacity {
		t.Fatalf("ring holds %d entries, want %d", len(all), activityCapacity)
	}
}

func TestAdminEndpointsRequireToken(t *testing.T) {
	d, _ := webUIDispatcher(t, "sekrit")
	if rec := doReq(t, d, "GET", "/admin/activity", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("activity without token: got %d, want 401", rec.Code)
	}
	if rec := doReq(t, d, "GET", "/admin/activity", "sekrit", ""); rec.Code != http.StatusOK {
		t.Fatalf("activity with token: got %d, want 200", rec.Code)
	}
	if rec := doReq(t, d, "GET", "/admin/config", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("config without token: got %d, want 401", rec.Code)
	}
	// No token configured: the section 10 rule leaves admin open.
	open, _ := webUIDispatcher(t, "")
	if rec := doReq(t, open, "GET", "/admin/activity", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("activity with no token configured: got %d, want 200", rec.Code)
	}
}

func TestConfigGetRedactsToken(t *testing.T) {
	d, _ := webUIDispatcher(t, "sekrit")
	rec := doReq(t, d, "GET", "/admin/config", "sekrit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var out struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.YAML, "sekrit") {
		t.Fatal("config GET leaked the admin token")
	}
	if !strings.Contains(out.YAML, redactedToken) {
		t.Fatal("config GET did not redact the admin token")
	}
}

func TestConfigPutRejectsInvalidAndKeepsFile(t *testing.T) {
	d, cfgPath := webUIDispatcher(t, "sekrit")
	before, _ := os.ReadFile(cfgPath)
	body, _ := json.Marshal(map[string]string{"yaml": "backends: {}\n"})
	rec := doReq(t, d, "PUT", "/admin/config", "sekrit", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid config PUT: got %d, want 400", rec.Code)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Fatal("invalid config PUT modified the file on disk")
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Fatal("invalid config PUT created a backup")
	}
}

func TestConfigPutValidRoundTripsTokenAndBacksUp(t *testing.T) {
	d, cfgPath := webUIDispatcher(t, "sekrit")
	// Load the redacted text exactly as the UI would, change a harmless
	// field, and save: the placeholder must become the real token again.
	rec := doReq(t, d, "GET", "/admin/config", "sekrit", "")
	var got struct {
		YAML string `json:"yaml"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(got.YAML, "default_model: m1", "default_model: m2", 1)
	body, _ := json.Marshal(map[string]string{"yaml": edited})
	rec = doReq(t, d, "PUT", "/admin/config", "sekrit", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid config PUT: got %d (%s), want 200", rec.Code, rec.Body.String())
	}
	saved, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(saved), "sekrit") {
		t.Fatal("saved config lost the real admin token")
	}
	if !strings.Contains(string(saved), "default_model: m2") {
		t.Fatal("saved config does not contain the edit")
	}
	if _, err := os.Stat(cfgPath + ".bak"); err != nil {
		t.Fatal("valid config PUT did not write a .bak backup")
	}
}

func TestWriteConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dispatcher.yaml"
	if err := writeConfigFile(path, []byte("a: 1\n")); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(path, []byte("a: 2\n")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a: 2\n" {
		t.Fatalf("content = %q, want the second write", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	// A target that cannot be replaced or written (a directory) must
	// surface an error, never a silent success. This stands in for
	// the bind-mount EBUSY case, which a unit test cannot mount.
	target := dir + "/adir"
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(target, []byte("x")); err == nil {
		t.Fatal("writeConfigFile onto a directory: got nil error")
	}
}

func TestUIServesPage(t *testing.T) {
	d, _ := webUIDispatcher(t, "sekrit")
	rec := doReq(t, d, "GET", "/ui", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "gondola") {
		t.Fatal("GET /ui did not return the console page")
	}
}
