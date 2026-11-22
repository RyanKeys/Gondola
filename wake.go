package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// reachable reports whether a backend answers GET /api/version.
func reachable(client *http.Client, base string) bool {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/api/version", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// sendMagic sends one Wake-on-LAN magic packet for mac to each broadcast
// address on UDP port 9.
func sendMagic(mac string, broadcasts []string) error {
	macBytes, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("bad mac %q: %w", mac, err)
	}
	packet := make([]byte, 0, 102)
	for i := 0; i < 6; i++ {
		packet = append(packet, 0xff)
	}
	for i := 0; i < 16; i++ {
		packet = append(packet, macBytes...)
	}
	if len(broadcasts) == 0 {
		broadcasts = []string{"255.255.255.255"}
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, addr := range broadcasts {
		ua, err := net.ResolveUDPAddr("udp", addr+":9")
		if err != nil {
			continue
		}
		_, _ = conn.WriteToUDP(packet, ua)
	}
	return nil
}

// waker coalesces wake attempts per backend.
type waker struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func newWaker() *waker { return &waker{locks: map[string]*sync.Mutex{}} }

func (w *waker) lockFor(name string) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()
	l, ok := w.locks[name]
	if !ok {
		l = &sync.Mutex{}
		w.locks[name] = l
	}
	return l
}

// ensureUp returns true when the backend is answering. When it is down,
// it wakes it only if the backend has a MAC and the mode allows waking
// (SPEC.md section 5.2); otherwise it returns false immediately.
func (d *Dispatcher) ensureUp(name string, mode ModeCfg) bool {
	b := d.cfg.Backends[name]
	probe := &http.Client{Timeout: 2 * time.Second}
	if reachable(probe, b.URL) {
		return true
	}
	if b.MAC == "" || !mode.Wake {
		return false
	}
	lock := d.waker.lockFor(name)
	lock.Lock()
	defer lock.Unlock()
	if reachable(probe, b.URL) {
		return true
	}
	d.logf("WAKE backend=%s mac=%s", name, b.MAC)
	deadline := time.Now().Add(d.cfg.WakeTimeoutDur)
	lastSent := time.Time{}
	for time.Now().Before(deadline) {
		if time.Since(lastSent) >= 30*time.Second {
			if err := sendMagic(b.MAC, b.WOLBroadcasts); err != nil {
				d.logf("WAKE-FAIL backend=%s err=%v", name, err)
				return false
			}
			lastSent = time.Now()
		}
		time.Sleep(5 * time.Second)
		if reachable(&http.Client{Timeout: 3 * time.Second}, b.URL) {
			d.logf("WAKE-OK backend=%s", name)
			return true
		}
	}
	return false
}
