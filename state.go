package main

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// ModeState persists the active mode. A missing or corrupt file means
// "start in the default mode" (SPEC.md section 8).
type ModeState struct {
	Mode  string    `json:"mode"`
	SetAt time.Time `json:"set_at"`
}

type StateStore struct {
	path string
	mu   sync.Mutex
}

func NewStateStore(path string) *StateStore { return &StateStore{path: path} }

func (s *StateStore) Get() (ModeState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return ModeState{}, false
	}
	var st ModeState
	if err := json.Unmarshal(raw, &st); err != nil || st.Mode == "" {
		return ModeState{}, false
	}
	return st, true
}

func (s *StateStore) Set(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.Marshal(ModeState{Mode: mode, SetAt: time.Now()})
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}

// ModeManager resolves the effective mode, applying TTL expiry.
type ModeManager struct {
	cfg   *Config
	store *StateStore
}

func NewModeManager(cfg *Config, store *StateStore) *ModeManager {
	return &ModeManager{cfg: cfg, store: store}
}

// Current returns the effective mode name and its config. A mode whose
// TTL has expired atomically falls back to the default mode.
func (m *ModeManager) Current() (string, ModeCfg) {
	st, ok := m.store.Get()
	if !ok {
		return m.cfg.DefaultMode, m.cfg.Modes[m.cfg.DefaultMode]
	}
	mc, defined := m.cfg.Modes[st.Mode]
	if !defined {
		return m.cfg.DefaultMode, m.cfg.Modes[m.cfg.DefaultMode]
	}
	if mc.TTLDur > 0 && !st.SetAt.IsZero() && time.Since(st.SetAt) > mc.TTLDur {
		_ = m.store.Set(m.cfg.DefaultMode)
		return m.cfg.DefaultMode, m.cfg.Modes[m.cfg.DefaultMode]
	}
	return st.Mode, mc
}

// ExpiresIn reports the remaining TTL of the active mode, or -1.
func (m *ModeManager) ExpiresIn() time.Duration {
	st, ok := m.store.Get()
	if !ok {
		return -1
	}
	mc, defined := m.cfg.Modes[st.Mode]
	if !defined || mc.TTLDur <= 0 {
		return -1
	}
	left := mc.TTLDur - time.Since(st.SetAt)
	if left < 0 {
		return 0
	}
	return left
}

func (m *ModeManager) Set(name string) error {
	if _, ok := m.cfg.Modes[name]; !ok {
		return &UnknownModeError{Name: name, Valid: m.cfg.ModeNames()}
	}
	return m.store.Set(name)
}

type UnknownModeError struct {
	Name  string
	Valid []string
}

func (e *UnknownModeError) Error() string {
	return "unknown mode " + e.Name
}
