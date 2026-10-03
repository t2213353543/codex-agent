package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proxy routes gateway requests to the upstream Codex API through pooled
// accounts, and serves the admin API for the management console.
type Proxy struct {
	Store               Store
	Groups              *GroupStore
	Keys                *KeyStore
	Proxies             *ProxyStore
	Settings            *SettingsStore
	Metrics             *Metrics
	IngressEvents       *gatewayIngressEventSink
	PromptCacheObserver *gatewayPromptCacheObserver
	AdminKey            string
	KeyFile             string
	Client              *http.Client
	NativeHTTP          *gatewayNativePool
	NativeWebSocket     bool
	BaseURL             string
	BPS                 *bpsRuntime
	RuntimeModes        bool
	Official            *officialRuntime
	CodexTransport      *Proxy
	AppServerCLI        *appServerCLI
	inferenceSessionMu  sync.Mutex
	inferenceSessions   map[string]inferenceSessionBinding
	TokenURL            string
	startedAt           time.Time
	lifecycle           *runtimeLifecycle

	refreshMu       sync.Mutex
	refreshCooldown gatewayRefreshCooldown
	wsAvailability  gatewayWSAvailability
	bpsWSSockets    bpsWSPool
	adminMu         sync.RWMutex
	clientMu        sync.Mutex
	clientPool      map[gatewayClientKey]*gatewayClientEntry
	clientUse       uint64

	admission   map[string]*int64 // immutable account ID -> in-flight requests
	admissionMu sync.Mutex
	roundRobin  map[string]int // account name -> proxy rotation cursor
	roundMu     sync.Mutex

	accountsMu sync.Mutex
	accountsAt time.Time
	accounts   []Account

	sessionMu             sync.Mutex
	sessionRoutes         map[string]gatewaySessionRoute
	cooldownMu            sync.Mutex
	cooldowns             map[string]time.Time
	cooldownOverflowUntil time.Time
}

func (p *Proxy) setAdminKey(key string) {
	p.adminMu.Lock()
	p.AdminKey = key
	p.adminMu.Unlock()
}

func (p *Proxy) adminKey() string {
	p.adminMu.RLock()
	defer p.adminMu.RUnlock()
	return p.AdminKey
}

// identifyKey resolves a bearer token to (keyID, keyName, ok). The admin key
// works as a gateway key while the admin API switch is on.
func (p *Proxy) identifyKey(token string) (string, string, bool) {
	return p.identifyKeyContext(context.Background(), token)
}

func (p *Proxy) identifyKeyContext(ctx context.Context, token string) (string, string, bool) {
	id, name, _, err := p.resolveGatewayKeyContext(ctx, token)
	return id, name, err == nil && id != ""
}

func (p *Proxy) settingsValue() (Settings, error) {
	if p.Settings == nil {
		return defaultSettings(), nil
	}
	return p.Settings.Get()
}

// accountsSnapshot returns the current account list, cached briefly so the
// gateway hot path avoids re-reading and re-parsing the store file.
func (p *Proxy) accountsSnapshot() ([]Account, error) {
	return p.accountsSnapshotObserved(nil)
}

func (p *Proxy) accountsSnapshotObserved(observation *gatewayAccountSnapshotObservation) ([]Account, error) {
	p.accountsMu.Lock()
	defer p.accountsMu.Unlock()
	now := time.Now()
	if len(p.accounts) > 0 || !p.accountsAt.IsZero() {
		if now.Sub(p.accountsAt) < 5*time.Second {
			observation.capture("cache_hit", now, p.accountsAt)
			return append([]Account(nil), p.accounts...), nil
		}
	}
	refreshWatermark := p.refreshCooldown.permanentWatermark()
	_, accounts, err := p.Store.List()
	if err != nil {
		if p.accountsAt.IsZero() {
			return nil, err
		}
		observation.capture("stale_on_read_error", now, p.accountsAt)
		return append([]Account(nil), p.accounts...), nil
	}
	p.refreshCooldown.prunePermanentOwners(accounts, refreshWatermark)
	p.accounts = accounts
	p.accountsAt = now
	observation.capture("fresh_read", now, p.accountsAt)
	return append([]Account(nil), accounts...), nil
}

func (p *Proxy) invalidateAccounts() {
	p.accountsMu.Lock()
	p.accountsAt = time.Time{}
	p.accountsMu.Unlock()
}

// candidatesFor returns the enabled accounts a gateway key may use.
func (p *Proxy) candidatesFor(token string) ([]Account, string, string, error) {
	return p.candidatesForContext(context.Background(), token)
}

func (p *Proxy) candidatesForContext(ctx context.Context, token string) ([]Account, string, string, error) {
	return p.candidatesForContextObserved(ctx, token, nil)
}

func (p *Proxy) candidatesForContextObserved(ctx context.Context, token string, observation *gatewayBoundCandidateSnapshot) ([]Account, string, string, error) {
	keyID, keyName, entry, err := p.resolveGatewayKeyContext(ctx, token)
	if err != nil {
		return nil, "", "", err
	}
	if keyID == "" {
		return nil, "", "", errors.New("invalid gateway key")
	}
	var snapshot *gatewayAccountSnapshotObservation
	if observation != nil {
		snapshot = &observation.snapshot
	}
	accounts, err := p.accountsSnapshotObserved(snapshot)
	if err != nil {
		return nil, "", "", err
	}
	if observation != nil {
		observation.accounts, observation.admin = accounts, keyID == "admin"
	}
	if keyID == "admin" {
		candidates := make([]Account, 0, len(accounts))
		for _, account := range accounts {
			if account.Enabled {
				candidates = append(candidates, account)
			}
		}
		return candidates, keyID, keyName, nil
	}
	allowed := make(map[string]bool, len(entry.Groups))
	for _, group := range entry.Groups {
		allowed[group] = true
	}
	if observation != nil {
		observation.allowed = allowed
	}
	candidates := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if !account.Enabled {
			continue
		}
		for _, group := range account.Groups {
			if allowed[group] {
				candidates = append(candidates, account)
				break
			}
		}
	}
	return candidates, keyID, keyName, nil
}

// acquire/release manage per-account concurrency admission.
func (p *Proxy) acquire(account string, limit int) bool {
	if limit < 1 {
		return false
	}
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	if p.admission == nil {
		p.admission = map[string]*int64{}
	}
	counter := p.admission[account]
	if counter == nil {
		counter = new(int64)
		p.admission[account] = counter
	}
	if *counter >= int64(limit) {
		return false
	}
	*counter++
	return true
}

