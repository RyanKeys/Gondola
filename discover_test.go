package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBroadcastAddr(t *testing.T) {
	cases := []struct{ ip, mask, want string }{
		{"10.0.0.132", "255.255.255.0", "10.0.0.255"},
		{"192.168.1.10", "255.255.0.0", "192.168.255.255"},
		{"10.0.0.80", "255.255.255.255", "10.0.0.80"},
		{"10.0.0.80", "255.255.255.254", "10.0.0.81"},
	}
	for _, c := range cases {
		got := broadcastAddr(net.ParseIP(c.ip), net.IPMask(net.ParseIP(c.mask).To4()))
		if got.String() != c.want {
			t.Errorf("broadcast(%s, %s) = %s, want %s", c.ip, c.mask, got, c.want)
		}
	}
}

func TestEnumerateHosts(t *testing.T) {
	hosts := func(cidr string) []string {
		ip, ipnet, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, h := range enumerateHosts(ip, ipnet.Mask) {
			out = append(out, h.String())
		}
		return out
	}
	if got := hosts("10.0.0.81/30"); len(got) != 2 || got[0] != "10.0.0.81" || got[1] != "10.0.0.82" {
		t.Errorf("/30 = %v, want [10.0.0.81 10.0.0.82]", got)
	}
	if got := hosts("10.0.0.80/31"); len(got) != 2 || got[0] != "10.0.0.80" || got[1] != "10.0.0.81" {
		t.Errorf("/31 = %v, want [10.0.0.80 10.0.0.81]", got)
	}
	if got := hosts("10.0.0.80/32"); len(got) != 1 || got[0] != "10.0.0.80" {
		t.Errorf("/32 = %v, want [10.0.0.80]", got)
	}
	if got := hosts("10.0.0.132/24"); len(got) != 254 || got[0] != "10.0.0.1" || got[253] != "10.0.0.254" {
		t.Errorf("/24 = %d hosts [%s..%s], want 254 [10.0.0.1..10.0.0.254]", len(got), got[0], got[len(got)-1])
	}
	if got := hosts("10.0.0.1/8"); len(got) != maxHostsPerSubnet {
		t.Errorf("/8 = %d hosts, want the %d cap", len(got), maxHostsPerSubnet)
	}
}

func TestParseProcNetARP(t *testing.T) {
	fixture := "IP address       HW type     Flags       HW address            Mask     Device\n" +
		"10.0.0.80        0x1         0x2         3c:7c:3f:f2:fe:80     *        eth0\n" +
		"10.0.0.152       0x1         0x2         30:56:0f:2c:bc:27     *        eth0\n" +
		"10.0.0.99        0x1         0x0         00:00:00:00:00:00     *        eth0\n"
	got := parseProcNetARP(fixture)
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(got), got)
	}
	if got["10.0.0.80"] != "3c:7c:3f:f2:fe:80" || got["10.0.0.152"] != "30:56:0f:2c:bc:27" {
		t.Errorf("wrong MACs: %v", got)
	}
}

func TestParseDarwinARP(t *testing.T) {
	fixture := "? (10.0.0.80) at 3c:7c:3f:f2:fe:80 on en0 ifscope [ethernet]\n" +
		"jarvis (10.0.0.152) at 30:56:f:2c:bc:27 on en0 ifscope [ethernet]\n" +
		"? (10.0.0.99) at (incomplete) on en0 ifscope [ethernet]\n"
	got := parseDarwinARP(fixture)
	if len(got) != 2 {
		t.Fatalf("parsed %d entries, want 2: %v", len(got), got)
	}
	if got["10.0.0.152"] != "30:56:0f:2c:bc:27" {
		t.Errorf("non-padded MAC not normalised: %v", got)
	}
}

// TestFetchBackendReadOnly proves the section 14.4 guarantee: probing a
// backend issues GETs only, and parses version/tags/ps correctly.
func TestFetchBackendReadOnly(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.13.0"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[
				{"name":"big:35b","size":21000000000,"details":{"parameter_size":"35B","quantization_level":"Q4_K_M"}},
				{"name":"small:4b","size":2500000000,"details":{"parameter_size":"4B","quantization_level":"Q4_K_M"}}
			]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"big:35b","size_vram":20000000000}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b, err := fetchBackend(srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("discovery issued a %s — read-only guarantee broken", m)
		}
	}
	if b.Version != "0.13.0" || len(b.Models) != 2 {
		t.Fatalf("parsed %+v", b)
	}
	if !b.Models[0].Loaded || b.Models[0].SizeVRAM != 20000000000 || b.Models[1].Loaded {
		t.Errorf("loaded state wrong: %+v", b.Models)
	}
	if got := smallestModel(b); got != "small:4b" {
		t.Errorf("smallestModel = %q, want small:4b", got)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"Anton.local":       "anton",
		"JARVIS":            "jarvis",
		"my pc!.home":       "mypc",
		"":                  "",
		"---":               "",
		"weird_name (copy)": "weirdnamecopy",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func fixtureBackends() []*DiscoveredBackend {
	return []*DiscoveredBackend{
		{Name: "anton", IP: "10.0.0.80", URL: "http://10.0.0.80:11434",
			MAC: "3c:7c:3f:f2:fe:80", Broadcast: "10.0.0.255",
			Models: []DiscoveredModel{
				{Name: "qwen3.5:9b", Size: 6000000000},
				{Name: "qwen3.5:4b", Size: 2500000000},
			}},
		{Name: "backend-1", IP: "10.0.0.152", URL: "http://10.0.0.152:11434",
			Broadcast: "10.0.0.255",
			Models:    []DiscoveredModel{{Name: "qwen3:14b", Size: 9000000000}}},
		{Name: "empty", IP: "10.0.0.200", URL: "http://10.0.0.200:11434",
			Broadcast: "10.0.0.255"},
	}
}

func TestGenerateConfigGolden(t *testing.T) {
	now := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	got := generateConfig(fixtureBackends(), now)
	want := `# Generated by gondola discover on 2026-11-22T19:00:00Z — review before use.
# Backend names and default models are starting points, not gospel.

backends:
  anton:
    url: http://10.0.0.80:11434
    mac: "3c:7c:3f:f2:fe:80"
    wol_broadcasts: [10.0.0.255, 255.255.255.255]
    default_model: qwen3.5:4b # guessed by discover — review
  backend-1:
    url: http://10.0.0.152:11434
    default_model: qwen3:14b # guessed by discover — review

# Roles are never guessed — uncomment and name the model each job
# should use. A role may also carry per-backend overrides; see
# SPEC.md section 4.
# roles:
#   brain: <model>
#   heavy: <model>
#   coder: <model>
#   utility: <model>

default_mode: home
modes:
  home:
    default: anton
    wake: false
  away:
    default: anton
    wake: true
  test:
    default: anton
    wake: false
    pin_default_model: true
  dev:
    default: anton
    wake: true
    ttl: 8h

server:
  listen: 0.0.0.0:11435
  state_file: /data/mode.json
  wake_timeout: 150s
  admin_token: ""
`
	if got != want {
		t.Errorf("generated config differs from golden:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "empty") {
		t.Error("zero-model backend leaked into the generated config")
	}
}

// TestGeneratedConfigLoads proves the generated file is a valid gondola
// config under the real loader and validator.
func TestGeneratedConfigLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatcher.yaml")
	gen := generateConfig(fixtureBackends(), time.Now())
	if err := os.WriteFile(path, []byte(gen), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if len(cfg.Backends) != 2 || cfg.DefaultMode != "home" {
		t.Errorf("loaded config wrong: %+v", cfg)
	}
}
