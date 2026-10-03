package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"codex-agent/internal/publicerrors"

	"github.com/klauspost/compress/zstd"
)

const (
	codexURL         = "https://chatgpt.com/backend-api/codex"
	oauthURL         = "https://auth.openai.com/oauth/token"
	oauthClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	clientVersion    = "0.155.0"
	clientOriginator = "codex_cli_rs"
	maxBody          = 32 << 20
)

func newHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Retain a completed HTTP/1 burst for the next turn instead of Go's two
	// idle connections per host. The global idle limit and timeout still bound
	// retention; active account concurrency is enforced separately by admission.
	transport.MaxIdleConnsPerHost = transport.MaxIdleConns
	transport.ResponseHeaderTimeout = 90 * time.Second
	// Codex's reqwest build does not enable response gzip negotiation. Avoid
	// adding a Go-only Accept-Encoding header to otherwise native CLI requests.
	transport.DisableCompression = true
	return &http.Client{Transport: transport, Timeout: 10 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": publicerrors.Message(hidePaths(message)), "type": "proxy_error"}})
}

// hidePaths replaces common absolute filesystem paths in error messages.
func hidePaths(message string) string {
	const placeholder = "[path]"
	var result strings.Builder
	result.Grow(len(message))
	for i := 0; i < len(message); {
		// Windows drive-letter paths and UNC/device paths. Quoted paths
		// can contain spaces, so consume through the matching quote.
		drivePath := i+2 < len(message) && (message[i] >= 'a' && message[i] <= 'z' || message[i] >= 'A' && message[i] <= 'Z') && message[i+1] == ':' && (message[i+2] == '\\' || message[i+2] == '/')
		uncPath := i+2 < len(message) && message[i] == '\\' && message[i+1] == '\\'
		if drivePath || uncPath {
			j := filesystemPathEnd(message, i)
			result.WriteString(placeholder)
			i = j
			continue
		}
		// Unix absolute path: /foo/bar (needs a nested segment, so
		// plain "/api" style text stays untouched).
		if message[i] == '/' && i+1 < len(message) && !pathDelimiter(message[i+1]) {
			j := filesystemPathEnd(message, i)
			if strings.Contains(message[i+1:j], "/") {
				result.WriteString(placeholder)
				i = j
				continue
			}
		}
		result.WriteByte(message[i])
		i++
	}
	return result.String()
}

func filesystemPathEnd(message string, start int) int {
	var quote byte
	if start > 0 && (message[start-1] == '\'' || message[start-1] == '"') {
		quote = message[start-1]
	}
	end := start
	for end < len(message) {
		char := message[end]
		if quote != 0 {
			if char == quote || char == '\n' || char == '\r' {
				break
			}
		} else if pathDelimiter(char) {
			break
		}
		end++
	}
	return end
}

func pathDelimiter(c byte) bool {
	return c == ' ' || c == ',' || c == ';' || c == '"' || c == '\'' || c == ')' || c == ']' || c == '\n' || c == '\r' || c == '\t'
}

func prepareBody(raw []byte, compact bool) ([]byte, bool, error) {
	return prepareHTTPBody(raw, compact, nil)
}

