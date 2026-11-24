package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModeTTLExpiry(t *testing.T) {
	c := testConfig(t)
	c.Modes["dev"] = ModeCfg{Default: "secondary", Wake: true, TTLDur: time.Hour}
	dir := t.TempDir()
	store := NewStateStore(filepath.Join(dir, "mode.json"))
	mm := NewModeManager(c, store)

	// Fresh dev mode is effective.
	if err := store.Set("dev"); err != nil {
		t.Fatal(err)
	}
	if name, _ := mm.Current(); name != "dev" {
		t.Fatalf("want dev, got %s", name)
	}
	// Backdate the set time beyond the TTL: falls back to default.
	st := ModeState{Mode: "dev", SetAt: time.Now().Add(-2 * time.Hour)}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(store.path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if name, _ := mm.Current(); name != "home" {
		t.Fatalf("expired dev should fall back to home, got %s", name)
	}
}

func TestModeCorruptStateFallsBack(t *testing.T) {
	c := testConfig(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "mode.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	mm := NewModeManager(c, NewStateStore(p))
	if name, _ := mm.Current(); name != "home" {
		t.Fatalf("corrupt state should give default mode home, got %s", name)
	}
}

func TestSetUnknownModeRejected(t *testing.T) {
	c := testConfig(t)
	mm := NewModeManager(c, NewStateStore(filepath.Join(t.TempDir(), "mode.json")))
	if err := mm.Set("bogus"); err == nil {
		t.Fatal("expected unknown mode to be rejected")
	}
	if err := mm.Set("away"); err != nil {
		t.Fatalf("known mode rejected: %v", err)
	}
	if name, _ := mm.Current(); name != "away" {
		t.Fatalf("want away, got %s", name)
	}
}

func TestStateFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mode.json")
	store := NewStateStore(path)
	if err := store.Set("home"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := fi.Mode()
	if mode&0o777 != 0o600 {
		t.Fatalf("want permissions 0600, got %04o", mode&0o777)
	}
}
