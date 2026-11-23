package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"syscall"
	"time"
)

// Applying saved settings (SPEC.md section 17): the console saves the
// config to disk, and the running dispatcher keeps serving the config
// it started with. This file closes that gap: it detects when the
// saved config differs from the running one, and it can re-execute
// the process in place so the saved config takes effect without an
// external restart. The mode survives because it lives in the state
// file, not in memory.

// configFingerprint is a canonical hash of everything a restart
// would change: the full settings document plus the admin token.
// Canonical JSON (map keys sorted by encoding/json) makes the hash
// immune to file formatting, so only real changes flag as pending.
func configFingerprint(c *Config) string {
	raw, err := json.Marshal(docFromConfig(c))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append(raw, c.Server.AdminToken...))
	return hex.EncodeToString(sum[:])
}

// restartPending reports whether the saved config differs from the
// running one.
func (d *Dispatcher) restartPending() (bool, error) {
	saved, err := d.loadSavedConfig()
	if err != nil {
		return false, err
	}
	return configFingerprint(saved) != configFingerprint(d.cfg), nil
}

// execProcess replaces the running process with a fresh copy of the
// same binary and arguments (same PID). A variable so tests can stub
// it — a test must never actually re-exec.
var execProcess = func() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	argv := append([]string{exe}, os.Args[1:]...)
	return syscall.Exec(exe, argv, os.Environ())
}

func (d *Dispatcher) handleRestart(w http.ResponseWriter, r *http.Request) {
	if !d.requireAdmin(w, r) {
		return
	}
	// Validate the saved config through the real loader before
	// committing to the restart: a config that would fail at startup
	// is rejected here and the running process is left untouched.
	if _, err := d.loadSavedConfig(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "saved config is not loadable, restart refused: " + err.Error()})
		return
	}
	d.logf("RESTART requested from console; re-executing to apply saved config")
	writeJSON(w, http.StatusOK, map[string]any{"restarting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		// Give the response a moment to reach the client before the
		// process image is replaced.
		time.Sleep(250 * time.Millisecond)
		if err := execProcess(); err != nil {
			d.logf("RESTART failed: %v (continuing with the running config)", err)
		}
	}()
}
