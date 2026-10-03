package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	gatewayCooldownDefault = 4 * time.Second
	// A malicious or stale upstream header cannot disable an account forever.
	// Valid delays up to one hour are honored without shortening them.
	gatewayCooldownMax        = time.Hour
	maxGatewayCooldowns       = 16384
	gatewayCooldownConnection = "connection"
)

type gatewayCooldownContextKey struct{}
type gatewayCooldownAccountContextKey struct{}

func withGatewayCooldownModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, gatewayCooldownContextKey{}, "model:"+strings.TrimSpace(model))
}

func gatewayCooldownScope(ctx context.Context) string {
	if scope, ok := ctx.Value(gatewayCooldownContextKey{}).(string); ok {
		return scope
	}
	return gatewayCooldownConnection
}

// Include the actual sent access token as well as the persisted credential
// generation: refresh and re-import release stale failures, while a management
// rename/edit does not. Only this digest enters memory or Redis.
func gatewayCooldownKey(account Account, scope string) string {
	raw, _ := json.Marshal([]any{gatewayAdmissionKey(account), account.CredentialVersion, account.AccessToken, scope})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type gatewayCooldownError struct {
	until   time.Time
	payment bool
}

func (e *gatewayCooldownError) Error() string {
	if e.payment {
		return gatewayPaymentMessage
	}
	return "upstream account is rate limited; retry after the cooldown"
}

type gatewayAccountBusyError struct{ session bool }

func (e *gatewayAccountBusyError) Error() string {
	if e.session {
		return "session account is busy; retry later"
	}
	return "all accounts in the key's groups are busy"
}

type gatewayCooldownStoreError struct{}

func (*gatewayCooldownStoreError) Error() string { return "shared account cooldown state unavailable" }

func gatewaySelectionFailure(err error) (status int, retryAfter string, recognized bool) {
	if errors.Is(err, errAccountDraining) {
		return http.StatusServiceUnavailable, "", true
	}
	if gatewaySharedAdmissionFailure(err) {
		return http.StatusServiceUnavailable, "", true
	}
	var ownerChanged *gatewayOwnerChangedError
	if errors.As(err, &ownerChanged) {
		return http.StatusConflict, "", true
	}
	var cooldown *gatewayCooldownError
	if errors.As(err, &cooldown) {
		remaining := max(time.Duration(0), time.Until(cooldown.until))
		seconds := int64(remaining / time.Second)
		if remaining%time.Second != 0 {
			seconds++
		}
		seconds = max(int64(1), seconds)
		if cooldown.payment {
			return http.StatusServiceUnavailable, strconv.FormatInt(seconds, 10), true
		}
		return http.StatusTooManyRequests, strconv.FormatInt(seconds, 10), true
	}
	var busy *gatewayAccountBusyError
	if errors.As(err, &busy) {
		return http.StatusTooManyRequests, "1", true
	}
	var unavailable *gatewayCooldownStoreError
	if errors.As(err, &unavailable) {
		return http.StatusServiceUnavailable, "", true
	}
	return http.StatusServiceUnavailable, "", false
}

func writeGatewaySelectionError(w http.ResponseWriter, err error) {
	status, retryAfter, _ := gatewaySelectionFailure(err)
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	apiError(w, status, err.Error())
}

// One bounded sorted set per namespace shares deadlines using Redis server
// time. Concurrent 429s only extend a deadline. A capacity marker conservatively
// pauses selection rather than evicting an active long Retry-After early.
const gatewayCooldownScript = `
local clock = redis.call('TIME')
local now = clock[1] * 1000 + math.floor(clock[2] / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if ARGV[1] == 'record' then
  local member = ARGV[5]
  local existing = redis.call('ZSCORE', KEYS[1], member)
  if not existing and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then
    member = '!capacity'
    existing = redis.call('ZSCORE', KEYS[1], member)
  end
  local deadline = now + tonumber(ARGV[3])
  if existing then deadline = math.max(deadline, tonumber(existing)) end
  redis.call('ZADD', KEYS[1], deadline, member)
  redis.call('PEXPIRE', KEYS[1], ARGV[4])
end
local deadline = tonumber(redis.call('ZSCORE', KEYS[1], '!capacity')) or now
for i = 5, #ARGV do
  local value = tonumber(redis.call('ZSCORE', KEYS[1], ARGV[i])) or now
  deadline = math.max(deadline, value)
end
return math.max(0, deadline - now)
`

func (p *Proxy) localGatewayCooldown(keys []string, recordUntil time.Time) time.Time {
	p.cooldownMu.Lock()
	defer p.cooldownMu.Unlock()
	now := time.Now()
	if !recordUntil.IsZero() {
		if p.cooldowns == nil {
			p.cooldowns = make(map[string]time.Time)
		}
		if len(p.cooldowns) >= maxGatewayCooldowns {
			for key, until := range p.cooldowns {
				if !now.Before(until) {
					delete(p.cooldowns, key)
				}
			}
		}
		key := keys[0]
		if current, exists := p.cooldowns[key]; exists || len(p.cooldowns) < maxGatewayCooldowns {
			if recordUntil.After(current) {
				p.cooldowns[key] = recordUntil
			}
		} else if recordUntil.After(p.cooldownOverflowUntil) {
			p.cooldownOverflowUntil = recordUntil
		}
	}
	until := p.cooldownOverflowUntil
	for _, key := range keys {
		value := p.cooldowns[key]
		if !now.Before(value) {
			delete(p.cooldowns, key)
		} else if value.After(until) {
			until = value
		}
	}
	return until
}

func (p *Proxy) gatewayCooldown(ctx context.Context, account Account, scope string, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay = min(delay, gatewayCooldownMax)
	// The legacy JSON store does not persist CredentialVersion (json:"-").
	// Use its actual token consistently, including immediately after refresh.
	if p.Store.DB == nil {
		account.CredentialVersion = 0
	}
	keys := []string{gatewayCooldownKey(account, scope)}
	if scope != gatewayCooldownConnection {
		keys = append(keys, gatewayCooldownKey(account, gatewayCooldownConnection))
	}
	var recordUntil time.Time
	if delay > 0 {
		recordUntil = time.Now().Add(delay)
	}
	until := p.localGatewayCooldown(keys, recordUntil)
	if db := p.Store.DB; db != nil && db.Redis != nil {
		operation := "read"
		if delay > 0 {
			operation = "record"
		}
		args := []any{operation, maxGatewayCooldowns, delay.Milliseconds(), (gatewayCooldownMax + time.Second).Milliseconds()}
		for _, key := range keys {
			args = append(args, key)
		}
		sharedCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		remaining, err := db.Redis.Eval(sharedCtx, gatewayCooldownScript, []string{db.Namespace + "gateway:cooldowns"}, args...).Int64()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &gatewayCooldownStoreError{}
		}
		if sharedUntil := time.Now().Add(time.Duration(remaining) * time.Millisecond); remaining > 0 && sharedUntil.After(until) {
			until = sharedUntil
		}
	}
	if time.Now().Before(until) {
		return &gatewayCooldownError{until: until}
	}
	return nil
}

