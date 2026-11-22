package main

import "testing"

func testConfig(t *testing.T) *Config {
	t.Helper()
	c := &Config{
		Backends: map[string]BackendCfg{
			"primary": {
				URL: "http://127.0.0.1:1", DefaultModel: "qwen3.5:9b",
				ModelPrefixes: []string{"qwen2.5-coder"},
			},
			"secondary": {
				URL: "http://127.0.0.1:2", DefaultModel: "qwen3:14b",
				ModelPrefixes: []string{"qwen3:14b"},
			},
		},
		Roles: map[string]RoleTarget{
			"brain": {Default: "ornith:9b", Overrides: map[string]string{"secondary": "ornith:9b-fast"}},
			"heavy": {Default: "qwen3.6:35b-a3b", Overrides: map[string]string{"secondary": "ornith:35b"}},
			"coder": {Default: "qwen3-coder:30b"}, "utility": {Default: "qwen3.5:4b"},
		},
		DefaultMode: "home",
		Modes: map[string]ModeCfg{
			"home": {Default: "primary"},
			"away": {Default: "secondary", Wake: true},
			"test": {Default: "primary", PinDefaultModel: true},
			"dev":  {Default: "secondary", Wake: true, Roles: map[string]string{"brain": "primary"}},
		},
	}
	if err := c.validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}
	return c
}

func TestResolveMatrix(t *testing.T) {
	c := testConfig(t)
	cases := []struct {
		name, model, header, mode string
		wantBackend, wantServed   string
		wantRewritten             bool
	}{
		// The pass-through guarantee: named models are served as named.
		{"explicit passes through home", "qwen3.6:35b-a3b", "", "home", "primary", "qwen3.6:35b-a3b", false},
		{"explicit passes through away", "some-vendor/model:Q4", "", "away", "secondary", "some-vendor/model:Q4", false},
		// Family pins.
		{"secondary family in home", "qwen3:14b", "", "home", "secondary", "qwen3:14b", false},
		{"primary family in away", "qwen2.5-coder:7b", "", "away", "primary", "qwen2.5-coder:7b", false},
		// Header wins over family and mode.
		{"header forces secondary", "qwen3.5:9b", "secondary", "home", "secondary", "qwen3.5:9b", false},
		{"header forces primary", "qwen3:14b", "primary", "away", "primary", "qwen3:14b", false},
		// Bare aliases and model-less requests.
		{"alias secondary", "secondary", "", "home", "secondary", "qwen3:14b", true},
		{"alias primary in away", "primary", "", "away", "primary", "qwen3.5:9b", true},
		{"model-less home", "", "", "home", "primary", "qwen3.5:9b", true},
		{"model-less away", "", "", "away", "secondary", "qwen3:14b", true},
		// Test-mode pin rewrites to the backend default...
		{"pin rewrites foreign model", "qwen3.6:35b-a3b", "", "test", "primary", "qwen3.5:9b", true},
		// ...but a genuine primary-family model routes by family first is
		// still the primary default here, so no visible change.
		{"pin keeps default", "qwen3.5:9b", "", "test", "primary", "qwen3.5:9b", false},
		// Roles resolve per mode, with per-mode backend overrides, and
		// the served model follows the role's per-backend override for
		// whichever backend ends up serving.
		{"role home", "heavy", "", "home", "primary", "qwen3.6:35b-a3b", true},
		{"role away per-backend override", "heavy", "", "away", "secondary", "ornith:35b", true},
		{"role brain home default", "brain", "", "home", "primary", "ornith:9b", true},
		{"role brain away override", "brain", "", "away", "secondary", "ornith:9b-fast", true},
		// Dev pins brain to primary; the role's override is for
		// secondary, so the default model serves — the two override
		// layers compose instead of colliding.
		{"role dev backend pin, model fallback", "brain", "", "dev", "primary", "ornith:9b", true},
		{"role test beats pin", "coder", "", "test", "primary", "qwen3-coder:30b", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := c.Resolve(tc.model, tc.header, c.Modes[tc.mode])
			if err != nil {
				t.Fatalf("Resolve(%q, %q, %s): %v", tc.model, tc.header, tc.mode, err)
			}
			if dec.Backend != tc.wantBackend || dec.ServedModel != tc.wantServed || dec.Rewritten != tc.wantRewritten {
				t.Errorf("got %+v, want backend=%s served=%s rewritten=%v",
					dec, tc.wantBackend, tc.wantServed, tc.wantRewritten)
			}
		})
	}
}

func TestResolveUnknownHeaderBackend(t *testing.T) {
	c := testConfig(t)
	if _, err := c.Resolve("qwen3.5:9b", "bogus", c.Modes["home"]); err == nil {
		t.Fatal("expected error for unknown X-Backend value")
	}
}