func (p *Proxy) release(account string) {
	p.admissionMu.Lock()
	if counter := p.admission[account]; counter != nil {
		if *counter > 1 {
			*counter--
		} else {
			delete(p.admission, account)
		}
	}
	p.admissionMu.Unlock()
}

func (p *Proxy) inFlight(account string) int64 {
	// Management callers can supply an immutable key directly. Compatibility
	// callers that only have a name resolve it before touching the counter lock.
	if !strings.HasPrefix(account, "id:") && !strings.HasPrefix(account, "upstream:") &&
		(p.Store.DB == nil || p.Store.DB.Pool != nil) {
		if current, err := p.Store.Get(account); err == nil {
			account = gatewayAdmissionKey(current)
		}
	}
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	if counter := p.admission[account]; counter != nil {
		return *counter
	}
	return 0
}

// pickAccount selects an account for the key's groups: highest priority
// first, weighted random inside a priority level, with concurrency
// admission and on-demand token refresh. It returns the account and a
// release function the caller must run when the request ends.
func (p *Proxy) pickAccount(ctx context.Context, token string) (Account, func(), string, string, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, nil, "", "", err
	}
	candidates, keyID, keyName, err := p.candidatesForContext(ctx, token)
	if err != nil {
		return Account{}, nil, keyID, keyName, err
	}
	if attempts := gatewayAccountFailoverFrom(ctx); attempts != nil {
		remaining := make([]Account, 0, len(candidates))
		for _, account := range candidates {
			if !attempts.contains(account) {
				remaining = append(remaining, account)
			}
		}
		candidates = remaining
	}
	if len(candidates) == 0 {
		if gatewayAccountFailoverFrom(ctx) != nil {
			return Account{}, nil, keyID, keyName, errGatewayAccountCandidatesExhausted
		}
		return Account{}, nil, keyID, keyName, errors.New("no enabled account in the key's groups")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Priority > candidates[j].Priority })
	busy := 0
	var earliestCooldown *gatewayCooldownError
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].Priority == candidates[start].Priority {
			end++
		}
		level := append([]Account(nil), candidates[start:end]...)
		for len(level) > 0 {
			account := gatewayCacheAffinityPick(ctx, level)
			for i := range level {
				if level[i].Name == account.Name {
					level = append(level[:i], level[i+1:]...)
					break
				}
			}
			if attempts := gatewayAccountFailoverFrom(ctx); attempts != nil && !attempts.claim(account) {
				continue
			}
			// Ranking is a scheduling snapshot. Authorization, credentials and
			// admission limits must come from the selected account's current row.
			account, err = p.gatewayCurrentSelectionAccount(ctx, account)
			if err := ctx.Err(); err != nil {
				return Account{}, nil, keyID, keyName, err
			}
			if errors.Is(err, errNotFound) {
				p.invalidateAccounts()
				continue
			}
			if err != nil {
				return Account{}, nil, keyID, keyName, err
			}
			if err := p.checkGatewayAccountAccess(ctx, token, keyID, account); err != nil {
				if errors.Is(err, errGatewayAccountNotAllowed) {
					continue
				}
				return Account{}, nil, keyID, keyName, err
			}
			if err := p.checkGatewayCooldown(ctx, account); err != nil {
				var cooldown *gatewayCooldownError
				if !errors.As(err, &cooldown) {
					return Account{}, nil, keyID, keyName, err
				}
				if earliestCooldown == nil || cooldown.until.Before(earliestCooldown.until) {
					earliestCooldown = cooldown
				}
				continue
			}
			admission, acquired, admissionErr := p.acquireGatewayAccountContext(ctx, account)
			if errors.Is(admissionErr, errAccountDraining) {
				continue
			}
			if admissionErr != nil {
				return Account{}, nil, keyID, keyName, admissionErr
			}
			if !acquired {
				busy++
				continue
			}
			release := admission.Release
			ready, err := p.ensureReady(admission.Context, account)
			if err != nil {
				admissionCause := context.Cause(admission.Context)
				release()
				if gatewaySharedAdmissionFailure(admissionCause) {
					return Account{}, nil, keyID, keyName, admissionCause
				}
				if ctx.Err() != nil {
					return Account{}, nil, keyID, keyName, ctx.Err()
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return Account{}, nil, keyID, keyName, err
				}
				p.noteAccountError(account, err)
				continue
			}
			if err := p.checkGatewayAccountAccess(ctx, token, keyID, ready); err != nil {
				release()
				if errors.Is(err, errGatewayAccountNotAllowed) {
					continue
				}
				return Account{}, nil, keyID, keyName, err
			}
			if err := p.checkGatewayCooldown(ctx, ready); err != nil {
				release()
				var cooldown *gatewayCooldownError
				if !errors.As(err, &cooldown) {
					return Account{}, nil, keyID, keyName, err
				}
				if earliestCooldown == nil || cooldown.until.Before(earliestCooldown.until) {
					earliestCooldown = cooldown
				}
				continue
			}
			return ready, release, keyID, keyName, nil
		}
		start = end
	}
	// A busy, otherwise eligible account can free a slot before another
	// account's rate-limit deadline. Do not advertise that longer deadline.
	if busy > 0 {
		return Account{}, nil, keyID, keyName, &gatewayAccountBusyError{}
	}
	if earliestCooldown != nil {
		return Account{}, nil, keyID, keyName, earliestCooldown
	}
	if gatewayAccountFailoverFrom(ctx) != nil {
		return Account{}, nil, keyID, keyName, errGatewayAccountCandidatesExhausted
	}
	return Account{}, nil, keyID, keyName, errors.New("no account in the key's groups is available")
}

func weightedPick(accounts []Account) Account {
	if len(accounts) == 0 {
		return Account{}
	}
	total := 0
	for _, account := range accounts {
		weight := account.effectiveWeight()
		if total > int(^uint(0)>>1)-weight {
			// Persisted weights may occupy the entire positive int range.
			// Reservoir selection avoids integer overflow without allocating,
			// while the ordinary path below retains exact integer sampling.
			var sum float64
			selected := accounts[0]
			for _, candidate := range accounts {
				weight := float64(candidate.effectiveWeight())
				sum += weight
				if rand.Float64()*sum < weight {
					selected = candidate
				}
			}
			return selected
		}
		total += weight
	}
	roll := rand.IntN(total)
	for _, account := range accounts {
		roll -= account.effectiveWeight()
		if roll < 0 {
			return account
		}
	}
	return accounts[len(accounts)-1]
}