func (p *Proxy) checkGatewayCooldown(ctx context.Context, account Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.checkGatewayPayment(account); err != nil {
		return err
	}
	settings, err := p.settingsValue()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return &gatewayCooldownStoreError{}
	}
	if !settings.RateLimitCooldownEnabled {
		// Retain shared deadlines so re-enabling observes their original TTL;
		// disabled policy must neither block nor touch Redis/cached deadlines.
		return nil
	}
	return p.gatewayCooldown(ctx, account, gatewayCooldownScope(ctx), 0)
}

func (p *Proxy) recordGatewayCooldown(ctx context.Context, account Account, retryAfter string) {
	settings, err := p.settingsValue()
	if err != nil || !settings.RateLimitCooldownEnabled {
		return
	}
	delay, valid := parseGatewayRetryAfter(retryAfter, time.Now(), gatewayCooldownMax)
	if !valid {
		delay = gatewayCooldownDefault
	}
	if delay > 0 {
		// The original upstream 429 remains authoritative even if recording in
		// Redis fails. Future shared-state reads fail closed during the outage.
		// A received upstream 429 remains valid after the downstream caller
		// disconnects. Shared writes still have the bounded timeout above.
		_ = p.gatewayCooldown(context.WithoutCancel(ctx), account, gatewayCooldownScope(ctx), delay)
	}
}

