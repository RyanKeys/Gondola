package main

import (
	"fmt"
	"os"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// BackendCfg is one PC/GPU serving models. Dialect selects the API
// the backend speaks (SPEC.md section 23): "ollama" (the default
// when absent) or "openai" for an OpenAI-compatible server such as
// PolyStrata. APIKey is gondola's Bearer credential to an openai
// backend; MaxContext is informational only (shown in the console)
// — an openai backend's context length is its launch property.
type BackendCfg struct {
	URL           string   `yaml:"url" json:"url"`
	MAC           string   `yaml:"mac" json:"mac"`
	WOLBroadcasts []string `yaml:"wol_broadcasts" json:"wol_broadcasts"`
	DefaultModel  string   `yaml:"default_model" json:"default_model"`
	ModelPrefixes []string `yaml:"model_prefixes" json:"model_prefixes"`
	Dialect       string   `yaml:"dialect" json:"dialect"`
	APIKey        string   `yaml:"api_key" json:"api_key"`
	MaxContext    int      `yaml:"max_context" json:"max_context"`
}

// IsOpenAI reports whether the backend speaks the OpenAI dialect.
func (b BackendCfg) IsOpenAI() bool { return b.Dialect == "openai" }

// RoleTarget is what a role resolves to. In YAML a role is either a
// plain model name (shorthand for Default alone) or a mapping with a
// default model plus per-backend overrides — for fleets where the best
// model for a job differs by machine.
type RoleTarget struct {
	Default   string            `yaml:"default" json:"default"`
	Overrides map[string]string `yaml:"overrides,omitempty" json:"overrides,omitempty"`
}

// UnmarshalYAML accepts both role forms: a scalar model name, or a
// mapping with default and overrides keys.
func (r *RoleTarget) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Default = value.Value
		return nil
	}
	type plain RoleTarget // distinct type: no UnmarshalYAML recursion
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*r = RoleTarget(p)
	return nil
}

// Model returns the model this role serves on the given backend: the
// backend's override when one is set, else the default.
func (r RoleTarget) Model(backend string) string {
	if m, ok := r.Overrides[backend]; ok {
		return m
	}
	return r.Default
}

// ModeCfg assigns backends to request classes. Default is required;
// Roles may override individual roles; Wake gates Wake-on-LAN for the
// whole mode; PinDefaultModel rewrites any model to the chosen backend's
// default (benchmark isolation); TTL auto-returns to the default mode.
type ModeCfg struct {
	Default         string            `yaml:"default" json:"default"`
	Wake            bool              `yaml:"wake" json:"wake"`
	PinDefaultModel bool              `yaml:"pin_default_model" json:"pin_default_model"`
	TTL             string            `yaml:"ttl" json:"ttl"`
	Roles           map[string]string `yaml:"roles" json:"roles"`
	TTLDur          time.Duration     `yaml:"-" json:"-"`
}

// Config is the whole dispatcher configuration (SPEC.md section 4).
type Config struct {
	Backends    map[string]BackendCfg `yaml:"backends"`
	Roles       map[string]RoleTarget `yaml:"roles"`
	DefaultMode string                `yaml:"default_mode"`
	Modes       map[string]ModeCfg    `yaml:"modes"`
	Server      struct {
		Listen      string `yaml:"listen"`
		StateFile   string `yaml:"state_file"`
		WakeTimeout string `yaml:"wake_timeout"`
		AdminToken  string `yaml:"admin_token"`
	} `yaml:"server"`
	WakeTimeoutDur time.Duration `yaml:"-"`
}

// BackendNames returns backend names in deterministic (sorted) order.
func (c *Config) BackendNames() []string {
	names := make([]string, 0, len(c.Backends))
	for n := range c.Backends {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ModeNames returns mode names in deterministic (sorted) order.
func (c *Config) ModeNames() []string {
	names := make([]string, 0, len(c.Modes))
	for n := range c.Modes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseConfig(raw)
}

// parseConfig unmarshals and validates config YAML (SPEC.md section 4).
// The web UI's config editor validates through this same path before a
// save is allowed to touch the file on disk.
func parseConfig(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("config: at least one backend is required")
	}
	for name, b := range c.Backends {
		if b.URL == "" {
			return fmt.Errorf("config: backend %q: url is required", name)
		}
		if b.DefaultModel == "" {
			return fmt.Errorf("config: backend %q: default_model is required", name)
		}
		switch b.Dialect {
		case "", "ollama", "openai":
		default:
			return fmt.Errorf("config: backend %q: dialect %q is not \"ollama\" or \"openai\"", name, b.Dialect)
		}
		if b.MaxContext < 0 {
			return fmt.Errorf("config: backend %q: max_context must not be negative", name)
		}
	}
	if c.DefaultMode == "" {
		return fmt.Errorf("config: default_mode is required")
	}
	if _, ok := c.Modes[c.DefaultMode]; !ok {
		return fmt.Errorf("config: default_mode %q is not a defined mode", c.DefaultMode)
	}
	for name, m := range c.Modes {
		if _, ok := c.Backends[m.Default]; !ok {
			return fmt.Errorf("config: mode %q: default backend %q is not defined", name, m.Default)
		}
		for role, backend := range m.Roles {
			if _, ok := c.Backends[backend]; !ok {
				return fmt.Errorf("config: mode %q: role %q names undefined backend %q", name, role, backend)
			}
			if _, ok := c.Roles[role]; !ok {
				return fmt.Errorf("config: mode %q: role override %q is not a defined role", name, role)
			}
		}
		if m.TTL != "" {
			d, err := time.ParseDuration(m.TTL)
			if err != nil {
				return fmt.Errorf("config: mode %q: bad ttl %q: %w", name, m.TTL, err)
			}
			m.TTLDur = d
			c.Modes[name] = m
		}
	}
	for role, target := range c.Roles {
		if target.Default == "" {
			return fmt.Errorf("config: role %q: default model is required", role)
		}
		for backend, model := range target.Overrides {
			if _, ok := c.Backends[backend]; !ok {
				return fmt.Errorf("config: role %q: override names undefined backend %q", role, backend)
			}
			if model == "" {
				return fmt.Errorf("config: role %q: override for backend %q: model is required", role, backend)
			}
		}
	}
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:11435"
	}
	if c.Server.StateFile == "" {
		c.Server.StateFile = "/data/mode.json"
	}
	c.WakeTimeoutDur = 150 * time.Second
	if c.Server.WakeTimeout != "" {
		d, err := time.ParseDuration(c.Server.WakeTimeout)
		if err != nil {
			return fmt.Errorf("config: server.wake_timeout %q: %w", c.Server.WakeTimeout, err)
		}
		c.WakeTimeoutDur = d
	}
	return nil
}
