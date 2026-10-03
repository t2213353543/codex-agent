package server

// The official model directory contains account-specific capabilities, not
// conversation state. Reusing it briefly avoids launching a second App Server
// for every new Responses session. Prompts and inference responses never enter
// this cache, and the official executable remains the directory's sole source.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	officialCatalogTTL        = 2 * time.Minute
	officialCatalogMaxEntries = 64
	officialCatalogMaxBytes   = 32 << 20
)

var errOfficialModelUnavailable = errors.New("requested model is absent from official runtime catalog")

type officialCatalogEntry struct {
	raw     []byte
	expires time.Time
}

type officialCatalogFlight struct {
	done   chan struct{}
	cancel context.CancelFunc
	raw    []byte
	err    error
}

type officialCatalogCache struct {
	mu      sync.Mutex
	entries map[[32]byte]officialCatalogEntry
	flights map[[32]byte]*officialCatalogFlight
	bytes   int
	closed  bool
}

func officialCatalogKey(account Account, proxy *ProxyEntry) [32]byte {
	// Neither access tokens nor proxy passwords are retained in map keys.
	identity, _ := json.Marshal([]any{gatewayAccountRef(account), officialCredentialDigest(account), proxy})
	return sha256.Sum256(identity)
}

func (c *officialCatalogCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.entries = nil
	c.bytes = 0
	for _, flight := range c.flights {
		flight.cancel()
	}
}

func (c *officialCatalogCache) forget(key [32]byte, raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok && bytes.Equal(entry.raw, raw) {
		delete(c.entries, key)
		c.bytes -= len(entry.raw)
	}
}

// Waiting requests can cancel independently. A canceled fetching request does
// not poison the key: remaining callers retry with their own live contexts.
func (c *officialCatalogCache) get(ctx context.Context, key [32]byte, fetch func(context.Context) ([]byte, error)) ([]byte, bool, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, false, errors.New("official runtime closed")
		}
		now := time.Now()
		for k, entry := range c.entries {
			if !now.Before(entry.expires) {
				delete(c.entries, k)
				c.bytes -= len(entry.raw)
			}
		}
		if entry, ok := c.entries[key]; ok {
			raw := bytes.Clone(entry.raw)
			c.mu.Unlock()
			return raw, true, nil
		}
		if flight := c.flights[key]; flight != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-flight.done:
				if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
					continue
				}
				return bytes.Clone(flight.raw), false, flight.err
			}
		}
		if c.entries == nil {
			c.entries = make(map[[32]byte]officialCatalogEntry)
			c.flights = make(map[[32]byte]*officialCatalogFlight)
		}
		fetchCtx, cancel := context.WithTimeout(ctx, time.Minute)
		flight := &officialCatalogFlight{done: make(chan struct{}), cancel: cancel}
		c.flights[key] = flight
		c.mu.Unlock()

		raw, err := fetch(fetchCtx)
		if err == nil {
			err = fetchCtx.Err()
		}
		cancel()
		if err == nil && (len(raw) == 0 || len(raw) > officialCatalogMaxBytes) {
			err = errors.New("official model directory is empty or too large")
		}
		c.mu.Lock()
		if c.closed && err == nil {
			err = errors.New("official runtime closed")
		}
		if err == nil {
			for len(c.entries) >= officialCatalogMaxEntries || c.bytes+len(raw) > officialCatalogMaxBytes {
				var oldest [32]byte
				var expires time.Time
				for k, entry := range c.entries {
					if expires.IsZero() || entry.expires.Before(expires) {
						oldest, expires = k, entry.expires
					}
				}
				c.bytes -= len(c.entries[oldest].raw)
				delete(c.entries, oldest)
			}
			raw = bytes.Clone(raw)
			c.entries[key] = officialCatalogEntry{raw: raw, expires: time.Now().Add(officialCatalogTTL)}
			c.bytes += len(raw)
			flight.raw = raw
		}
		flight.err = err
		delete(c.flights, key)
		close(flight.done)
		c.mu.Unlock()
		return bytes.Clone(flight.raw), false, err
	}
}

func (rt *officialRuntime) clientToolModelCatalog(ctx context.Context, account Account, proxy *ProxyEntry, model string) ([]byte, []byte, error) {
	key := officialCatalogKey(account, proxy)
	fetch := func(ctx context.Context) ([]byte, error) {
		bootstrap, err := rt.startRPC(ctx, account, proxy)
		if err != nil {
			return nil, err
		}
		defer bootstrap.close()
		if _, err = bootstrap.call(ctx, "model/list", map[string]any{"limit": 100, "includeHidden": true}); err != nil {
			return nil, err
		}
		file, err := os.Open(filepath.Join(bootstrap.home, "models_cache.json"))
		if os.IsNotExist(err) {
			// stdout and stderr are separate pipes. Give the bounded diagnostic
			// classifier a moment to observe the already completed model fetch.
			if !bootstrap.modelAuthDiagnostic.unauthorized.Load() {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
			}
			if bootstrap.modelAuthDiagnostic.unauthorized.Load() {
				updated, refreshErr := bootstrap.recoverAuth(ctx)
				if refreshErr != nil {
					return nil, officialAuthenticationUnavailable(refreshErr)
				}
				bootstrap.modelAuthDiagnostic.unauthorized.Store(false)
				if _, err = bootstrap.call(ctx, "account/login/start", map[string]any{"type": "chatgptAuthTokens", "accessToken": updated.AccessToken, "chatgptAccountId": updated.AccountID}); err != nil {
					return nil, err
				}
				if _, err = bootstrap.call(ctx, "model/list", map[string]any{"limit": 100, "includeHidden": true}); err != nil {
					return nil, err
				}
				file, err = os.Open(filepath.Join(bootstrap.home, "models_cache.json"))
			}
		}
		if err != nil {
			if bootstrap.modelAuthDiagnostic.unauthorized.Load() {
				return nil, officialAuthenticationUnavailable(errors.New("official model directory rejected refreshed credentials"))
			}
			return nil, errors.New("official model directory is unavailable")
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, officialCatalogMaxBytes+1))
		if err != nil || len(raw) > officialCatalogMaxBytes {
			return nil, errors.New("official model directory is unavailable or too large")
		}
		// Validate all identities before sharing even when the initial request
		// names a model that the account cannot use.
		var catalog struct {
			Models []struct{ Slug string } `json:"models"`
		}
		if json.Unmarshal(raw, &catalog) != nil || len(catalog.Models) == 0 {
			return nil, errors.New("official model directory is invalid or empty")
		}
		if _, err = officialClientToolCatalog(raw, catalog.Models[0].Slug); err != nil {
			return nil, err
		}
		return raw, nil
	}
	cache, cached, err := rt.catalog.get(ctx, key, fetch)
	if err != nil {
		return nil, nil, err
	}
	catalog, err := officialClientToolCatalog(cache, model)
	if err != nil && cached {
		// A just-enabled model must not be rejected by a stale directory.
		rt.catalog.forget(key, cache)
		cache, _, err = rt.catalog.get(ctx, key, fetch)
		if err == nil {
			catalog, err = officialClientToolCatalog(cache, model)
		}
	}
	return cache, catalog, err
}
