// gondola: a mode-aware routing proxy for multiple Ollama
// machines. See SPEC.md. One binary: `serve` runs the proxy; the other
// subcommands are a thin admin client for a running instance.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "mode":
		if err := runMode(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "mode:", err)
			os.Exit(1)
		}
	case "modes":
		if err := runModes(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "modes:", err)
			os.Exit(1)
		}
	case "health":
		if err := runHealth(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "health:", err)
			os.Exit(1)
		}
	case "discover":
		os.Exit(runDiscover(os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  gondola serve --config dispatcher.yaml")
	fmt.Fprintln(os.Stderr, "  gondola mode [--addr http://127.0.0.1:11435] [name]")
	fmt.Fprintln(os.Stderr, "  gondola modes [--addr ...]")
	fmt.Fprintln(os.Stderr, "  gondola health [--addr ...]")
	fmt.Fprintln(os.Stderr, "  gondola discover [--timeout 300ms] [--port 11434] [--json] [--write-config dispatcher.yaml]")
}

// flagVal extracts --name value from args.
func flagVal(args []string, name, def string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--"+name {
			return args[i+1]
		}
	}
	return def
}

func runServe(args []string) error {
	cfgPath := flagVal(args, "config", "dispatcher.yaml")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	d := NewDispatcher(cfg)
	d.cfgPath = cfgPath
	mode, _ := d.modes.Current()
	d.logf("gondola %s listening on %s (mode=%s, backends=%s)",
		version, cfg.Server.Listen, mode, strings.Join(cfg.BackendNames(), ","))
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: d.Routes()}
	return srv.ListenAndServe()
}

func clientGet(addr, path string) (map[string]any, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get(strings.TrimRight(addr, "/") + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %s", path, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

func runMode(args []string) error {
	addr := flagVal(args, "addr", "http://127.0.0.1:11435")
	// first positional arg, if any, is the mode to set
	var target string
	for _, a := range args {
		if !strings.HasPrefix(a, "--") && a != addr {
			target = a
		}
	}
	if target == "" {
		out, err := clientGet(addr, "/mode")
		if err != nil {
			return err
		}
		fmt.Println(out["mode"])
		return nil
	}
	c := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(addr, "/")+"/mode",
		strings.NewReader(fmt.Sprintf(`{"mode":%q}`, target)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := os.Getenv("DISPATCHER_ADMIN_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	fmt.Println(target)
	return nil
}

func runModes(args []string) error {
	addr := flagVal(args, "addr", "http://127.0.0.1:11435")
	out, err := clientGet(addr, "/modes")
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(out["modes"], "", "  ")
	fmt.Println(string(raw))
	return nil
}

func runHealth(args []string) error {
	addr := flagVal(args, "addr", "http://127.0.0.1:11435")
	out, err := clientGet(addr, "/health")
	if err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
	return nil
}
