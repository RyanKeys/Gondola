package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// Discovery (SPEC.md section 14): `gondola discover` inventories the
// Ollama backends on the operator's own LAN and drafts a starter
// config. It is strictly read-only against the network: no WoL packets,
// no POSTs, nothing that would load a model — only TCP probes and
// GET /api/version, /api/tags, /api/ps.

const (
	discoverWorkers     = 64
	maxHostsPerSubnet   = 4096
	discoverHTTPTimeout = 3 * time.Second
)

// DiscoveredModel is one model on a discovered backend.
type DiscoveredModel struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	ParameterSize string `json:"parameter_size,omitempty"`
	Quantization  string `json:"quantization,omitempty"`
	Loaded        bool   `json:"loaded"`
	SizeVRAM      int64  `json:"size_vram,omitempty"`
}

// DiscoveredBackend is one Ollama host found by a scan.
type DiscoveredBackend struct {
	Name      string            `json:"name"`
	IP        string            `json:"ip"`
	URL       string            `json:"url"`
	Hostname  string            `json:"hostname,omitempty"`
	Version   string            `json:"version,omitempty"`
	MAC       string            `json:"mac,omitempty"`
	Broadcast string            `json:"broadcast"`
	LatencyMS int64             `json:"latency_ms"`
	Models    []DiscoveredModel `json:"models"`
}

// subnet is one directly attached local network to scan.
type subnet struct {
	iface     string
	ip        net.IP
	mask      net.IPMask
	broadcast net.IP
}

// broadcastAddr returns ip | ^mask.
func broadcastAddr(ip net.IP, mask net.IPMask) net.IP {
	ip = ip.To4()
	if ip == nil {
		return nil
	}
	if len(mask) == net.IPv6len {
		mask = mask[net.IPv6len-net.IPv4len:]
	}
	out := make(net.IP, len(ip))
	for i := range ip {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}

// enumerateHosts lists the addresses to probe in a subnet: the usable
// host range for normal prefixes, both addresses for /31, the single
// address for /32, capped at maxHostsPerSubnet.
func enumerateHosts(ip net.IP, mask net.IPMask) []net.IP {
	ones, bits := mask.Size()
	if bits != 32 {
		return nil
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil
	}
	base := binary32(ip4) & binary32(net.IP(mask))
	switch ones {
	case 32:
		return []net.IP{ip4}
	case 31:
		return []net.IP{ipFrom32(base), ipFrom32(base + 1)}
	}
	count := (1 << (32 - ones)) - 2
	if count > maxHostsPerSubnet {
		count = maxHostsPerSubnet
	}
	hosts := make([]net.IP, 0, count)
	for i := 1; i <= count; i++ {
		hosts = append(hosts, ipFrom32(base+uint32(i)))
	}
	return hosts
}

func binary32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func ipFrom32(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// localSubnets returns the host's up, non-loopback IPv4 networks.
// Virtual plumbing (docker bridges, veth pairs) is skipped: backends on
// those networks are reached via the host's LAN address anyway, and
// sweeping a docker /16 would take minutes for nothing. Link-local is
// skipped for the same reason.
func localSubnets() []subnet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []subnet
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") ||
			strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			key := ipnet.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, subnet{
				iface:     ifc.Name,
				ip:        ipnet.IP.To4(),
				mask:      ipnet.Mask,
				broadcast: broadcastAddr(ipnet.IP.To4(), ipnet.Mask),
			})
		}
	}
	return out
}

// --- probing ---------------------------------------------------------------

type tagsResponse struct {
	Models []struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Details struct {
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

type psResponse struct {
	Models []struct {
		Name     string `json:"name"`
		SizeVRAM int64  `json:"size_vram"`
	} `json:"models"`
}

func httpGetJSON(client *http.Client, url string, out any) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// fetchBackend reads version, tags, and ps from one Ollama base URL.
// GETs only — this is the read-only guarantee of section 14.4.
func fetchBackend(client *http.Client, baseURL string) (*DiscoveredBackend, error) {
	b := &DiscoveredBackend{URL: baseURL}
	var ver struct {
		Version string `json:"version"`
	}
	if err := httpGetJSON(client, baseURL+"/api/version", &ver); err != nil {
		return nil, err
	}
	b.Version = ver.Version
	var tags tagsResponse
	if err := httpGetJSON(client, baseURL+"/api/tags", &tags); err != nil {
		return nil, err
	}
	loaded := map[string]int64{}
	var ps psResponse
	if err := httpGetJSON(client, baseURL+"/api/ps", &ps); err == nil {
		for _, m := range ps.Models {
			loaded[m.Name] = m.SizeVRAM
		}
	}
	for _, m := range tags.Models {
		dm := DiscoveredModel{
			Name:          m.Name,
			Size:          m.Size,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.QuantizationLevel,
		}
		if vram, ok := loaded[m.Name]; ok {
			dm.Loaded = true
			dm.SizeVRAM = vram
		}
		b.Models = append(b.Models, dm)
	}
	return b, nil
}

// probeHost dials one address; if the port is open it fetches the
// backend's details. Returns nil when nothing answers.
func probeHost(ip net.IP, port int, timeout time.Duration) *DiscoveredBackend {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), fmt.Sprint(port)), timeout)
	if err != nil {
		return nil
	}
	conn.Close()
	client := &http.Client{Timeout: discoverHTTPTimeout}
	b, err := fetchBackend(client, fmt.Sprintf("http://%s:%d", ip, port))
	if err != nil {
		return nil
	}
	b.IP = ip.String()
	b.LatencyMS = time.Since(start).Milliseconds()
	return b
}