// ensureReady refreshes an expiring token once, serialized so rotating
// refresh tokens are exchanged exactly once across concurrent requests.
func (p *Proxy) ensureReady(ctx context.Context, a Account) (Account, error) {
	if a.ExpiresAt == 0 || a.ExpiresAt > time.Now().Add(time.Minute).Unix() {
		return a, nil
	}
	settings, err := p.settingsValue()
	if err != nil {
		return a, err
	}
	if !settings.AutomaticTokenRefreshEnabled {
		if a.ExpiresAt > time.Now().Unix() {
			return a, nil
		}
		return a, errors.New("access token expired and automatic refresh is disabled; refresh or re-import credentials")
	}
	if a.RefreshToken == "" {
		if a.ExpiresAt > time.Now().Unix() {
			return a, nil
		}
		return a, errors.New("access token expired; re-import credentials")
	}
	// PostgreSQL Store.Refresh owns a per-account lock across the exchange.
	// Keep the legacy whole-file lock only for the file compatibility store.
	if p.Store.DB == nil {
		p.refreshMu.Lock()
		defer p.refreshMu.Unlock()
	}
	ref := a.ID
	if ref == "" {
		ref = a.Name
	}
	current, err := p.gatewayCurrentSelectionAccount(ctx, a)
	if err := ctx.Err(); err != nil {
		return a, err
	}
	if err != nil {
		return a, err
	}
	checkIdentity := func(current Account) error {
		if gatewayAccountOwnerRef(current) != gatewayAccountOwnerRef(a) {
			return errors.New("upstream account changed during token refresh")
		}
		if !current.Enabled {
			return errors.New("upstream account disabled during token refresh")
		}
		return nil
	}
	if err := checkIdentity(current); err != nil {
		return a, err
	}
	if current.ExpiresAt == 0 || current.ExpiresAt > time.Now().Add(time.Minute).Unix() {
		return current, nil
	}
	if current.RefreshToken == "" {
		if current.ExpiresAt > time.Now().Unix() {
			return current, nil
		}
		return current, errors.New("access token expired; re-import credentials")
	}
	updated, err := p.Store.RefreshContext(ctx, ref, current.AccessToken, func(c Account) (Account, error) {
		if err := accountRefreshPreflight(ctx); err != nil {
			return c, err
		}
		if err := checkIdentity(c); err != nil {
			return c, err
		}
		return p.refreshAccountWithFingerprint(ctx, c)
	})
	p.invalidateAccounts()
	if err != nil {
		return a, err
	}
	// A concurrent refresh may return a newer snapshot without our callback.
	if err := checkIdentity(updated); err != nil {
		return a, err
	}
	return updated, nil
}

var errGatewayAccountErrorUnchanged = errors.New("account error no longer applicable or unchanged")

// A refresh releases its store lock before selection records its failure.
// Bind that late write to the selected ID and credential epoch; a rename must
// not retarget it, and replacement credentials must not inherit the old error.
func (p *Proxy) noteAccountError(selected Account, err error) {
	if p.Store.Path == "" || selected.ID == "" || selected.CredentialVersion <= 0 || err == nil {
		return
	}
	message := err.Error()
	_ = p.Store.UpdateAccount(selected.ID, func(account *Account) error {
		if account.ID != selected.ID || account.CredentialVersion != selected.CredentialVersion || account.LastError == message {
			// Returning an error aborts the mutation, avoiding a redundant
			// file fsync/rename (or metadata update in a database-backed store).
			return errGatewayAccountErrorUnchanged
		}
		account.LastError = message
		return nil
	})
}

// pickProxy rotates through an account's enabled bound proxies.
func (p *Proxy) pickProxy(account Account) *ProxyEntry {
	if p.Proxies == nil || len(account.Proxies) == 0 {
		return nil
	}
	resolved := make([]*ProxyEntry, 0, len(account.Proxies))
	for _, id := range account.Proxies {
		if entry, ok := p.Proxies.Get(id); ok {
			resolved = append(resolved, entry)
		}
	}
	if len(resolved) == 0 {
		return nil
	}
	p.roundMu.Lock()
	if p.roundRobin == nil {
		p.roundRobin = make(map[string]int)
	}
	index := p.roundRobin[account.Name] % len(resolved)
	p.roundRobin[account.Name] = index + 1
	p.roundMu.Unlock()
	return resolved[index]
}

// clientFor returns the HTTP client for a request, wired through the
// account's proxy when one is bound.
func (p *Proxy) clientFor(proxy *ProxyEntry) *http.Client {
	return p.gatewayClientFor(proxy, gatewayClientHTTP)
}

// ServeHTTP handles gateway paths directly; the admin console and static
// frontend are wired through webHandler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"ok"}`)
		return
	}
	p.observeDashboardRealtimeIngress(r)
	p.gateway(w, r)
}

