package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Faults known per role. redis_down is not a switch: chaos stops redis.
var knownFaults = map[string][]string{
	"frontend": {"bad_config"},
	"catalog":  {"n_plus_one", "pool_leak"},
	"cart":     {},
}

// Faults is the set of active fault switches for one service.
type Faults struct {
	mu      sync.RWMutex
	allowed []string
	active  map[string]bool
	onSet   func(name string, on bool) // optional hook, called outside the lock
}

// NewFaults parses a comma-separated FAULT value for role.
func NewFaults(role, env string) (*Faults, error) {
	f := &Faults{allowed: knownFaults[role], active: map[string]bool{}}
	for _, name := range strings.Split(env, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(f.allowed, name) {
			return nil, fmt.Errorf("fault %q not valid for %s (valid: %v)", name, role, f.allowed)
		}
		f.active[name] = true
	}
	return f, nil
}

// On reports whether fault name is active.
func (f *Faults) On(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.active[name]
}

// Set toggles a fault.
func (f *Faults) Set(name string, on bool) error {
	if !slices.Contains(f.allowed, name) {
		return fmt.Errorf("unknown fault %q (valid: %v)", name, f.allowed)
	}
	f.mu.Lock()
	f.active[name] = on
	hook := f.onSet
	f.mu.Unlock()
	if hook != nil {
		hook(name, on)
	}
	return nil
}

// Active lists active faults, sorted.
func (f *Faults) Active() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var out []string
	for k, v := range f.active {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// isLocal accepts loopback and private (RFC 1918 / ULA) peers only, which in
// the demo means the host via a 127.0.0.1-bound port or the compose network.
func isLocal(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	return a.IsLoopback() || a.IsPrivate()
}

// AdminHandler serves GET/POST /admin/fault. It is disabled (404) when token
// is empty. POST body: {"fault":"n_plus_one","enabled":true}.
func AdminHandler(f *Faults, token string, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.NotFound(w, r)
			return
		}
		if !isLocal(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		got := r.Header.Get("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			var req struct {
				Fault   string `json:"fault"`
				Enabled bool   `json:"enabled"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := f.Set(req.Fault, req.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Deliberately vague: an investigator should infer the cause from
			// signals, not from a log line that names the fault.
			log.Info("runtime override applied")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"active": f.Active()})
	}
}