func prepareHTTPBody(raw []byte, compact bool, headers http.Header, unbounded ...bool) ([]byte, bool, error) {
	if len(raw) > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, false, errGatewayPreparedBodyLimit
	}
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil || b == nil {
		return nil, false, errors.New("body must be a JSON object")
	}
	var model string
	if json.Unmarshal(b["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, false, errors.New("model is required")
	}
	stream := false
	if value, ok := b["stream"]; ok {
		if string(value) == "null" || json.Unmarshal(value, &stream) != nil {
			return nil, false, errors.New("stream must be a boolean")
		}
	}
	if compact {
		stream = false
		delete(b, "stream")
		delete(b, "store")
	} else {
		b["stream"], b["store"] = json.RawMessage("true"), json.RawMessage("false")
		if _, ok := b["instructions"]; !ok {
			// Codex Responses Lite carries instructions in input and omits
			// this field. Match the scalar header that survives forwarding,
			// including canonical-key precedence and Connection filtering.
			lite := make(http.Header)
			copyGatewayHeaders(lite, headers, func(name string) bool {
				return name == "x-openai-internal-codex-responses-lite"
			}, func(string) bool { return true })
			if lite.Get("X-Openai-Internal-Codex-Responses-Lite") != "true" {
				b["instructions"] = json.RawMessage(`""`)
			}
		}
		var input string
		if json.Unmarshal(b["input"], &input) == nil && string(b["input"]) != "null" {
			b["input"], _ = marshalGatewayRequestJSON([]any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": input}}}})
		}
	}
	encoded, err := marshalGatewayRequestJSON(b)
	if len(encoded) > maxBody && !gatewayPayloadUnboundedOption(unbounded) {
		return nil, false, errGatewayPreparedBodyLimit
	}
	return encoded, stream, err
}

func copyRequestHeaders(dst, src http.Header) {
	if dst == nil {
		return
	}
	copyGatewayHeaders(dst, src, func(lower string) bool {
		switch lower {
		case "user-agent", "originator", "version", "openai-beta",
			"session-id", "thread-id", "session_id", "conversation_id",
			"x-request-id", "x-client-request-id", "x-oai-attestation",
			"x-openai-subagent", "x-openai-memgen-request",
			"x-openai-internal-codex-responses-lite", "x-openai-internal-codex-residency",
			"x-responsesapi-include-timing-metrics", "traceparent", "tracestate", "if-none-match":
			return true
		}
		return strings.HasPrefix(lower, "x-codex-")
	}, func(lower string) bool {
		// These fields are lists. Identity, routing, tracing and opaque state
		// fields are scalar and must not become multiple wire identities.
		if strings.HasPrefix(lower, "x-codex-") {
			return isGatewayScalarCodexHeader(lower)
		}
		return lower != "openai-beta" && lower != "tracestate" &&
			lower != "if-none-match"
	})
	userAgent, originator := dst.Get("User-Agent"), dst.Get("Originator")
	// The CLI supports an originator override and builds its user agent from
	// that value. Such native identities need not contain the word "codex".
	nativeIdentity := isGatewayNativeUserAgent(userAgent, originator)
	if !nativeIdentity {
		fallbackUA, fallbackOriginator := gatewayDefaultApplicationIdentity()
		dst.Set("User-Agent", fallbackUA)
		dst.Set("originator", fallbackOriginator)
		dst.Set("version", clientVersion)
	}
	if dst.Get("originator") == "" {
		dst.Set("originator", clientOriginator)
	}
	if !nativeIdentity || originator == "" {
		// Match the explicit fingerprint override policy: an opaque caller
		// attestation must not survive a gateway-generated identity change.
		dst.Del("X-Oai-Attestation")
	}
	// Native custom-provider exec clients can omit the version header, while
	// the built-in OpenAI provider supplies it. Preserve both absence and an
	// explicit value rather than mixing the two provider identities.
	// Drop only the obsolete beta, preserving any other negotiated features.
	betas := dst.Values("OpenAI-Beta")
	dst.Del("OpenAI-Beta")
	for _, value := range betas {
		for _, beta := range strings.Split(value, ",") {
			if beta = strings.TrimSpace(beta); beta != "" && !strings.EqualFold(beta, "responses=experimental") {
				dst.Add("OpenAI-Beta", beta)
			}
		}
	}
}

// This is the gateway's compatibility heuristic, not an authentication check
// or an official Codex identity detector. A thread-scoped Originator can differ
// from the process originator embedded in the CLI's complete user agent.
func isGatewayNativeUserAgent(userAgent, originator string) bool {
	if strings.HasPrefix(strings.ToLower(userAgent), "codex") ||
		(originator != "" && strings.HasPrefix(userAgent, originator+"/")) {
		return true
	}
	separator := strings.Index(userAgent, "/")
	osStart := strings.Index(userAgent, " (")
	if separator <= 0 || osStart <= separator+1 {
		return false
	}
	version := userAgent[separator+1 : osStart]
	if index := strings.IndexAny(version, "-+"); index >= 0 {
		version = version[:index]
	}
	components := strings.Split(version, ".")
	if len(components) != 3 {
		return false
	}
	for _, component := range components {
		if component == "" {
			return false
		}
		for _, digit := range component {
			if digit < '0' || digit > '9' {
				return false
			}
		}
	}
	platform := userAgent[osStart+2:]
	osEnd := strings.Index(platform, ") ")
	return osEnd > 0 && strings.Contains(platform[:osEnd], "; ") && strings.TrimSpace(platform[osEnd+2:]) != ""
}

func copyResponseHeaders(dst, src http.Header) {
	copyGatewayHeaders(dst, src, func(lower string) bool {
		switch lower {
		case "content-type", "retry-after", "x-request-id", "request-id", "x-oai-request-id",
			"etag", "x-models-etag", "openai-model", "x-openai-model", "x-reasoning-included",
			"x-openai-authorization-error", "x-error-json":
			return true
		}
		return strings.HasPrefix(lower, "x-codex-") || isCodexRateLimitHeader(lower)
	}, func(lower string) bool {
		return !strings.HasPrefix(lower, "x-codex-") || isGatewayScalarCodexHeader(lower)
	})
}

func isGatewayScalarCodexHeader(name string) bool {
	switch name {
	case "x-codex-installation-id", "x-codex-parent-thread-id", "x-codex-routing-hint",
		"x-codex-turn-metadata", "x-codex-turn-state", "x-codex-window-id",
		"x-codex-ws-stream-request-start-ms", "x-codex-imagegen-request-id",
		"x-codex-safety-buffering-enabled", "x-codex-safety-buffering-faster-model",
		"x-codex-credits-balance", "x-codex-credits-has-credits", "x-codex-credits-unlimited",
		"x-codex-active-limit", "x-codex-rate-limit-reached-type", "x-codex-promo-message":
		return true
	}
	// Unknown future extension fields remain multi-valued until their protocol
	// defines scalar semantics; do not silently discard negotiated capabilities.
	return isCodexRateLimitHeader(name)
}

// copyGatewayHeaders makes case folding deterministic even for headers built
// directly as Go maps. Network-parsed headers already use canonical keys. A
// canonical spelling wins scalar conflicts; list fields retain every value.
// Connection-nominated fields belong only to that hop and never cross it.
func copyGatewayHeaders(dst, src http.Header, allowed, singleton func(string) bool) {
	if dst == nil {
		return
	}
	connectionHeaders := make(map[string]bool)
	keys := make([]string, 0, len(src))
	for name, values := range src {
		if allowed(strings.ToLower(name)) {
			keys = append(keys, name)
		}
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, nominated := range strings.Split(value, ",") {
					connectionHeaders[strings.ToLower(strings.TrimSpace(nominated))] = true
				}
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left != right {
			return left < right
		}
		canonical := http.CanonicalHeaderKey(left)
		if keys[i] == canonical || keys[j] == canonical {
			return keys[i] == canonical
		}
		return keys[i] < keys[j]
	})
	copied := make(http.Header)
	for _, name := range keys {
		lower := strings.ToLower(name)
		if !allowed(lower) || connectionHeaders[lower] {
			continue
		}
		canonical := http.CanonicalHeaderKey(name)
		values := src[name]
		if singleton(lower) {
			if len(copied[canonical]) == 0 && len(values) > 0 {
				copied[canonical] = []string{values[0]}
			}
		} else {
			copied[canonical] = append(copied[canonical], values...)
		}
	}
	for name := range dst {
		if _, ok := copied[http.CanonicalHeaderKey(name)]; ok {
			delete(dst, name)
		}
	}
	for name, values := range copied {
		dst[name] = values
	}
}