// gateway validates the key, picks an account and forwards the request.
func (p *Proxy) gateway(w http.ResponseWriter, r *http.Request) {
	if p.RuntimeModes {
		settings, err := p.settingsValue()
		if err != nil {
			apiError(w, http.StatusServiceUnavailable, "gateway settings unavailable")
			return
		}
		if officialFailoverFrom(r.Context()) != nil {
			settings.InferenceMode = requestInferenceMode(r.Context())
		}
		r = r.WithContext(withInferenceMode(r.Context(), settings.InferenceMode))
		if strings.HasPrefix(r.URL.Path, "/v1/codex/") || settings.InferenceMode == inferenceAppServer {
			if p.Official == nil {
				apiError(w, http.StatusNotFound, "endpoint is no longer available")
				return
			}
			p.gatewayOfficial(w, r, settings.InferenceMode)
			return
		}
	}
	if p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI {
		r = r.WithContext(withGatewayUnboundedPayload(r.Context(), true))
		if r.URL.Path == appServerClientIdentityPath {
			p.gatewayAppServerClientIdentity(w, r)
			return
		}
		var delegatedErr error
		r, delegatedErr = p.appServerBindDelegatedRequest(r)
		if delegatedErr != nil {
			apiError(w, http.StatusConflict, delegatedErr.Error())
			return
		}
		if alias := appServerModelAlias(r.URL.Path); alias != "" {
			copy := r.Clone(r.Context())
			address := *r.URL
			address.Path = alias
			address.RawPath = ""
			copy.URL = &address
			r = copy
		}
	}
	if p.BPS != nil && requestInferenceMode(r.Context()) != inferenceCodexCLI {
		p.gatewayBPS(w, r)
		return
	}
	if p.RuntimeModes && p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI && appServerBackendPath(r.URL.Path) {
		p.gatewayAppServerBackend(w, r)
		return
	}
	if p.RuntimeModes && p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI && (r.URL.Path == "/v1/analytics/codex/turn-costs" || r.URL.Path == "/backend-api/wham/usage/thread-estimates/query") {
		p.gatewayAppServerTurnCosts(w, r)
		return
	}
	if p.RuntimeModes && p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI &&
		appServerRealtimePath(r.URL.Path) {
		p.gatewayAppServerRealtime(w, r)
		return
	}
	if p.RuntimeModes && p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI &&
		appServerHistoryPath(r.URL.Path) {
		p.gatewayAppServerHistory(w, r)
		return
	}
	if p.RuntimeModes && p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI &&
		appServerAuxiliaryPath(r.URL.Path) {
		p.gatewayAppServerAuxiliary(w, r)
		return
	}
	if p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI && officialFailoverFrom(r.Context()) == nil && r.Method == http.MethodPost && (r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/responses/compact" || r.URL.Path == "/responses" || r.URL.Path == "/responses/compact") {
		p.gatewayWithAccountFailover(w, r, p.gateway)
		return
	}
	authStarted := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, authErr := p.resolveGatewayKeyContext(r.Context(), token)
	authFinished := time.Now()
	clientRequestID := gatewayObservedClientRequestID(r)
	recordIngress := func(status int, reason string) {
		p.recordGatewayIngressEvent(status, reason, gatewayObservedRoute(r.URL.Path), clientRequestID, gatewayObservedDuration(authStarted, authFinished))
	}
	if authErr != nil {
		if r.Context().Err() != nil {
			return
		}
		apiError(w, http.StatusServiceUnavailable, "gateway key authorization unavailable")
		recordIngress(http.StatusServiceUnavailable, "key_store_unavailable")
		return
	}
	if keyID == "" {
		apiError(w, http.StatusUnauthorized, "invalid API key")
		recordIngress(http.StatusUnauthorized, "invalid_api_key")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "gateway settings unavailable")
		recordIngress(http.StatusServiceUnavailable, "settings_unavailable")
		return
	}
	switch path {
	case "/responses", "/responses/compact", "/guardian", "/guardian-classifier":
		if r.Method != http.MethodPost {
			// Codex 0.154.0 switches its session to HTTP on this handshake
			// status (core/src/client.rs), without replaying a model turn.
			if (path == "/responses" || path == "/guardian" || path == "/guardian-classifier") && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				if settings.WebsocketEnabled {
					p.gatewayWebSocket(w, r, keyID, keyName, settings)
					return
				}
				apiError(w, http.StatusUpgradeRequired, "use the HTTP Responses transport")
				recordIngress(http.StatusUpgradeRequired, "websocket_disabled")
				return
			}
			w.Header().Set("Allow", http.MethodPost)
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			recordIngress(http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
	case "/models":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			apiError(w, http.StatusMethodNotAllowed, "method not allowed")
			recordIngress(http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
	default:
		apiError(w, http.StatusNotFound, "not found")
		recordIngress(http.StatusNotFound, "unknown_endpoint")
		return
	}
	rec := RequestRecord{
		unboundedPayload: gatewayUnboundedPayload(r.Context()),
		Time:             time.Now().Unix(),
		KeyID:            keyID,
		KeyName:          keyName,
		Endpoint:         strings.TrimPrefix(path, "/"),
		IP:               clientIP(r),
	}
	started := time.Now()
	var connectionResult *appServerConnectionResult
	if p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI && (path == "/responses" || path == "/responses/compact") {
		connectionResult = newAppServerConnectionResult()
		r = r.WithContext(withAppServerConnectionResult(r.Context(), connectionResult))
		if failover := officialFailoverFrom(r.Context()); failover != nil {
			failover.connectionResult = connectionResult
		}
	}
	observeGatewayClientIdentity(&rec, clientRequestID, "request")
	observeGatewayAuthentication(&rec, authStarted, authFinished)
	observedCtx, attemptObservation := withGatewayAttemptObservation(r.Context())
	r = r.WithContext(observedCtx)
	attemptObservation.ObserveInbound(r.URL.Path, nil)
	requestType := "http_json"
	switch path {
	case "/models":
		requestType = "models"
	case "/responses/compact":
		requestType = "compact"
	}
	observeGatewayRequest(&rec, r.UserAgent(), requestType, nil)
	observation := gatewayObservation{record: &rec, started: started}
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	defer func() {
		applyGatewayAttemptSnapshot(&rec, attemptObservation)
		if failover := officialFailoverFrom(r.Context()); failover != nil {
			failover.record, failover.key = &rec, keyID
		} else {
			if connectionResult != nil {
				connectionResult.Confirm(rec.Status == "ok" && r.Context().Err() == nil)
			}
			p.finishRecord(&rec, started, keyID)
		}
	}()

	if (path == "/models" && !settings.ModelsEnabled) || (path == "/responses/compact" && !settings.CompactEnabled) {
		rec.Status, rec.Error = "error", "endpoint is disabled by gateway settings"
		observeGatewayError(&rec, "gateway", "settings")
		apiError(w, http.StatusForbidden, rec.Error)
		return
	}
	if path == "/models" {
		p.forwardModels(w, r, &rec, settings)
		return
	}

	body, err := readBody(w, r)
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "client", "request_body")
		if strings.Contains(err.Error(), "32 MiB") {
			apiError(w, 413, err.Error())
		} else if errors.Is(err, errUnsupportedContentEncoding) {
			apiError(w, http.StatusUnsupportedMediaType, err.Error())
		} else {
			apiError(w, 400, err.Error())
		}
		return
	}
	var bodyModel string
	if p.AppServerCLI != nil && requestInferenceMode(r.Context()) == inferenceCodexCLI && (path == "/responses" || path == "/responses/compact") {
		rec.turnCostObservation = newAppServerCostObservation(body, r.Header)
		r = r.WithContext(withAppServerCostObservation(r.Context(), rec.turnCostObservation))
	}
	if failover := officialFailoverFrom(r.Context()); failover != nil && !failover.retrying {
		failover.requestBody = append([]byte(nil), body...)
	}
	if parsed := map[string]json.RawMessage{}; json.Unmarshal(body, &parsed) == nil {
		_ = json.Unmarshal(parsed["model"], &bodyModel)
		if failover := officialFailoverFrom(r.Context()); failover != nil && !failover.retrying {
			var previous string
			_ = json.Unmarshal(parsed["previous_response_id"], &previous)
			// The lightweight path does not own the missing conversation. A
			// delta tied to another account cannot be replayed as full history.
			failover.replayBlocked = previous != "" || appServerDelegatedOwner(r.Context()) != ""
		}
	}
	rec.Model = bodyModel
	attemptObservation.ObserveInbound(r.URL.Path, body)
	observeGatewayRequest(&rec, r.UserAgent(), requestType, body)
	compact := path == "/responses/compact"
	cacheKeyMode := "unobserved"
	if p.PromptCacheObserver != nil {
		cacheKeyMode = gatewayPromptCacheObservationKeyMode(r, body, settings)
	}
	session := gatewaySessionKey(r, body)
	if path == "/responses" || compact {
		body, session, err = applyGatewayPolicy(r, body, keyID, settings)
		if err != nil {
			rec.Status, rec.Error = "error", err.Error()
			observeGatewayError(&rec, "gateway", "request_policy")
			status := http.StatusBadRequest
			if strings.Contains(rec.Error, "32 MiB") {
				status = http.StatusRequestEntityTooLarge
			}
			apiError(w, status, rec.Error)
			return
		}
	}
	if failover := officialFailoverFrom(r.Context()); failover != nil {
		if failover.session != "" {
			session = failover.session
		} else {
			failover.session = session
		}
	}
	if err := p.bindInferenceSession(r.Context(), keyID, session); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		apiError(w, http.StatusConflict, rec.Error)
		return
	}
	prepared, stream, err := prepareHTTPBody(body, compact, r.Header, gatewayUnboundedPayload(r.Context()))
	compactV2 := compact && gatewayUsesResponsesCompaction(p.gatewayInferenceBaseURL(r.Context()))
	if err == nil && compactV2 {
		prepared, err = prepareResponsesCompaction(prepared, gatewayUnboundedPayload(r.Context()))
	}
	if err == nil && (path == "/responses" || compact) {
		err = validateGatewayAccountPromptCacheSize(prepared, keyID, settings, gatewayUnboundedPayload(r.Context()))
	}
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "client", "request_body")
		status := http.StatusBadRequest
		if errors.Is(err, errGatewayPreparedBodyLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		apiError(w, status, err.Error())
		return
	}
	rec.Stream = stream
	clientPromptCacheKey := gatewayPromptCacheKeyValue(prepared)
	if stream {
		rec.RequestType = "http_sse"
	}
	if path == "/responses" && ((stream && !settings.HTTPStreamEnabled) || (!stream && !settings.HTTPJSONEnabled)) {
		rec.Status, rec.Error = "error", "HTTP response mode is disabled by gateway settings"
		observeGatewayError(&rec, "gateway", "settings")
		apiError(w, http.StatusForbidden, rec.Error)
		return
	}

	routeCtx := withGatewayCooldownModel(withGatewayRequestRoute(r.Context(), keyID, session), bodyModel)
	defer stopGatewayRequestRoute(routeCtx)
	defer observeGatewayAdmissionLoss(&rec, routeCtx)
	routeCtx = withGatewayCacheAffinity(routeCtx, keyID, session, prepared, settings)
	routeCtx = p.withGatewayNativeDispatchAuthorization(routeCtx, token, keyID)
	selectionStarted := time.Now()
	var account Account
	var release func()
	failover := officialFailoverFrom(r.Context())
	if failover != nil && failover.retrying {
		account, release, err = p.pickFailoverAccount(routeCtx, token, keyID)
	} else {
		account, release, err = p.pickSessionAccountAwait(routeCtx, token, keyID, session)
	}
	attemptObservation.ObserveStage("account_selection", selectionStarted, time.Now())
	if err != nil {
		attemptObservation.ObserveStage("request_preparation", started, selectionStarted)
		officialObserveAttemptFailure(r.Context(), err)
		var unavailable *gatewayBoundAccountUnavailableError
		if failover != nil && errors.As(err, &unavailable) {
			failover.account, failover.owner = unavailable.Account, unavailable.Owner
		}
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(&rec, "gateway", "account_selection")
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	if failover != nil {
		failover.account = account
		rec.AccountID, rec.AccountCredentialVersion = account.ID, account.CredentialVersion
		if !failover.retrying {
			failover.owner = gatewayAccountRef(account)
		}
	}
	rec.Account = account.Name
	if rec.turnCostObservation != nil {
		rec.turnCostObservation.Owner = gatewayAccountRef(account)
	}
	rec.Groups = append([]string(nil), account.Groups...)
	// The cache namespace depends on the admitted owner; compress only after
	// that scalar rewrite so JSON, SSE and native/zstd send the same key.
	// Sub2API's raw OAuth passthrough scopes compact bodies too. Keep that
	// behavior here: the Codex backend translates compact into streaming
	// Responses, while standalone compact retains its raw request contract.
	if path == "/responses" || compact {
		prepared, err = prepareGatewayAccountPromptCache(prepared, keyID, account, settings, gatewayUnboundedPayload(r.Context()))
	}
	var wireBody []byte
	var nativeCompression bool
	if err == nil {
		wireBody, nativeCompression, err = prepareGatewayRequestBody(p, r, path, prepared)
	}
	// Preparation now spans account admission because its cache key depends on
	// the admitted owner. Include the final rewrite/compression in this interval.
	attemptObservation.ObserveStage("request_preparation", started, time.Now())
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "client", "request_body")
		status := http.StatusInternalServerError
		if errors.Is(err, errGatewayPreparedBodyLimit) || errors.Is(err, errGatewayCompressedBodyLimit) {
			status = http.StatusRequestEntityTooLarge
		}
		apiError(w, status, rec.Error)
		return
	}
	leaseCtx, stopLease := p.officialFailoverLease(routeCtx, keyID, session, account)
	defer stopLease()
	proxyStarted := time.Now()
	proxy, err := p.gatewayRequestProxy(routeCtx, account)
	attemptObservation.ObserveStage("proxy_selection", proxyStarted, time.Now())
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "gateway", "proxy_route")
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}

	upstreamPath := path
	if compactV2 {
		upstreamPath = "/responses"
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(r.Context()), upstreamPath, nil)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(&rec, "gateway", "request_build")
		apiError(w, http.StatusInternalServerError, rec.Error)
		return
	}
	if nativeCompression {
		leaseCtx = withGatewayNativeRequestCompression(leaseCtx)
	}
	up, err := http.NewRequestWithContext(leaseCtx, http.MethodPost, target, bytes.NewReader(wireBody))
	if err != nil {
		rec.Status = "error"
		rec.Error = "cannot create upstream request"
		observeGatewayError(&rec, "gateway", "request_build")
		apiError(w, 500, rec.Error)
		return
	}
	if err := copyCodexRequestHeaders(up.Header, r.Header, settings); err != nil {
		rec.Status, rec.Error = "error", "invalid Codex fingerprint settings"
		observeGatewayError(&rec, "gateway", "request_headers")
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	if requestInferenceMode(r.Context()) == inferenceCodexCLI {
		p.AppServerCLI.applyIdentity(up.Header)
	}
	up.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if account.AccountID != "" {
		up.Header.Set("ChatGPT-Account-Id", account.AccountID)
	}
	applyGatewayCacheHeaders(up.Header, p.gatewayInferenceBaseURL(r.Context()), path, keyID, account, clientPromptCacheKey, settings, false)
	applyGatewayRoutingHint(up.Header, p.gatewayInferenceBaseURL(r.Context()), path, account, prepared)
	up.Header.Set("Content-Type", "application/json")
	if gatewayWantsRequestCompression(r, path) && !nativeCompression {
		up.Header.Set("Content-Encoding", "zstd")
	}
	up.Header.Set("Accept", "application/json")
	if !compact || compactV2 {
		up.Header.Set("Accept", "text/event-stream")
	}
	attemptObservation.ObserveSent(up.URL.Path, prepared)
	if path == "/responses" || compact {
		rec.PromptCache = p.PromptCacheObserver.Observe(r, prepared, keyID, account, cacheKeyMode)
	}
	authorizeRetry := func(recovered Account) error {
		return p.authorizeGatewayRetry(up.Context(), token, keyID, recovered)
	}
	var res *http.Response
	if compact {
		client := p.gatewayRequestClientWithHeaderTimeout(routeCtx, proxy, gatewayClientCompact, time.Duration(settings.UpstreamCompactTimeoutSeconds)*time.Second)
		res, err = p.doObservedAccountRequest(&rec, client, up, account, func(c *http.Client, req *http.Request) (*http.Response, error) {
			if p.AppServerCLI != nil && requestInferenceMode(req.Context()) == inferenceCodexCLI {
				return p.AppServerCLI.RoundTrip(req, c, proxy, prepared, body)
			}
			return doCompactRequestWithTimeout(c, req, time.Duration(settings.UpstreamCompactTimeoutSeconds)*time.Second)
		}, authorizeRetry)
	} else {
		client := p.gatewayRequestClientWithHeaderTimeout(routeCtx, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
		res, err = p.doObservedAccountRequest(&rec, client, up, account, func(c *http.Client, req *http.Request) (*http.Response, error) {
			if p.AppServerCLI != nil && path == "/responses" && requestInferenceMode(req.Context()) == inferenceCodexCLI {
				return p.AppServerCLI.RoundTrip(req, c, proxy, prepared, body)
			}
			if settings.PreferWebsocket && (path == "/responses" || path == "/guardian" || path == "/guardian-classifier") && !p.wsDialSkipped(account.ID, proxy, gatewayObservedRoute(path)) {
				response, wsErr := p.preferredResponsesWebSocket(req, proxy, prepared, settings)
				if !errors.Is(wsErr, errPreferredWebSocketUnavailable) {
					return response, wsErr
				}
				first, _ := req.Context().Value(gatewayTransportAttemptKey{}).(*gatewayAttemptHandle)
				finishGatewayObservedAttempt(first, response, wsErr)
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				fallback := gatewayAttemptObservationFrom(req.Context()).Begin("request", req.URL.EscapedPath(), time.Now())
				response, wsErr = doStreamingRequestWithIdleTimeout(c, req, time.Duration(settings.UpstreamStreamIdleTimeoutSeconds)*time.Second)
				finishGatewayObservedAttempt(fallback, response, wsErr)
				return response, wsErr
			}
			return doStreamingRequestWithIdleTimeout(c, req, time.Duration(settings.UpstreamStreamIdleTimeoutSeconds)*time.Second)
		}, authorizeRetry)
	}
	if err != nil {
		rec.Status = "error"
		rec.Error = "upstream connection failed"
		officialObserveAttemptFailure(r.Context(), err)
		var ownerChanged *gatewayOwnerChangedError
		if errors.As(err, &ownerChanged) {
			rec.Error = ownerChanged.Error()
		}
		var authorization *gatewayRecoveryAuthorizationError
		if errors.As(err, &authorization) {
			rec.Error = authorization.Error()
		}
		observeGatewayTransportError(&rec, err, "response_headers")
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	defer res.Body.Close()
	responseStarted := time.Now()
	defer func() { observeGatewayResponseTransfer(&rec, responseStarted, time.Now()) }()
	observeGatewayHeaders(&rec, res.Header)
	var cooldownIdentity gatewayHTTPCooldownIdentity
	observeEvent := func(data []byte) {
		view := gatewayWSObservationView(data) // Observation only; forward original SSE.
		// The response observer validates response identity before recording its model.
		observation.event(view)
		if rec.turnCostObservation != nil {
			var event struct {
				Response json.RawMessage `json:"response"`
			}
			_ = json.Unmarshal(view, &event)
			if object := gatewayResponseObject(event.Response); object != nil && gatewayResponseMatchesRecord(&rec, object) {
				rec.turnCostObservation.ObserveResponse(view)
			}
		}
		if !cooldownIdentity.known && rec.responseIdentityKnown {
			cooldownIdentity.digest, cooldownIdentity.known = rec.responseIdentityDigest, true
		}
		p.observeGatewayCooldownEvent(res, data, &cooldownIdentity)
		observeAppServerCLIFailoverEvent(r.Context(), data)
	}
	if res.StatusCode >= 300 {
		observeGatewayError(&rec, "upstream", "http_status")
		copyResponseHeaders(w.Header(), res.Header)
		w.WriteHeader(res.StatusCode)
		prefix, copyErr := copyGatewayErrorBody(w, res.Body, attemptObservation)
		p.observeGatewayPaymentBody(res, prefix)
		observeAppServerCLIFailoverHTTP(r.Context(), res.StatusCode, prefix)
		rec.Status = "error"
		rec.HTTPCode = res.StatusCode
		rec.Error = errorFromPayload(prefix)
		if rec.Error == "" {
			rec.Error = fmt.Sprintf("upstream returned HTTP %d", res.StatusCode)
		}
		if copyErr != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if compact {
		// JSON compact clients receive a replacement context even when the
		// subscription upstream produced it through streaming Responses v2.
		var result []byte
		var readErr error
		if compactV2 {
			var collected json.RawMessage
			collected, readErr = collectResponseObserved(res.Body, observeEvent, &rec)
			if readErr == nil {
				recordGatewayHTTPResponseOutcome(&rec, collected)
				result, readErr = responsesCompactionResult(prepared, collected, gatewayUnboundedPayload(r.Context()))
			}
		} else {
			var source io.Reader = res.Body
			if !gatewayUnboundedPayload(r.Context()) {
				source = io.LimitReader(source, maxBody+1)
			}
			result, readErr = io.ReadAll(source)
		}
		if readErr != nil || len(result) > maxBody && !gatewayUnboundedPayload(r.Context()) || !json.Valid(result) {
			if recordGatewayHTTPReadCancellation(&rec, r.Context(), readErr) {
				panic(http.ErrAbortHandler)
			}
			var failure *upstreamResponseError
			if errors.As(readErr, &failure) {
				recordGatewayHTTPResponseOutcome(&rec, failure.Response)
				rec.Status, rec.Error = "error", errorFromPayload(failure.Payload)
				observeGatewayError(&rec, "upstream", "response_event")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": failure.Payload})
				return
			}
			rec.Status, rec.Error = "error", "invalid upstream compaction response"
			observeGatewayError(&rec, "upstream", "response_body")
			apiError(w, http.StatusBadGateway, rec.Error)
			return
		}
		recordGatewayHTTPResponseOutcome(&rec, result)
		if !commitAppServerCLIFailover(w, r.Context(), &rec) {
			return
		}
		copyResponseHeaders(w.Header(), res.Header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		written, writeErr := w.Write(result)
		if writeErr != nil || written != len(result) {
			rec.Status, rec.Error = "aborted", "client response delivery failed"
			observeGatewayError(&rec, "client", "response_write")
		}
		return
	}
	if !stream {
		result, err := collectResponseObserved(res.Body, observeEvent, &rec)
		if err != nil {
			if recordGatewayHTTPReadCancellation(&rec, r.Context(), err) {
				panic(http.ErrAbortHandler)
			}
			var upstream *upstreamResponseError
			if errors.As(err, &upstream) {
				recordGatewayHTTPResponseOutcome(&rec, upstream.Response)
				observeGatewayError(&rec, "upstream", "response_event")
				rec.Status = "error"
				rec.Error = errorFromPayload(upstream.Payload)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": upstream.Payload})
				return
			}
			rec.Status = "error"
			rec.Error = err.Error()
			observeGatewayError(&rec, "upstream", "response_body")
			apiError(w, 502, err.Error())
			return
		}
		recordGatewayHTTPResponseOutcome(&rec, result)
		if !commitAppServerCLIFailover(w, r.Context(), &rec) {
			return
		}
		copyResponseHeaders(w.Header(), res.Header)
		w.Header().Set("Content-Type", "application/json")
		written, writeErr := w.Write(result)
		if writeErr != nil || written != len(result) {
			rec.Status, rec.Error = "aborted", "client response delivery failed"
			observeGatewayError(&rec, "client", "response_write")
		}
		return
	}
	// Streaming: pass bytes through unchanged while the scanner harvests the
	// terminal event for token and error metrics.
	copyResponseHeaders(w.Header(), res.Header)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(res.StatusCode)
	controller := http.NewResponseController(w)
	_ = controller.Flush()
	scanner := &sseScanner{rec: &rec, observe: observeEvent}
	if err := writeStreamPipe(w, controller, res, scanner, true); err != nil {
		if recordGatewayHTTPReadCancellation(&rec, r.Context(), err) {
			panic(http.ErrAbortHandler)
		}
		if rec.Status != "error" {
			rec.Status = "aborted"
		}
		if strings.Contains(err.Error(), "upstream stream read failed") {
			observeGatewayError(&rec, "upstream", "response_body")
			// The upstream promised more bytes; aborting the client
			// connection surfaces the truncation instead of a clean EOF.
			if rec.Error == "" {
				rec.Error = "upstream stream truncated"
			}
			panic(http.ErrAbortHandler)
		}
		if rec.Error == "" {
			rec.Error = "client connection lost"
		}
		observeGatewayError(&rec, "client", "response_write")
		return
	}
	scanner.Finish()
	if rec.Status == "error" || rec.Status == "aborted" {
		observeGatewayError(&rec, "upstream", "response_body")
	}
	if scanner.finished {
		// scanner already set status/error/tokens
	} else if rec.Status == "" {
		rec.Status = "aborted"
		rec.Error = "upstream stream ended without a terminal response"
		observeGatewayError(&rec, "upstream", "response_body")
	}
}

// forwardModels serves the model catalog through the selected account.
func (p *Proxy) forwardModels(w http.ResponseWriter, r *http.Request, rec *RequestRecord, settings Settings) {
	token := bearerToken(r)
	observer := gatewayAttemptObservationFrom(r.Context())
	selectionStarted := time.Now()
	session := ""
	if settings.StickySessionsEnabled {
		candidate := gatewaySessionKey(r, nil)
		if candidate != "" {
			bound, err := p.sessionAccount(r.Context(), rec.KeyID, candidate, "")
			if err != nil {
				observer.ObserveStage("account_selection", selectionStarted, time.Now())
				rec.Status, rec.Error = "error", err.Error()
				observeGatewayError(rec, "gateway", "account_selection")
				writeGatewaySelectionError(w, err)
				return
			}
			if bound != "" {
				session = candidate
			}
		}
	}
	// Catalog discovery may honor an existing explicit inference session, but
	// an unbound GET must not create a conversation or choose its future owner.
	routeCtx := context.WithValue(withGatewayRequestRoute(r.Context(), rec.KeyID, session), gatewayCooldownContextKey{}, "models")
	defer stopGatewayRequestRoute(routeCtx)
	defer observeGatewayAdmissionLoss(rec, routeCtx)
	routeCtx = p.withGatewayNativeDispatchAuthorization(routeCtx, token, rec.KeyID)
	account, release, err := p.pickSessionAccount(routeCtx, token, rec.KeyID, session)
	observer.ObserveStage("account_selection", selectionStarted, time.Now())
	if err != nil {
		rec.Status = "error"
		rec.Error = err.Error()
		observeGatewayError(rec, "gateway", "account_selection")
		writeGatewaySelectionError(w, err)
		return
	}
	defer release()
	rec.Account = account.Name
	rec.Groups = append([]string(nil), account.Groups...)
	proxyStarted := time.Now()
	proxy, err := p.gatewayRequestProxy(routeCtx, account)
	observer.ObserveStage("proxy_selection", proxyStarted, time.Now())
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(rec, "gateway", "proxy_route")
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	if proxy != nil {
		rec.ProxyName = proxy.Name
	}
	query := url.Values{}
	if settings.CodexFingerprintEnabled {
		profile, err := effectiveCodexFingerprintProfile(settings.CodexFingerprintProfile)
		if err != nil {
			rec.Status, rec.Error = "error", "invalid Codex fingerprint settings"
			observeGatewayError(rec, "gateway", "request_headers")
			apiError(w, http.StatusServiceUnavailable, rec.Error)
			return
		}
		query.Set("client_version", profile.Version)
	} else if v := r.URL.Query().Get("client_version"); v != "" {
		query.Set("client_version", v)
	} else {
		query.Set("client_version", clientVersion)
	}
	target, err := gatewayUpstreamURL(p.gatewayInferenceBaseURL(r.Context()), "/models", query)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		observeGatewayError(rec, "gateway", "request_build")
		apiError(w, http.StatusInternalServerError, rec.Error)
		return
	}
	up, err := http.NewRequestWithContext(routeCtx, http.MethodGet, target, nil)
	if err != nil {
		rec.Status = "error"
		rec.Error = "cannot create upstream request"
		observeGatewayError(rec, "gateway", "request_build")
		apiError(w, 500, rec.Error)
		return
	}
	if err := copyCodexRequestHeaders(up.Header, r.Header, settings); err != nil {
		rec.Status, rec.Error = "error", "invalid Codex fingerprint settings"
		observeGatewayError(rec, "gateway", "request_headers")
		apiError(w, http.StatusServiceUnavailable, rec.Error)
		return
	}
	if requestInferenceMode(r.Context()) == inferenceCodexCLI {
		p.AppServerCLI.applyIdentity(up.Header)
	}
	up.Header.Set("Authorization", "Bearer "+account.AccessToken)
	if account.AccountID != "" {
		up.Header.Set("ChatGPT-Account-Id", account.AccountID)
	}
	up.Header.Set("Accept", "application/json")
	observer.ObserveSent(up.URL.Path, nil)
	client := p.gatewayRequestClientWithHeaderTimeout(routeCtx, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
	res, err := p.doObservedAccountRequest(rec, client, up, account, doHTTPAccountRequest, func(recovered Account) error {
		return p.authorizeGatewayRetry(up.Context(), token, rec.KeyID, recovered)
	})
	if err != nil {
		rec.Status = "error"
		rec.Error = "upstream connection failed"
		var ownerChanged *gatewayOwnerChangedError
		if errors.As(err, &ownerChanged) {
			rec.Error = ownerChanged.Error()
		}
		var authorization *gatewayRecoveryAuthorizationError
		if errors.As(err, &authorization) {
			rec.Error = authorization.Error()
		}
		observeGatewayTransportError(rec, err, "response_headers")
		apiError(w, gatewayTransportFailureStatus(err), rec.Error)
		return
	}
	defer res.Body.Close()
	responseStarted := time.Now()
	defer func() { observeGatewayResponseTransfer(rec, responseStarted, time.Now()) }()
	observeGatewayHeaders(rec, res.Header)
	if res.StatusCode == http.StatusNotModified {
		rec.Status = "ok"
		copyResponseHeaders(w.Header(), res.Header)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if res.StatusCode >= 300 {
		observeGatewayError(rec, "upstream", "http_status")
		copyResponseHeaders(w.Header(), res.Header)
		w.WriteHeader(res.StatusCode)
		prefix, copyErr := copyGatewayErrorBody(w, res.Body, observer)
		rec.Status = "error"
		rec.HTTPCode = res.StatusCode
		rec.Error = errorFromPayload(prefix)
		if rec.Error == "" {
			rec.Error = fmt.Sprintf("upstream returned HTTP %d", res.StatusCode)
		}
		if copyErr != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	// Validate the bounded catalog before committing upstream success. A
	// truncated or oversized body must not become a successful partial JSON.
	const modelsLimit = 8 << 20
	var source io.Reader = res.Body
	if !gatewayUnboundedPayload(r.Context()) {
		source = io.LimitReader(source, modelsLimit+1)
	}
	body, readErr := io.ReadAll(source)
	if readErr != nil || len(body) > modelsLimit && !gatewayUnboundedPayload(r.Context()) {
		rec.Status, rec.Error = "error", "invalid upstream models response"
		observeGatewayError(rec, "upstream", "response_body")
		apiError(w, http.StatusBadGateway, rec.Error)
		return
	}
	rec.Status = "ok"
	copyResponseHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(body)
}

// finishRecord persists the request metrics and key usage timestamps.
func (p *Proxy) finishRecord(rec *RequestRecord, started time.Time, keyID string) {
	if rec.turnCostObservation != nil && rec.Status == "ok" && p.AppServerCLI != nil && p.AppServerCLI.turnCosts != nil {
		if mapping, ok := rec.turnCostObservation.Mapping(); ok {
			p.AppServerCLI.recordTurnCost(keyID, rec.turnCostObservation.ClientTurn, mapping)
		}
	}
	rec.turnCostObservation = nil
	if rec.ObservationSchemaVersion > 0 && rec.Error != "" {
		_, truncated := redactMetricErrorObserved(rec.Error)
		if rec.ErrorTruncated == nil || truncated {
			rec.ErrorTruncated = &truncated
		}
	}
	rec.LatencyMs = max(0, time.Since(started).Milliseconds())
	measured := true
	rec.LatencyKnown = &measured
	if rec.Status == "" {
		rec.Status = "ok"
	}
	if p.Metrics != nil {
		p.Metrics.Record(*rec)
	}
	if keyID != "" && keyID != "admin" && p.Keys != nil {
		p.Keys.TouchLastUsed(keyID, rec.Time)
	}
}