// scan probes every host in the given subnets with a bounded pool.
func scan(subnets []subnet, port int, timeout time.Duration) []*DiscoveredBackend {
	type target struct {
		ip        net.IP
		broadcast string
	}
	var targets []target
	for _, sn := range subnets {
		for _, h := range enumerateHosts(sn.ip, sn.mask) {
			targets = append(targets, target{h, sn.broadcast.String()})
		}
	}
	jobs := make(chan target)
	results := make(chan *DiscoveredBackend)
	var wg sync.WaitGroup
	for i := 0; i < discoverWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				if b := probeHost(t.ip, port, timeout); b != nil {
					b.Broadcast = t.broadcast
					results <- b
				}
			}
		}()
	}
	go func() {
		for _, t := range targets {
			jobs <- t
		}
		close(jobs)
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	var found []*DiscoveredBackend
	for b := range results {
		found = append(found, b)
	}
	sort.Slice(found, func(i, j int) bool {
		return binary32(net.ParseIP(found[i].IP)) < binary32(net.ParseIP(found[j].IP))
	})
	return found
}

// --- neighbour table (MAC addresses) ---------------------------------------

// neighbourMACs returns IP → MAC from the OS neighbour table. Read
// after probing, so the scan's own traffic has populated it.
func neighbourMACs() map[string]string {
	switch runtime.GOOS {
	case "linux":
		raw, err := os.ReadFile("/proc/net/arp")
		if err != nil {
			return nil
		}
		return parseProcNetARP(string(raw))
	case "darwin":
		out, err := exec.Command("arp", "-a").Output()
		if err != nil {
			return nil
		}
		return parseDarwinARP(string(out))
	}
	return nil
}

// parseProcNetARP parses Linux /proc/net/arp content.
func parseProcNetARP(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || net.ParseIP(f[0]) == nil {
			continue
		}
		mac := strings.ToLower(f[3])
		if mac == "00:00:00:00:00:00" {
			continue
		}
		out[f[0]] = mac
	}
	return out
}

// parseDarwinARP parses macOS `arp -a` output, normalising the
// non-zero-padded MAC octets macOS prints.
func parseDarwinARP(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		open := strings.Index(line, "(")
		clos := strings.Index(line, ")")
		if open < 0 || clos < 0 || clos < open {
			continue
		}
		ip := line[open+1 : clos]
		if net.ParseIP(ip) == nil {
			continue
		}
		rest := line[clos+1:]
		at := strings.Index(rest, " at ")
		if at < 0 {
			continue
		}
		macField := strings.Fields(rest[at+4:])
		if len(macField) == 0 || macField[0] == "(incomplete)" {
			continue
		}
		parts := strings.Split(macField[0], ":")
		if len(parts) != 6 {
			continue
		}
		for i, p := range parts {
			if len(p) == 1 {
				parts[i] = "0" + p
			}
		}
		out[ip] = strings.ToLower(strings.Join(parts, ":"))
	}
	return out
}

// --- naming and config generation ------------------------------------------

// sanitizeName turns a hostname into a backend name: first DNS label,
// lowercased, [a-z0-9-] only.
func sanitizeName(hostname string) string {
	label := strings.Split(hostname, ".")[0]
	label = strings.ToLower(label)
	var sb strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sb.WriteRune(r)
		}
	}
	return strings.Trim(sb.String(), "-")
}

// assignNames names backends from their hostnames where possible,
// falling back to backend-1, backend-2, ... in scan order.
func assignNames(found []*DiscoveredBackend) {
	taken := map[string]bool{}
	n := 0
	for _, b := range found {
		name := sanitizeName(b.Hostname)
		if name == "" || taken[name] {
			for {
				n++
				name = fmt.Sprintf("backend-%d", n)
				if !taken[name] {
					break
				}
			}
		}
		taken[name] = true
		b.Name = name
	}
}

// smallestModel returns the name of the backend's smallest model.
func smallestModel(b *DiscoveredBackend) string {
	best := ""
	var bestSize int64
	for _, m := range b.Models {
		if best == "" || m.Size < bestSize {
			best, bestSize = m.Name, m.Size
		}
	}
	return best
}