func (p *Proxy) observeGatewayCooldownResponse(ctx context.Context, account Account, response *http.Response) {
	if response == nil {
		return
	}
	// SSE failures arrive after HTTP headers. Keep the actual authenticated
	// credential generation available to that observer, including a 401 retry.
	request := response.Request
	if request == nil {
		request = &http.Request{}
	}
	response.Request = request.WithContext(context.WithValue(ctx, gatewayCooldownAccountContextKey{}, account))
	if response.StatusCode == http.StatusTooManyRequests {
		p.recordGatewayCooldown(ctx, account, response.Header.Get("Retry-After"))
	}
	if response.StatusCode == http.StatusPaymentRequired {
		p.recordGatewayPayment(ctx, account)
	}
}

// HTTP has one accepted logical response. Keep its exact response identity
// separate from bounded/redacted metric strings: long IDs or common prefixes
// must not make a foreign terminal eligible to change account scheduling.
type gatewayHTTPCooldownIdentity struct {
	digest [32]byte
	known  bool
}

func (identity *gatewayHTTPCooldownIdentity) accepts(payload []byte) bool {
	fields, err := gatewayWSParseFields(payload)
	return err == nil && identity.acceptsFields(fields)
}

func (identity *gatewayHTTPCooldownIdentity) acceptsFields(fields map[string]json.RawMessage) bool {
	var responseID, nestedID string
	var response json.RawMessage
	if gatewayWSDecodeFields(fields, map[string]any{"response_id": &responseID, "response": &response}) != nil {
		return false
	}
	if len(response) > 0 && string(response) != "null" && gatewayWSDecodeExact(response, map[string]any{"id": &nestedID}) != nil {
		return false
	}
	id := nestedID
	if id != "" && responseID != "" && id != responseID {
		return false
	}
	if id == "" {
		id = responseID
	}
	if id == "" {
		// Legacy errors can be request-scoped without a response ID. Preserve
		// their structured quota signals; only explicit conflicts are excluded.
		return true
	}
	digest := sha256.Sum256([]byte(id))
	if identity.known && identity.digest != digest {
		return false
	}
	identity.digest, identity.known = digest, true
	return true
}

func (p *Proxy) observeGatewayCooldownEvent(response *http.Response, payload []byte, identity *gatewayHTTPCooldownIdentity) {
	if response == nil || response.Request == nil {
		return
	}
	fields, err := gatewayWSParseFields(payload)
	if err != nil || identity != nil && !identity.acceptsFields(fields) {
		return
	}
	ctx := response.Request.Context()
	account, ok := ctx.Value(gatewayCooldownAccountContextKey{}).(Account)
	if !ok {
		return
	}
	if gatewayPaymentFailureFields(fields, true) {
		p.recordGatewayPayment(ctx, account)
	}
	_, _, retryAfter, limited := gatewayEventRateLimitFields(payload, fields)
	if limited {
		p.recordGatewayCooldown(ctx, account, retryAfter)
	}
}

// Structured status/code fields identify rate limiting; messages are never
// inspected, persisted or treated as retry instructions.
func gatewayEventRateLimit(payload []byte) (streamID, responseID, retryAfter string, limited bool) {
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return
	}
	return gatewayEventRateLimitFields(payload, fields)
}

