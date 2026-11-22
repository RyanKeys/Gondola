package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dispatcher.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadExampleConfig(t *testing.T) {
	if _, err := LoadConfig("dispatcher.example.yaml"); err != nil {
		t.Fatalf("example config must load: %v", err)
	}
}

func TestInvalidConfigsRejected(t *testing.T) {
	base := `
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
default_mode: home
modes:
  home: {default: primary}
`
	if _, err := LoadConfig(writeTemp(t, base)); err != nil {
		t.Fatalf("base config should load: %v", err)
	}
	bad := []string{
		// mode names an undefined backend
		`
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
default_mode: home
modes:
  home: {default: ghost}
`,
		// default_mode not defined
		`
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
default_mode: away
modes:
  home: {default: primary}
`,
		// bad ttl
		`
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
default_mode: home
modes:
  home: {default: primary, ttl: "soon"}
`,
	}
	for i, content := range bad {
		if _, err := LoadConfig(writeTemp(t, content)); err == nil {
			t.Errorf("bad config %d loaded without error", i)
		}
	}
}

func TestRoleTargetForms(t *testing.T) {
	// Scalar and mapping role forms coexist; the mapping form carries a
	// default plus per-backend overrides.
	content := `
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
  secondary: {url: "http://127.0.0.1:11435", default_model: "m2"}
roles:
  brain: ornith:9b
  heavy:
    default: qwen3.6:35b-a3b
    overrides:
      secondary: ornith:35b
default_mode: home
modes:
  home: {default: primary}
`
	c, err := LoadConfig(writeTemp(t, content))
	if err != nil {
		t.Fatalf("mixed role forms should load: %v", err)
	}
	if got := c.Roles["brain"]; got.Default != "ornith:9b" || len(got.Overrides) != 0 {
		t.Errorf("scalar role parsed as %+v, want default-only ornith:9b", got)
	}
	heavy := c.Roles["heavy"]
	if heavy.Default != "qwen3.6:35b-a3b" || heavy.Model("secondary") != "ornith:35b" || heavy.Model("primary") != "qwen3.6:35b-a3b" {
		t.Errorf("mapping role parsed as %+v, want default + secondary override", heavy)
	}
}

func TestInvalidRoleTargetsRejected(t *testing.T) {
	head := `
backends:
  primary: {url: "http://127.0.0.1:11434", default_model: "m1"}
default_mode: home
modes:
  home: {default: primary}
roles:
`
	bad := []string{
		// mapping form without a default
		head + `
  heavy:
    overrides: {primary: ornith:35b}
`,
		// override names an undefined backend
		head + `
  heavy:
    default: qwen3.6:35b-a3b
    overrides: {ghost: ornith:35b}
`,
		// override with an empty model
		head + `
  heavy:
    default: qwen3.6:35b-a3b
    overrides: {primary: ""}
`,
		// scalar form with an empty model
		head + `
  heavy: ""
`,
	}
	for i, content := range bad {
		if _, err := LoadConfig(writeTemp(t, content)); err == nil {
			t.Errorf("bad role config %d loaded without error", i)
		}
	}
}