// generateConfig renders a starter dispatcher.yaml from scan results
// (section 14.6). Backends with no models are omitted — they cannot
// serve anything and have no default to guess; the caller reports them.
func generateConfig(found []*DiscoveredBackend, now time.Time) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Generated by gondola discover on %s — review before use.\n",
		now.UTC().Format(time.RFC3339)))
	sb.WriteString("# Backend names and default models are starting points, not gospel.\n\n")
	sb.WriteString("backends:\n")
	var usable []*DiscoveredBackend
	for _, b := range found {
		if len(b.Models) == 0 {
			continue
		}
		usable = append(usable, b)
		fmt.Fprintf(&sb, "  %s:\n", b.Name)
		fmt.Fprintf(&sb, "    url: %s\n", b.URL)
		if b.MAC != "" {
			fmt.Fprintf(&sb, "    mac: %q\n", b.MAC)
			fmt.Fprintf(&sb, "    wol_broadcasts: [%s, 255.255.255.255]\n", b.Broadcast)
		}
		fmt.Fprintf(&sb, "    default_model: %s # guessed by discover — review\n", smallestModel(b))
	}
	sb.WriteString(`
# Roles are never guessed — uncomment and name the model each job
# should use. A role may also carry per-backend overrides; see
# SPEC.md section 4.
# roles:
#   brain: <model>
#   heavy: <model>
#   coder: <model>
#   utility: <model>

`)
	first := usable[0].Name
	fmt.Fprintf(&sb, "default_mode: home\nmodes:\n")
	fmt.Fprintf(&sb, "  home:\n    default: %s\n    wake: false\n", first)
	fmt.Fprintf(&sb, "  away:\n    default: %s\n    wake: true\n", first)
	fmt.Fprintf(&sb, "  test:\n    default: %s\n    wake: false\n    pin_default_model: true\n", first)
	fmt.Fprintf(&sb, "  dev:\n    default: %s\n    wake: true\n    ttl: 8h\n", first)
	sb.WriteString(`
server:
  listen: 0.0.0.0:11435
  state_file: /data/mode.json
  wake_timeout: 150s
  admin_token: ""
`)
	return sb.String()
}

// --- report ------------------------------------------------------------------

func printReport(w io.Writer, found []*DiscoveredBackend) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ADDRESS\tNAME\tVERSION\tMODELS\tLARGEST\tLOADED\tMAC")
	for _, b := range found {
		largest, loaded := "", 0
		var largestSize int64
		for _, m := range b.Models {
			if m.Size > largestSize {
				largest, largestSize = m.Name, m.Size
			}
			if m.Loaded {
				loaded++
			}
		}
		mac := b.MAC
		if mac == "" {
			mac = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%s\n",
			b.IP, b.Name, b.Version, len(b.Models), largest, loaded, mac)
	}
	tw.Flush()
	for _, b := range found {
		if len(b.Models) == 0 {
			fmt.Fprintf(w, "note: %s (%s) answered but has no models installed; left out of generated config\n", b.IP, b.URL)
		}
	}
}

// --- the subcommand ------------------------------------------------------------

func runDiscover(args []string) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 300*time.Millisecond, "per-host dial timeout")
	port := fs.Int("port", 11434, "Ollama port to probe")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	writeCfg := fs.String("write-config", "", "also write a starter config to this path")
	force := fs.Bool("force", false, "overwrite an existing --write-config target")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	subnets := localSubnets()
	if len(subnets) == 0 {
		fmt.Fprintln(os.Stderr, "discover: no usable local networks found")
		return 1
	}
	found := scan(subnets, *port, *timeout)
	macs := neighbourMACs()
	for _, b := range found {
		b.MAC = macs[b.IP]
		if names, err := net.LookupAddr(b.IP); err == nil && len(names) > 0 {
			b.Hostname = strings.TrimSuffix(names[0], ".")
		}
	}
	assignNames(found)
	if len(found) == 0 {
		var nets []string
		for _, sn := range subnets {
			nets = append(nets, sn.ip.String()+"/"+maskString(sn.mask))
		}
		fmt.Fprintf(os.Stderr, "discover: no Ollama backends found on %s port %d — is Ollama running and reachable (OLLAMA_HOST)?\n",
			strings.Join(nets, ", "), *port)
		return 1
	}
	if *asJSON {
		raw, _ := json.MarshalIndent(found, "", "  ")
		fmt.Println(string(raw))
	} else {
		printReport(os.Stdout, found)
	}
	if *writeCfg != "" {
		usable := 0
		for _, b := range found {
			if len(b.Models) > 0 {
				usable++
			}
		}
		if usable == 0 {
			fmt.Fprintln(os.Stderr, "discover: every backend found has zero models; no config written")
			return 1
		}
		if _, err := os.Stat(*writeCfg); err == nil && !*force {
			fmt.Fprintf(os.Stderr, "discover: %s exists — pass --force to overwrite\n", *writeCfg)
			return 1
		}
		if err := os.WriteFile(*writeCfg, []byte(generateConfig(found, time.Now())), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "discover:", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "discover: starter config written to %s — review it before use\n", *writeCfg)
	}
	return 0
}

func maskString(m net.IPMask) string {
	ones, _ := m.Size()
	return fmt.Sprint(ones)
}
