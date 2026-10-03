package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type runtimeLifecycle struct {
	active      atomic.Int64
	draining    atomic.Bool
	stopWorkers context.CancelFunc
	workersDone <-chan struct{}
}

var runtimeBinaryIdentity struct {
	sync.Once
	digest string
}

func runtimeExecutableDigest() string {
	runtimeBinaryIdentity.Do(func() {
		name, err := os.Executable()
		if err != nil {
			return
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return
		}
		sum := sha256.Sum256(raw)
		runtimeBinaryIdentity.digest = hex.EncodeToString(sum[:])
	})
	return runtimeBinaryIdentity.digest
}

func runtimeLifecycleHandler(p *Proxy, w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/_internal/lifecycle" && r.URL.Path != "/_internal/drain" {
		return false
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	token := os.Getenv("CODEX_LIFECYCLE_TOKEN")
	if err != nil || !net.ParseIP(peer).IsLoopback() || len(token) < 32 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Codex-Lifecycle-Token")), []byte(token)) != 1 {
		http.NotFound(w, r)
		return true
	}
	if p.lifecycle == nil {
		http.Error(w, "runtime lifecycle unavailable", 503)
		return true
	}
	if r.URL.Path == "/_internal/drain" && r.Method == http.MethodPost {
		p.lifecycle.draining.Store(true)
		p.lifecycle.stopWorkers()
	} else if r.Method != http.MethodGet || r.URL.Path != "/_internal/lifecycle" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	refreshes := 0
	accountRefreshGates.Lock()
	for _, entry := range accountRefreshGates.entries {
		refreshes += entry.refs
	}
	accountRefreshGates.Unlock()
	pending := 0
	if p.lifecycle.draining.Load() {
		select {
		case <-p.lifecycle.workersDone:
		default:
			pending++
		}
		if p.lifecycle.active.Load() == 0 && refreshes == 0 {
			if p.Metrics != nil && p.Metrics.Save() != nil {
				pending++
			}
			if p.Keys != nil && p.Keys.FlushLastUsed() != nil {
				pending++
			}
		}
	}
	if p.Metrics != nil {
		p.Metrics.mu.Lock()
		pending += len(p.Metrics.pending)
		if p.Metrics.writeErr != nil && pending == 0 {
			pending = 1
		}
		p.Metrics.mu.Unlock()
	}
	if p.IngressEvents != nil {
		pending += int(p.IngressEvents.pending.Load())
		if p.IngressEvents.failed.Load() {
			pending++
		}
	}
	admission := "process_local"
	if p.Store.DB != nil && p.Store.DB.Redis != nil {
		admission = "redis_account_leases_v1"
	}
	writeJSON(w, map[string]any{"pid": os.Getpid(), "version": version, "binary_sha256": runtimeExecutableDigest(), "active_handlers": p.lifecycle.active.Load(), "refresh_operations": refreshes, "pending_persistence": pending, "draining": p.lifecycle.draining.Load(), "admission_mode": admission, "observed_at": time.Now().UTC().Format(time.RFC3339Nano)})
	return true
}