// Codex discovers additional metered limits by their header suffix, not just
// the legacy x-codex-* family (codex-api/src/rate_limits.rs).
func isCodexRateLimitHeader(name string) bool {
	name = strings.ToLower(name)
	if !strings.HasPrefix(name, "x-") {
		return false
	}
	for _, suffix := range []string{
		"-primary-used-percent", "-primary-window-minutes", "-primary-reset-at",
		"-secondary-used-percent", "-secondary-window-minutes", "-secondary-reset-at", "-limit-name",
	} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

type upstreamResponseError struct {
	Payload  json.RawMessage
	Response json.RawMessage
}

func (e *upstreamResponseError) Error() string { return "upstream response failed" }

// Assemble completed output items when Codex omits them from its terminal
// metadata. Streaming clients continue to receive the original event bytes.
func collectResponse(r io.Reader) ([]byte, error) {
	return collectResponseObserved(r, nil)
}

func collectResponseObserved(r io.Reader, observe func([]byte), records ...*RequestRecord) ([]byte, error) {
	unbounded := len(records) > 0 && records[0] != nil && records[0].unboundedPayload
	scanner := newGatewaySSELines(r, unbounded)
	var data []byte
	output := responseOutputAccumulator{unbounded: unbounded}
	var started bool
	var pendingEvent, pendingResponse []byte
	var pendingError error
	pendingPriority := 0
	consume := func() ([]byte, error) {
		if len(data) == 0 {
			return nil, nil
		}
		payload := data[:len(data)-1]
		priority := gatewayHTTPDeferredResponsePriority(payload)
		if priority == 1 {
			// response.done is unknown to the CLI. Keep an EOF-only JSON
			// compatibility candidate without letting it establish stream identity.
			if pendingPriority <= 1 {
				pendingEvent, pendingResponse, pendingError, pendingPriority = payload, nil, nil, 1
			}
			return nil, nil
		}
		response, terminal, err := output.consume(payload)
		if terminal && priority > 0 {
			// Retain only the latest event error. No accounting/cooldown side
			// effect is committed until EOF makes that error the final result.
			if priority >= pendingPriority {
				pendingEvent, pendingResponse, pendingError, pendingPriority = payload, response, err, priority
			}
			if len(records) > 0 && !gatewayHTTPIsCompleted(payload) {
				observeGatewayHTTPPendingIdentity(records[0], payload)
			}
			return nil, nil
		}
		if observe != nil {
			observe(payload)
		}
		if err != nil {
			return nil, err
		}
		if terminal {
			if len(records) > 0 && gatewayHTTPIsCompleted(payload) {
				records[0].httpCompleted = true
			}
			return response, nil
		}
		return nil, nil
	}
	for scanner.Scan() {
		line := stripInitialSSEBOM(scanner.Bytes(), &started)
		if !validSSELine(line) {
			return nil, errors.New("upstream SSE contains invalid UTF-8")
		}
		if len(line) == 0 {
			if result, err := consume(); result != nil || err != nil {
				return result, err
			}
			data = nil
			continue
		}
		var value []byte
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			value = bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		case bytes.Equal(line, []byte("data")):
		default:
			continue
		}
		if !unbounded && len(value)+1 > maxBody-len(data) {
			return nil, errors.New("upstream SSE event exceeds 32 MiB")
		}
		data = append(data, value...)
		data = append(data, '\n')
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, errors.New("upstream SSE event exceeds 32 MiB")
		}
		return nil, fmt.Errorf("upstream stream read failed: %w", err)
	}
	if result, err := consume(); result != nil || err != nil {
		return result, err
	}
	if pendingEvent != nil {
		if pendingPriority == 1 {
			pendingResponse, _, pendingError = output.consume(pendingEvent)
		}
		if observe != nil && !(pendingError != nil && gatewayHTTPIsCompleted(pendingEvent)) {
			observe(pendingEvent)
		}
		if pendingError != nil {
			return nil, pendingError
		}
		return pendingResponse, nil
	}
	return nil, errors.New("upstream stream ended without a response")
}

