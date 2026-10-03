package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	gatewayRefreshFailureTTL   = 4 * time.Second
	gatewayRefreshFailureMax   = time.Minute
	gatewayRefreshFailureLimit = 1024
	gatewayRefreshRedisTimeout = 250 * time.Millisecond
)

// Keep only fixed error classes and status codes. Neither the OAuth response
// body nor a URL/credential-bearing network error may enter logs or Redis.
type gatewayOAuthRefreshError struct {
	Kind              string
	StatusCode        int
	Permanent         bool
	RetryAfter        time.Duration
	RetryAfterPresent bool
}

func (e *gatewayOAuthRefreshError) Error() string {
	switch e.Kind {
	case "http":
		if e.Permanent {
			return fmt.Sprintf("OAuth refresh returned HTTP %d; re-import credentials", e.StatusCode)
		}
		if e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500 {
			return fmt.Sprintf("OAuth refresh returned HTTP %d; retry later", e.StatusCode)
		}
		return fmt.Sprintf("OAuth refresh returned HTTP %d; re-import credentials", e.StatusCode)
	case "invalid":
		return "invalid OAuth refresh response"
	default:
		return "OAuth refresh connection failed"
	}
}

type gatewayRefreshFailure struct {
	Kind       string `json:"kind"`
	StatusCode int    `json:"status"`
	Until      int64  `json:"until"`
	use        uint64
}

func (f gatewayRefreshFailure) err(now time.Time) error {
	return &gatewayOAuthRefreshError{Kind: f.Kind, StatusCode: f.StatusCode, RetryAfter: time.UnixMilli(f.Until).Sub(now), RetryAfterPresent: true}
}

func (f gatewayRefreshFailure) valid(now time.Time) bool {
	remaining := time.UnixMilli(f.Until).Sub(now)
	if remaining <= 0 || remaining > gatewayRefreshFailureMax {
		return false
	}
	switch f.Kind {
	case "http":
		return f.StatusCode >= 300 && f.StatusCode <= 599
	case "connection", "invalid":
		return true
	}
	return false
}

type gatewayRefreshCooldown struct {
	mu      sync.Mutex
	entries map[string]gatewayRefreshFailure
	// Like Codex AuthManager, terminal grant failures are process-local and
	// apply only while the same credentials remain current. One slot per
	// account replaces its previous snapshot; no historical token keys grow.
	permanent           map[string]gatewayPermanentRefreshFailure
	permanentGeneration uint64
	use                 uint64
}

type gatewayPermanentRefreshFailure struct {
	Snapshot   string
	Status     int
	Generation uint64
}

func gatewayPermanentRefreshAccountKey(account Account) string {
	ref := account.ID
	if ref == "" {
		ref = "name:" + account.Name
	}
	owner := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(owner[:])
}

func gatewayPermanentRefreshKeys(account Account, endpoint string) (string, string) {
	// A metadata version, display-name edit, or local owner-revision change
	// must not authorize another grant with the same rejected credentials.
	raw, _ := json.Marshal([]string{account.AccessToken, account.RefreshToken, account.ClientID, endpoint})
	snapshot := sha256.Sum256(raw)
	return gatewayPermanentRefreshAccountKey(account), hex.EncodeToString(snapshot[:])
}

func (c *gatewayRefreshCooldown) permanentFailure(accountKey, snapshot string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	failure, exists := c.permanent[accountKey]
	if !exists {
		return nil
	}
	if failure.Snapshot != snapshot {
		delete(c.permanent, accountKey)
		return nil
	}
	return &gatewayOAuthRefreshError{Kind: "http", StatusCode: failure.Status, Permanent: true}
}

func (c *gatewayRefreshCooldown) rememberPermanent(accountKey, snapshot string, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.permanent == nil {
		c.permanent = make(map[string]gatewayPermanentRefreshFailure)
	}
	c.permanentGeneration++
	c.permanent[accountKey] = gatewayPermanentRefreshFailure{Snapshot: snapshot, Status: status, Generation: c.permanentGeneration}
}

func (c *gatewayRefreshCooldown) permanentWatermark() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.permanentGeneration
}

// Prune deleted account IDs only after a fresh successful store listing.
// A failure inserted/updated while that listing was read may belong to a newly
// added account missing from the older snapshot, so preserve newer generations.
// Never use listed credentials to clear a still-present account's failure.
func (c *gatewayRefreshCooldown) prunePermanentOwners(accounts []Account, watermark uint64) {
	c.mu.Lock()
	empty := len(c.permanent) == 0
	c.mu.Unlock()
	if empty {
		return
	}
	present := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		present[gatewayPermanentRefreshAccountKey(account)] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, failure := range c.permanent {
		if !present[key] && failure.Generation <= watermark {
			delete(c.permanent, key)
		}
	}
}

// This classifier is used only for /oauth/token grant responses. A model or
// usage 401 token_revoked does not establish that the refresh token is unusable.
// Match Codex 0.155.0 try_refresh_token/classify_refresh_token_failure.
func gatewayPermanentOAuthGrantFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return false
	}
	readCode := func(raw json.RawMessage) (string, bool) {
		var value string
		if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &value) != nil {
			return "", false
		}
		return value, true
	}
	var code string
	var found bool
	if raw := root["error"]; len(raw) != 0 {
		if code, found = readCode(raw); !found {
			var nested map[string]json.RawMessage
			if json.Unmarshal(raw, &nested) == nil {
				code, found = readCode(nested["code"])
			}
		}
	}
	if !found {
		code, found = readCode(root["code"])
	}
	if !found {
		return false
	}
	switch strings.ToLower(code) {
	case "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	case "invalid_grant":
		return status == http.StatusBadRequest
	}
	return false
}

