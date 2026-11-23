package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Section 17 tests: restart-pending detection and the restart
// endpoint's safety properties (validation before exec, admin
// gating, exec stubbed so a test never actually re-executes).

func TestFingerprintIgnoresFormatting(t *testing.T) {
	a := `
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
  state_file: /tmp/x.json
admin_token: tok
`
	b := `
server: {listen: 127.0.0.1:0, state_file: /tmp/x.json}
modes: {home: {default: primary}}
default_mode: home
backends: {primary: {url: http://127.0.0.1:1, default_model: m1}}
admin_token: tok
`
	ca, err := parseConfig([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	cb, err := parseConfig([]byte(b))
	if err != nil {
		t.Fatal(err)
	}
	if configFingerprint(ca) != configFingerprint(cb) {
		t.Fatal("formatting alone must not change the fingerprint")
	}
	cb.Backends["primary"] = BackendCfg{URL: "http://127.0.0.1:1", DefaultModel: "other"}
	if configFingerprint(ca) == configFingerprint(cb) {
		t.Fatal("a model change must change the fingerprint")
	}
	cb2, err := parseConfig([]byte(a))
	if err != nil {
		t.Fatal(err)
	}
	cb2.Server.AdminToken = "rotated"
	if configFingerprint(ca) == configFingerprint(cb2) {
		t.Fatal("a token rotation must change the fingerprint")
	}
}

func TestRestartPending(t *testing.T) {
	d, cfgPath := webUIDispatcher(t, "tok")
	pending, err := d.restartPending()
	if err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("disk matches running config: must not be pending")
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "default_model: m1", "default_model: m2", 1)
	if err := os.WriteFile(cfgPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	pending, err = d.restartPending()
	if err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("disk differs from running config: must be pending")
	}
}

func TestHandleRestartValidatesThenExecs(t *testing.T) {
	d, cfgPath := webUIDispatcher(t, "tok")
	called := make(chan struct{}, 2)
	old := execProcess
	execProcess = func() error { called <- struct{}{}; return nil }
	defer func() { execProcess = old }()

	rec := doReq(t, d, http.MethodPost, "/admin/restart", "tok", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("valid saved config: got %d, want 200 (%s)", rec.Code, rec.Body)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("exec was not invoked after a valid restart request")
	}

	if err := os.WriteFile(cfgPath, []byte("backends: [nonsense"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, d, http.MethodPost, "/admin/restart", "tok", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid saved config: got %d, want 400", rec.Code)
	}
	select {
	case <-called:
		t.Fatal("exec must not run when the saved config is invalid")
	case <-time.After(600 * time.Millisecond):
	}
}

func TestHandleRestartRequiresToken(t *testing.T) {
	d, _ := webUIDispatcher(t, "sekret")
	rec := doReq(t, d, http.MethodPost, "/admin/restart", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", rec.Code)
	}
}