// bearerToken extracts the bearer credential from an Authorization header.
func bearerToken(r *http.Request) string {
	token, _, valid := adminBearer(r)
	if !valid {
		return ""
	}
	return token
}

// clientIP derives the remote address without the port.
func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(ingressClientIPKey{}).(string); ok {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	// RemoteAddr usually includes a port, but internal callers can supply a
	// bare IPv6 literal. Splitting at the last colon corrupts that address.
	return strings.Trim(r.RemoteAddr, "[]")
}

func refreshAccount(ctx context.Context, client *http.Client, endpoint string, a Account) (Account, error) {
	return refreshAccountWithSettings(ctx, client, endpoint, a, defaultSettings())
}

func refreshAccountWithSettings(ctx context.Context, client *http.Client, endpoint string, a Account, settings Settings) (updated Account, resultErr error) {
	if a.RefreshToken == "" {
		return a, errors.New("account has no refresh token")
	}
	if err := accountRefreshPreflight(ctx); err != nil {
		return a, err
	}
	// A rotating refresh token must be saved even if the requesting client leaves.
	ctx, cancel := context.WithTimeout(withGatewayNativeAccountScope(withoutGatewayNativeRequestCompression(context.WithoutCancel(ctx)), a), 45*time.Second)
	defer cancel()
	clientID := a.ClientID
	if clientID == "" {
		clientID = oauthClientID
	}
	// Match login/src/auth/manager.rs RefreshRequest: JSON, without a scope
	// override, and with the client_id that originally issued these credentials.
	encoded, _ := json.Marshal(map[string]string{
		"grant_type": "refresh_token", "refresh_token": a.RefreshToken, "client_id": clientID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		// URL parse errors can contain userinfo or query credentials.
		return a, &gatewayOAuthRefreshError{Kind: "invalid"}
	}
	if req.URL.Hostname() == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.User != nil || req.URL.Fragment != "" || req.URL.Opaque != "" {
		return a, &gatewayOAuthRefreshError{Kind: "invalid"}
	}
	if client == nil {
		return a, &gatewayOAuthRefreshError{Kind: "connection"}
	}
	req.Header.Set("Content-Type", "application/json")
	if err := applyGatewayOAuthHeaders(req.Header, settings, false); err != nil {
		return a, err
	}
	attempt := gatewayAttemptObservationFrom(ctx).Begin("refresh", req.URL.Path, time.Now())
	res, err := client.Do(req)
	res = gatewayHTTPResponseView(res)
	if res != nil {
		attempt.Headers(res.StatusCode, time.Now())
	}
	defer func() { finishGatewayObservedAttempt(attempt, res, resultErr) }()
	if err != nil {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errGatewayProxyUnavailable} {
			if errors.Is(err, cause) {
				return a, fmt.Errorf("OAuth refresh connection failed: %w", cause)
			}
		}
		return a, &gatewayOAuthRefreshError{Kind: "connection"}
	}
	defer res.Body.Close()
	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		retryAfter, present := parseGatewayRetryAfter(res.Header.Get("Retry-After"), time.Now(), gatewayRefreshFailureMax)
		// Error-code inspection is bounded and never retains provider text. Only
		// a token grant response may mark this credential snapshot terminal.
		const errorLimit = 64 << 10
		var raw []byte
		if res.StatusCode != http.StatusUnauthorized {
			var readErr error
			raw, readErr = io.ReadAll(io.LimitReader(res.Body, errorLimit+1))
			if readErr != nil || len(raw) > errorLimit {
				raw = nil
			}
		}
		return a, &gatewayOAuthRefreshError{Kind: "http", StatusCode: res.StatusCode,
			Permanent: gatewayPermanentOAuthGrantFailure(res.StatusCode, raw), RetryAfter: retryAfter, RetryAfterPresent: present}
	}
	var result struct {
		AccessToken  string          `json:"access_token"`
		RefreshToken string          `json:"refresh_token"`
		IDToken      json.RawMessage `json:"id_token"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	// Read one byte past the limit so a complete JSON prefix cannot hide an
	// oversized body. Unmarshal also rejects a trailing second JSON value.
	const responseLimit = 2 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
	if err != nil {
		for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, cause) {
				return a, fmt.Errorf("OAuth refresh connection failed: %w", cause)
			}
		}
		return a, &gatewayOAuthRefreshError{Kind: "invalid"}
	}
	if len(body) > responseLimit || json.Unmarshal(body, &result) != nil {
		return a, &gatewayOAuthRefreshError{Kind: "invalid"}
	}
	hasIdentity := gatewayRefreshIdentityPresent(result.IDToken)
	if result.AccessToken == "" && result.RefreshToken == "" {
		var idToken string
		if !hasIdentity || json.Unmarshal(result.IDToken, &idToken) != nil || managedIdentityJWT(idToken) == nil {
			return a, &gatewayOAuthRefreshError{Kind: "invalid"}
		}
	}
	if result.RefreshToken != "" {
		a.RefreshToken = result.RefreshToken
	}
	// All three tokens are optional in Codex's RefreshResponse. Preserve a
	// rotated refresh token and independently apply a returned identity token.
	if result.AccessToken == "" {
		if hasIdentity {
			a = rebuildGatewayRefreshIdentity(a, result.IDToken)
		}
		return a, nil
	}
	changedAccess := result.AccessToken != a.AccessToken
	a.AccessToken = result.AccessToken
	if changedAccess || hasIdentity {
		a = rebuildGatewayRefreshIdentity(a, result.IDToken)
	}
	// expires_in is an optional extension absent from Codex's RefreshResponse.
	// Invalid metadata must not discard already rotated credentials.
	var expiresIn int64
	now := time.Now().Unix()
	if json.Unmarshal(result.ExpiresIn, &expiresIn) == nil && expiresIn > 0 && expiresIn <= (1<<63-1)-now {
		a.ExpiresAt = now + expiresIn
	} else {
		// Codex's RefreshResponse does not require expires_in. A successful
		// exchange may already have rotated the refresh token, so persist it
		// even when the access token does not expose a usable JWT expiry.
		a.ExpiresAt, _ = jwtAccountClaims(a.AccessToken)
	}
	return a, nil
}

var errUnsupportedContentEncoding = errors.New("unsupported Content-Encoding; expected identity or zstd")

type gatewayLimitedRequestReader struct {
	io.Reader
	exceeded bool
}

func (r *gatewayLimitedRequestReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		r.exceeded = true
	}
	return n, err
}

// readBody accepts the native CLI's zstd HTTP requests. Limit both wire bytes
// and decoded JSON so compression cannot bypass the request size limit.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if gatewayUnboundedPayload(r.Context()) {
		return readOfficialBody(r)
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	limited := &gatewayLimitedRequestReader{Reader: gatewayRequestContextReader{ctx: r.Context(), reader: http.MaxBytesReader(w, r.Body, maxBody)}}
	var reader io.Reader = limited
	encoding := gatewayRequestContentEncoding(r.Header)
	switch encoding {
	case "", "identity":
	case "zstd":
		decoder, err := zstd.NewReader(limited, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(2*maxBody), zstd.WithDecoderMaxWindow(maxBody))
		if err != nil {
			return nil, errors.New("invalid zstd request body")
		}
		defer decoder.Close()
		reader = decoder
	default:
		return nil, errUnsupportedContentEncoding
	}
	body, err := io.ReadAll(io.LimitReader(gatewayRequestContextReader{ctx: r.Context(), reader: reader}, maxBody+1))
	if canceled := r.Context().Err(); canceled != nil {
		return nil, canceled
	}
	if len(body) > maxBody || limited.exceeded {
		return nil, errors.New("request exceeds 32 MiB")
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, errors.New("request exceeds 32 MiB")
		}
		if encoding == "zstd" {
			return nil, errors.New("invalid zstd request body")
		}
		return nil, errors.New("cannot read request")
	}
	return body, nil
}

// writeStreamPipe forwards upstream bytes to the client and, when a
// scanner is provided, feeds them into the SSE metrics watcher.
func writeStreamPipe(w http.ResponseWriter, controller *http.ResponseController, res *http.Response, scanner io.Writer, flush bool) error {
	// The write budget belongs only to downstream delivery. An expired HTTP/2
	// write deadline can reset the stream while we wait for the next upstream
	// event; HTTP/1 can also fail its final chunk after a delayed clean EOF.
	defer controller.SetWriteDeadline(time.Time{})
	buf := make([]byte, 32<<10)
	emptyReads := 0
	for {
		_ = controller.SetWriteDeadline(time.Time{})
		n, readErr := res.Body.Read(buf)
		if n > 0 {
			emptyReads = 0
			_ = controller.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if scanner != nil {
				_, _ = scanner.Write(buf[:n])
			}
			written, err := w.Write(buf[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
			if flush {
				if err := controller.Flush(); err != nil {
					return err
				}
			}
			// The CLI stops reading at the terminal event, not at HTTP EOF.
			// Stop only after delivering that event so a held-open backend
			// does not retain admission or turn a completed call into aborted.
			if watcher, ok := scanner.(*sseScanner); ok && watcher.terminal {
				return nil
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("upstream stream read failed: %w", readErr)
			}
			return nil
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return fmt.Errorf("upstream stream read failed: %w", io.ErrNoProgress)
			}
		}
	}
}

// copyErrorBody forwards an upstream error body while capturing a bounded
// prefix for metric and diagnostic use.
func copyErrorBody(w io.Writer, body io.Reader) (prefix []byte) {
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if len(prefix) < 64<<10 {
				remaining := 64<<10 - len(prefix)
				prefix = append(prefix, buf[:min(n, remaining)]...)
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return prefix
			}
		}
		if err != nil {
			return prefix
		}
	}
}