func gatewayEventRateLimitFields(payload []byte, fields map[string]json.RawMessage) (streamID, responseID, retryAfter string, limited bool) {
	var event struct {
		Type       string                     `json:"-"`
		Code       string                     `json:"code"`
		Status     int                        `json:"status"`
		StatusCode int                        `json:"status_code"`
		StreamID   string                     `json:"-"`
		ResponseID string                     `json:"-"`
		Headers    map[string]json.RawMessage `json:"headers"`
		Error      struct {
			Code   string `json:"code"`
			Type   string `json:"type"`
			Status int    `json:"status"`
		} `json:"error"`
		Response struct {
			ID    string `json:"-"`
			Error struct {
				Code   string `json:"code"`
				Type   string `json:"type"`
				Status int    `json:"status"`
			} `json:"error"`
		} `json:"-"`
	}
	// Keep legacy error/status/header parsing, but protocol ownership and event
	// classification use exact JSON members just like the official serde types.
	var response json.RawMessage
	if gatewayWSDecodeFields(fields, map[string]any{"type": &event.Type, "stream_id": &event.StreamID, "response_id": &event.ResponseID, "response": &response}) != nil || (event.Type != "error" && event.Type != "response.failed") {
		return
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if len(response) > 0 && string(response) != "null" {
		var responseError json.RawMessage
		if gatewayWSDecodeExact(response, map[string]any{"id": &event.Response.ID, "error": &responseError}) != nil {
			return
		}
		if len(responseError) > 0 && json.Unmarshal(responseError, &event.Response.Error) != nil {
			return
		}
	}
	if event.Response.ID != "" && event.ResponseID != "" && event.Response.ID != event.ResponseID {
		return
	}
	rateLimit := func(code, kind string, status int) bool {
		return status == 429 || code == "rate_limit_exceeded" || code == "usage_limit_reached" || code == "rateLimitExceeded" || code == "usageLimitExceeded" || code == "basispoints_route_rate_limit_exceeded" || kind == "rate_limit_error"
	}
	if event.Status == 0 {
		// WrappedWebsocketErrorEvent in the CLI accepts status_code too.
		event.Status = event.StatusCode
	}
	limited = rateLimit(event.Code, "", event.Status) || rateLimit(event.Error.Code, event.Error.Type, event.Error.Status) || rateLimit(event.Response.Error.Code, event.Response.Error.Type, event.Response.Error.Status)
	for key, value := range event.Headers {
		if strings.EqualFold(key, "Retry-After") {
			// The CLI accepts scalar JSON header values, including numeric
			// quota windows. An unrelated numeric header must not hide a 429.
			if json.Unmarshal(value, &retryAfter) != nil {
				var number json.Number
				if json.Unmarshal(value, &number) == nil {
					retryAfter = number.String()
				}
			}
		}
	}
	responseID = event.Response.ID
	if responseID == "" {
		responseID = event.ResponseID
	}
	return event.StreamID, responseID, retryAfter, limited
}

func (t *gatewayWSTurns) observeCooldown(ctx context.Context, payload []byte, connected Account) {
	fields, err := gatewayWSParseFields(payload)
	if err != nil {
		return
	}
	t.observeCooldownFields(ctx, payload, connected, fields)
}

func (t *gatewayWSTurns) observeCooldownFields(ctx context.Context, payload []byte, connected Account, fields map[string]json.RawMessage) {
	streamID, responseID, retryAfter, limited := gatewayEventRateLimitFields(payload, fields)
	if !limited {
		return
	}
	t.mu.Lock()
	// The upstream pump observes cooldown before consuming the terminal event.
	// The first failure still applies; a previously completed ID cannot affect
	// an as-yet unidentified next turn or create a connection-wide cooldown.
	if t.completed.contains(responseID) {
		t.mu.Unlock()
		return
	}
	explicitLane := streamID != ""
	streamID, _, unique := t.responseLaneLocked(streamID, responseID, explicitLane)
	if !unique {
		t.mu.Unlock()
		return
	}
	scope := ""
	if streamID != "" {
		// Named events belong only to that lane's current head. Unknown
		// lanes and conflicting IDs cannot rate-limit unrelated models.
		queue := t.pending[streamID]
		if len(queue) > 0 && (responseID == "" || queue[0].responseID == "" || queue[0].responseID == responseID) {
			scope = "model:" + strings.TrimSpace(queue[0].rec.Model)
		}
	} else if responseID != "" {
		// An identified event without a lane can still name a known active
		// response. Never turn a stale or unknown ID into a global limit.
		for _, pending := range t.pending {
			if len(pending) > 0 && pending[0].responseID == responseID {
				scope = "model:" + strings.TrimSpace(pending[0].rec.Model)
				break
			}
		}
		// The default lane may fail before response.created supplies its ID.
		if queue := t.pending[""]; scope == "" && len(queue) > 0 && queue[0].responseID == "" {
			scope = "model:" + strings.TrimSpace(queue[0].rec.Model)
		}
	} else {
		// Only an event with neither response nor lane identity can signal
		// a genuine connection-wide limit.
		scope = gatewayCooldownConnection
	}
	t.mu.Unlock()
	if scope != "" {
		t.proxy.recordGatewayCooldown(context.WithValue(ctx, gatewayCooldownContextKey{}, scope), connected, retryAfter)
	}
}