func gatewayRefreshFailureKey(account Account, endpoint string) string {
	// Metadata Version changes on failed PG refreshes and management edits.
	// CredentialVersion changes only when credentials are replaced. Legacy
	// file stores have no persisted credential epoch; actual AT/RT still scope
	// their failures without making a rename or error-note bypass the cooldown.
	encoded, _ := json.Marshal(struct {
		Identity, AccessToken, RefreshToken, ClientID, Endpoint string
		CredentialVersion                                       int64
	}{gatewayAccountRef(account), account.AccessToken, account.RefreshToken, account.ClientID, endpoint, account.CredentialVersion})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (c *gatewayRefreshCooldown) get(key string, now time.Time) (gatewayRefreshFailure, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, exists := c.entries[key]
	if !exists {
		return gatewayRefreshFailure{}, false
	}
	if !entry.valid(now) {
		delete(c.entries, key)
		return gatewayRefreshFailure{}, false
	}
	c.use++
	entry.use = c.use
	c.entries[key] = entry
	return entry, true
}

func (c *gatewayRefreshCooldown) put(key string, entry gatewayRefreshFailure, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]gatewayRefreshFailure)
	}
	for k, cached := range c.entries {
		if !cached.valid(now) {
			delete(c.entries, k)
		}
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= gatewayRefreshFailureLimit {
		var oldestKey string
		var oldestUse uint64
		for k, cached := range c.entries {
			if oldestUse == 0 || cached.use < oldestUse {
				oldestKey, oldestUse = k, cached.use
			}
		}
		delete(c.entries, oldestKey)
	}
	c.use++
	entry.use = c.use
	c.entries[key] = entry
}

func (c *gatewayRefreshCooldown) clear(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (p *Proxy) cachedGatewayRefreshFailure(ctx context.Context, key string) (gatewayRefreshFailure, bool) {
	now := time.Now()
	if failure, ok := p.refreshCooldown.get(key, now); ok {
		return failure, true
	}
	db := p.Store.DB
	if db == nil || db.Redis == nil {
		return gatewayRefreshFailure{}, false
	}
	lookup, cancel := context.WithTimeout(ctx, gatewayRefreshRedisTimeout)
	defer cancel()
	raw, err := db.Redis.Get(lookup, db.Namespace+"gateway:oauth-failure:"+key).Bytes()
	if err != nil {
		// This optimization must not override Store.Refresh's authoritative
		// credential lock/CAS when Redis is unavailable.
		return gatewayRefreshFailure{}, false
	}
	var failure gatewayRefreshFailure
	if json.Unmarshal(raw, &failure) != nil || !failure.valid(now) {
		return gatewayRefreshFailure{}, false
	}
	p.refreshCooldown.put(key, failure, now)
	return failure, true
}

// Called inside Store.Refresh's existing account lock. Publishing a failure
// before returning makes the next waiting callback reuse it across processes,
// while Store CAS remains solely responsible for accepting rotated tokens.
func (p *Proxy) refreshAccountWithCooldown(ctx context.Context, client *http.Client, account Account, settings Settings) (Account, error) {
	if err := accountRefreshPreflight(ctx); err != nil {
		return account, err
	}
	accountKey, snapshot := gatewayPermanentRefreshKeys(account, p.TokenURL)
	if err := p.refreshCooldown.permanentFailure(accountKey, snapshot); err != nil {
		return account, err
	}
	key := gatewayRefreshFailureKey(account, p.TokenURL)
	if failure, ok := p.cachedGatewayRefreshFailure(ctx, key); ok {
		return account, failure.err(time.Now())
	}
	if err := accountRefreshPreflight(ctx); err != nil {
		return account, err
	}
	updated, err := refreshAccountWithSettings(ctx, client, p.TokenURL, account, settings)
	if err == nil {
		p.refreshCooldown.clear(key)
		if db := p.Store.DB; db != nil && db.Redis != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayRefreshRedisTimeout)
			defer cancel()
			_ = db.Redis.Del(cleanup, db.Namespace+"gateway:oauth-failure:"+key).Err()
		}
		return updated, nil
	}
	var failure *gatewayOAuthRefreshError
	if !errors.As(err, &failure) {
		return account, err
	}
	if failure.Permanent {
		// The admitted exchange has its own bounded context. A client leaving
		// after admission must not erase an observed terminal grant response.
		p.refreshCooldown.rememberPermanent(accountKey, snapshot, failure.StatusCode)
		return account, err
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return account, err
	}
	ttl := gatewayRefreshFailureTTL
	if failure.RetryAfterPresent {
		ttl = min(failure.RetryAfter, gatewayRefreshFailureMax)
	}
	if ttl <= 0 {
		return account, err
	}
	now := time.Now()
	entry := gatewayRefreshFailure{Kind: failure.Kind, StatusCode: failure.StatusCode, Until: now.Add(ttl).UnixMilli()}
	p.refreshCooldown.put(key, entry, now)
	if db := p.Store.DB; db != nil && db.Redis != nil {
		publish, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayRefreshRedisTimeout)
		defer cancel()
		encoded, _ := json.Marshal(entry)
		_ = db.Redis.Set(publish, db.Namespace+"gateway:oauth-failure:"+key, encoded, ttl).Err()
	}
	return account, err
}
