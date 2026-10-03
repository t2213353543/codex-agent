package server

// This adapter shares the official request writer without hosting a Codex
// agent. Conversation history, environment and tool execution stay with the
// caller. It is opt-in until the capture/compatibility matrix is complete.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type appServerCLI struct {
	owner          *Proxy
	transport      *appServerTransport
	installation   string
	identity       *gatewayRuntimeIdentity
	turnCosts      *appServerTurnCostLedger
	turnCostWrites sync.WaitGroup
	mu             sync.Mutex
	closed         bool
	catalog        map[[32]byte]appServerCatalogSnapshot
	loads          singleflight.Group
}
type appServerCatalogSnapshot struct {
	models  map[string]json.RawMessage
	expires time.Time
}

// capabilityManifest is intentionally explicit: a client must not infer that
// a transparent route means the full hosted App Server lifecycle exists.
// The manifest is metadata only; it never grants a tool, account or URL.
func (a *appServerCLI) capabilityManifest() map[string]any {
	return map[string]any{
		"client_owned_tools":        true,
		"client_owned_history":      true,
		"client_owned_compaction":   true,
		"native_http_required":      true,
		"native_websocket_required": true,
		"hosted_runtime":            false,
		"responses":                 true,
		"history_notes":             true,
		"standalone_search":         true,
		"images":                    true,
		"realtime":                  true,
		"guardian":                  "transparent_client_request",
		"apps_directory":            "app_server_executor_only",
		"conditional_unverified":    []string{"oauth_device_auth", "agent_identity_lifecycle", "remote_control", "telemetry", "dynamic_mcp_destinations", "dynamic_plugin_urls", "provider_semantic_equivalence"},
	}
}

func newAppServerCLI(p *Proxy) *appServerCLI {
	return &appServerCLI{owner: p, transport: newAppServerTransport(p), installation: bpsUUID(), catalog: map[[32]byte]appServerCatalogSnapshot{}, turnCosts: newAppServerTurnCostLedger(p.Store.DB)}
}
func (a *appServerCLI) Close() {
	if a.transport != nil {
		_ = a.transport.Close()
	}
	a.mu.Lock()
	a.closed = true
	clear(a.catalog)
	a.mu.Unlock()
	a.turnCostWrites.Wait()
}

// Credential and route scoped: a catalog from a different account or outbound
// proxy must never decide this request's model capabilities. Only metadata is
// cached; neither authorization tokens nor conversation bodies are retained.
func (a *appServerCLI) model(req *http.Request, client *http.Client, proxy *ProxyEntry, name string) (json.RawMessage, *http.Response, error) {
	if err := checkGatewayNativeDispatch(req.Context()); err != nil {
		return nil, nil, err
	}
	scope := req.URL.Scheme + "://" + req.URL.Host + "\x00" + req.Header.Get("Authorization") + "\x00" + req.Header.Get("ChatGPT-Account-Id")
	if proxy != nil {
		scope += "\x00" + proxy.ID + "\x00" + hex.EncodeToString(gatewayProxyFingerprintBytes(*proxy))
	}
	key := sha256.Sum256([]byte(scope))
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, nil, errAppServerTransportClosed
	}
	for k, entry := range a.catalog {
		if !time.Now().Before(entry.expires) {
			delete(a.catalog, k)
		}
	}
	cached, ok := a.catalog[key]
	a.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		if m := cached.models[name]; m != nil {
			return append(json.RawMessage(nil), m...), nil, nil
		}
		return nil, nil, errOfficialModelUnavailable
	}
	// Singleflight shares immutable catalog/error bytes, never a response body
	// reader. Each waiter observes its own cancellation and authorization.
	load := func() (any, error) {
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return nil, errAppServerTransportClosed
		}
		current, exists := a.catalog[key]
		a.mu.Unlock()
		if exists && time.Now().Before(current.expires) {
			return current, nil
		}
		u := *req.URL
		u.Path = strings.TrimSuffix(u.Path, "/responses") + "/models"
		u.RawPath = ""
		u.RawQuery = url.Values{"client_version": {clientVersion}}.Encode()
		up, err := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(req.Context()), http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		up.Header = req.Header.Clone()
		up.Header.Del("Content-Encoding")
		up.Header.Del("Content-Length")
		up.Header.Del("Content-Type")
		up.Header.Set("Accept", "application/json")
		res, err := client.Do(up)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				return nil, err
			}
			return nil, &appServerCatalogHTTPError{status: res.StatusCode, headers: res.Header.Clone(), body: raw}
		}
		var body struct {
			Models []json.RawMessage `json:"models"`
		}
		// Decode directly from the response stream; retaining a second full raw
		// catalog buffer doubles memory without improving the metadata cache.
		decoder := json.NewDecoder(res.Body)
		if err := decoder.Decode(&body); err != nil {
			if requestErr := req.Context().Err(); requestErr != nil {
				return nil, requestErr
			}
			return nil, errors.New("invalid upstream App Server model catalog")
		}
		if body.Models == nil {
			return nil, errors.New("invalid upstream App Server model catalog")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if requestErr := req.Context().Err(); requestErr != nil {
				return nil, requestErr
			}
			return nil, errors.New("invalid trailing upstream App Server model catalog")
		}
		snapshot := appServerCatalogSnapshot{models: map[string]json.RawMessage{}, expires: time.Now().Add(5 * time.Minute)}
		for _, m := range body.Models {
			var entry struct {
				Slug string `json:"slug"`
			}
			if json.Unmarshal(m, &entry) != nil || entry.Slug == "" {
				continue
			}
			snapshot.models[entry.Slug] = m
		}
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			return nil, errAppServerTransportClosed
		}
		for k, v := range a.catalog {
			if time.Now().After(v.expires) {
				delete(a.catalog, k)
			}
		}
		a.catalog[key] = snapshot
		a.mu.Unlock()
		return snapshot, nil
	}
	var result any
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		wait := a.loads.DoChan(hex.EncodeToString(key[:]), load)
		select {
		case <-req.Context().Done():
			return nil, nil, req.Context().Err()
		case response := <-wait:
			result, err = response.Val, response.Err
		}
		if checkErr := checkGatewayNativeDispatch(req.Context()); checkErr != nil {
			return nil, nil, checkErr
		}
		// A canceled leader may abort a shared idempotent GET. An uncanceled
		// waiter may load once under its own request context after that ends.
		if !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			break
		}
	}
	if err != nil {
		var upstream *appServerCatalogHTTPError
		if errors.As(err, &upstream) {
			return nil, &http.Response{StatusCode: upstream.status, Header: upstream.headers.Clone(), Body: io.NopCloser(bytes.NewReader(upstream.body)), Request: req}, nil
		}
		return nil, nil, err
	}
	m := result.(appServerCatalogSnapshot).models[name]
	if m == nil {
		return nil, nil, errOfficialModelUnavailable
	}
	return append(json.RawMessage(nil), m...), nil, nil
}
func gatewayProxyFingerprintBytes(p ProxyEntry) []byte {
	sum := gatewayProxyFingerprint(p)
	return sum[:]
}

type appServerCatalogHTTPError struct {
	status  int
	headers http.Header
	body    []byte
}

func (e *appServerCatalogHTTPError) Error() string { return "upstream model catalog request failed" }

// prepareAppServerCLI keeps caller controls visible. Already-built Lite and
// previous_response_id requests are handled by the existing transparent path;
// this adapter must not prepend another harness or reinterpret delta history.
func prepareAppServerCLI(raw []byte, headers http.Header, model json.RawMessage, installation string) (appServerRequestInput, appServerToolAdapter, error) {
	var in appServerRequestInput
	var adapter appServerToolAdapter
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return in, adapter, errors.New("invalid App Server CLI request")
	}
	meta, err := parseAppServerRequestModel(model)
	if err != nil {
		return in, adapter, err
	}
	in.ModelMetadataRaw = model
	in.SessionID = headers.Get("Session-Id")
	if in.SessionID == "" {
		in.SessionID = headers.Get("Session_id")
	}
	in.ThreadID = headers.Get("Thread-Id")
	if in.SessionID == "" {
		_ = json.Unmarshal(body["prompt_cache_key"], &in.SessionID)
	}
	if in.SessionID == "" {
		in.SessionID = newID("")
	}
	if in.ThreadID == "" {
		in.ThreadID = in.SessionID
	}
	in.ExtraHeaders = headers.Clone()
	in.Originator = headers.Get("Originator")
	in.BetaFeatures = headers.Get("X-Codex-Beta-Features")
	in.TurnState = headers.Get("X-Codex-Turn-State")
	in.ClientMetadata = map[string]string{}
	if raw := body["client_metadata"]; len(raw) > 0 && json.Unmarshal(raw, &in.ClientMetadata) != nil {
		return in, adapter, errors.New("invalid client_metadata")
	}
	if in.ClientMetadata == nil {
		in.ClientMetadata = map[string]string{}
	}
	for _, k := range []string{"x-codex-installation-id", "x-codex-window-id", "x-codex-turn-metadata", "x-codex-parent-thread-id", "x-openai-subagent"} {
		if v := headers.Get(k); v != "" && in.ClientMetadata[k] == "" {
			in.ClientMetadata[k] = v
		}
	}
	for k, v := range map[string]string{"session_id": in.SessionID, "thread_id": in.ThreadID, "x-codex-installation-id": installation} {
		if in.ClientMetadata[k] == "" {
			in.ClientMetadata[k] = v
		}
	}
	if instruction, exists := body["instructions"]; exists {
		var text string
		if json.Unmarshal(instruction, &text) != nil {
			return in, adapter, errors.New("instructions must be a string")
		}
		in.Instructions = &text
	}
	var reasoning struct{ Effort, Summary string }
	if r := body["reasoning"]; len(r) > 0 && json.Unmarshal(r, &reasoning) != nil {
		return in, adapter, errors.New("invalid reasoning")
	}
	in.Effort, in.Summary = reasoning.Effort, reasoning.Summary
	if raw, exists := body["service_tier"]; exists && json.Unmarshal(raw, &in.ServiceTier) != nil {
		return in, adapter, errors.New("invalid service_tier")
	}
	in.ParallelToolCalls = true
	if b, exists := body["parallel_tool_calls"]; exists && json.Unmarshal(b, &in.ParallelToolCalls) != nil {
		return in, adapter, errors.New("invalid parallel_tool_calls")
	}
	var text struct {
		Verbosity string
		Format    struct {
			Type   string
			Schema json.RawMessage
			Strict bool
		}
	}
	if b := body["text"]; len(b) > 0 && json.Unmarshal(b, &text) != nil {
		return in, adapter, errors.New("invalid text format")
	}
	in.Verbosity = text.Verbosity
	if text.Format.Type == "json_schema" {
		in.OutputSchema = text.Format.Schema
		in.OutputSchemaStrict = text.Format.Strict
	}
	if b, exists := body["prompt_cache_key"]; exists {
		var key string
		if json.Unmarshal(b, &key) != nil {
			return in, adapter, errors.New("invalid prompt_cache_key")
		}
		in.PromptCacheKey = &key
	}
	var prompt string
	if json.Unmarshal(body["input"], &prompt) == nil {
		item, _ := marshalGatewayRequestJSON(map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}})
		in.Input = []json.RawMessage{item}
	} else if json.Unmarshal(body["input"], &in.Input) != nil || in.Input == nil {
		return in, adapter, errors.New("input must be a string or array")
	}
	for i, item := range in.Input {
		var v map[string]json.RawMessage
		if json.Unmarshal(item, &v) != nil || v == nil {
			return in, adapter, errors.New("invalid input item")
		}
		if _, exists := v["type"]; !exists && len(v["role"]) > 0 {
			v["type"] = json.RawMessage(`"message"`)
		}
		var content string
		if json.Unmarshal(v["content"], &content) == nil {
			kind := "input_text"
			if string(v["role"]) == `"assistant"` {
				kind = "output_text"
			}
			v["content"], _ = marshalGatewayRequestJSON([]any{map[string]string{"type": kind, "text": content}})
		}
		in.Input[i], _ = marshalGatewayRequestJSON(v)
	}
	tools := body["tools"]
	if len(tools) == 0 {
		tools = json.RawMessage(`[]`)
	}
	var declared []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(tools, &declared) != nil {
		return in, adapter, errors.New("invalid client tools")
	}
	for _, tool := range declared {
		if tool.Type == "web_search" || tool.Type == "web_search_preview" {
			in.StandardTools = true
			break
		}
	}
	if in.StandardTools {
		delete(in.ClientMetadata, "ws_request_header_x_openai_internal_codex_responses_lite")
	}
	adapter, err = buildAppServerTools(tools, meta.UseResponsesLite && !in.StandardTools)
	if err != nil {
		return in, adapter, err
	}
	in.Tools = adapter.Tools
	in.Input, err = adapter.PrepareInput(in.Input)
	if err != nil {
		return in, adapter, err
	}
	in.ClientControls = map[string]json.RawMessage{}
	for key, value := range body {
		switch key {
		case "model", "input", "tools", "instructions", "client_metadata", "stream", "store", "prompt_cache_key", "reasoning", "service_tier", "text", "parallel_tool_calls":
			continue
		}
		in.ClientControls[key] = value
	}
	if choice, exists := body["tool_choice"]; exists {
		in.ClientControls["tool_choice"], err = adapter.PrepareToolChoice(choice)
		if err != nil {
			return in, adapter, err
		}
	}
	for _, key := range []string{"service_tier", "parallel_tool_calls"} {
		if value, explicit := body[key]; explicit {
			in.ClientControls[key] = value
		}
	}
	if b := body["text"]; len(b) > 0 {
		var explicit map[string]json.RawMessage
		_ = json.Unmarshal(b, &explicit)
		if explicit != nil && meta.SupportVerbosity && explicit["verbosity"] == nil {
			explicit["verbosity"], _ = json.Marshal(meta.DefaultVerbosity)
		}
		if explicit != nil {
			in.ClientControls["text"], _ = marshalGatewayRequestJSON(explicit)
		}
	}
	if b := body["reasoning"]; len(b) > 0 {
		var explicit map[string]json.RawMessage
		_ = json.Unmarshal(b, &explicit)
		if explicit != nil {
			var merged map[string]json.RawMessage
			wire, buildErr := buildAppServerRequest(in)
			if buildErr != nil {
				return in, adapter, buildErr
			}
			var built map[string]json.RawMessage
			_ = json.Unmarshal(wire.Body, &built)
			_ = json.Unmarshal(built["reasoning"], &merged)
			for key, value := range explicit {
				// The hosted writer maps Codex configuration enums to model
				// defaults. An explicit Responses API effort is caller-owned.
				merged[key] = value
			}
			in.ClientControls["reasoning"], _ = marshalGatewayRequestJSON(merged)
		}
	}
	return in, adapter, err
}

func (a *appServerCLI) RoundTrip(req *http.Request, client *http.Client, proxy *ProxyEntry, prepared []byte, originalBody ...[]byte) (*http.Response, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(prepared, &body) != nil {
		return nil, errors.New("invalid prepared request")
	}
	if req.Header.Get("X-Openai-Internal-Codex-Responses-Lite") == "true" || len(body["previous_response_id"]) > 0 || len(body["generate"]) > 0 {
		return a.roundTripNative(req, client, proxy, prepared)
	}
	var items []struct{ Type string }
	_ = json.Unmarshal(body["input"], &items)
	for _, item := range items {
		if item.Type == "additional_tools" {
			return a.roundTripNative(req, client, proxy, prepared)
		}
	}
	// prepareHTTPBody inserts instructions:"" for generic Codex compatibility.
	// Only an actual caller/policy override should suppress catalog defaults.
	// Native Lite/delta bypass above remains byte-transparent.
	if len(originalBody) > 0 {
		var original map[string]json.RawMessage
		if json.Unmarshal(originalBody[0], &original) != nil || original == nil {
			return nil, errors.New("invalid original App Server request")
		}
		var instructions string
		if _, explicit := original["instructions"]; !explicit && json.Unmarshal(body["instructions"], &instructions) == nil && instructions == "" {
			delete(body, "instructions")
			var err error
			prepared, err = marshalGatewayRequestJSON(body)
			if err != nil {
				return nil, err
			}
		}
	}
	var name string
	_ = json.Unmarshal(body["model"], &name)
	model, rejection, err := a.model(req, client, proxy, name)
	if err != nil || rejection != nil {
		if err != nil {
			officialObserveAttemptFailure(req.Context(), err)
		}
		return rejection, err
	}
	defaults := appServerRequestDefaults(req, a.installation)
	identityHeaders := appServerRequestIdentityHeaders(req.Header, body, defaults)
	in, adapter, err := prepareAppServerCLI(prepared, identityHeaders, model, a.installation)
	if err != nil {
		return nil, err
	}
	profile, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in, Defaults: defaults, MirrorAppServer: true})
	if err != nil {
		return nil, err
	}
	in = profile.Request
	wire, err := buildAppServerRequest(in)
	if err != nil {
		return nil, err
	}
	gatewayAttemptObservationFrom(req.Context()).ObserveSent(req.URL.Path, wire.Body)
	appServerCostObservationFrom(req.Context()).BindSent(wire.Body, wire.Headers)
	var res *http.Response
	if a.prefersWebSocket() && strings.HasSuffix(req.URL.Path, "/responses") {
		wsWire, buildErr := buildAppServerWebSocketRequest(in, appServerWebSocketRequestState{StreamRequestStartMS: time.Now().UnixMilli()})
		if buildErr != nil {
			return nil, buildErr
		}
		res, err = a.roundTripPreferredWebSocket(req, client, proxy, wire, wsWire)
	} else if in.StandardTools {
		// Captured standard HTTP emits real web_search_call events where the
		// Lite additional_tools variant does not execute the hosted search.
		// This is one backend request, with no local search or agent loop.
		res, err = a.roundTripHTTP(req, client, wire)
	} else {
		res, err = a.transport.RoundTripPrepared(req, proxy, in)
	}
	var transportFailure *appServerTransportError
	if err != nil && errors.As(err, &transportFailure) && !transportFailure.GenerationSent &&
		(errors.Is(err, errAppServerWSHandshakeUnavailable) || errors.Is(err, errNativeWSCapability)) {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		// A rejected handshake has never started inference. Unlike repeating
		// a generation, one HTTP fallback is safe and avoids retrying the same
		// unavailable WebSocket several times before producing a first token.
		res, err = a.roundTripHTTP(req, client, wire)
	}
	if err != nil || res == nil {
		var upstream *appServerTransportUpstreamError
		if errors.As(err, &upstream) {
			// Prewarm can return a complete structured rejection before a
			// generation is sent. Expose that event through the same complete
			// SSE boundary so the existing account classifier can act on it.
			observeAppServerCLIFailoverEvent(req.Context(), upstream.Event)
			res = &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(bytes.NewReader(append(append([]byte("data: "), upstream.Event...), []byte("\n\n")...))), Request: req}
			err = nil
		} else {
			if err != nil {
				officialObserveAttemptFailure(req.Context(), err)
			}
			return res, err
		}
	}
	if res.StatusCode >= 300 {
		return res, nil
	}
	res.Body = newAppServerMappedStream(res.Body, adapter, gatewayUnboundedPayload(req.Context()))
	return res, nil
}

// The native client has already constructed its model context and delta.
// Apply only the measured App Server metadata projection, never a second
// instruction/tool prefix or hidden runtime state.
func (a *appServerCLI) roundTripNative(req *http.Request, client *http.Client, proxy *ProxyEntry, prepared []byte) (*http.Response, error) {
	body, headers, changed, err := appServerMirrorNativeMetadata(prepared, req.Header)
	if err != nil {
		return nil, err
	}
	appServerCostObservationFrom(req.Context()).BindSent(body, headers)
	if a.prefersWebSocket() && strings.HasSuffix(req.URL.Path, "/responses") {
		return a.roundTripPreferredWebSocket(req, client, proxy, appServerRequestWire{Body: body, Headers: headers})
	}
	if !changed {
		return client.Do(req)
	}
	return a.roundTripHTTP(req, client, appServerRequestWire{Body: body, Headers: headers})
}

func (a *appServerCLI) roundTripPreferredWebSocket(req *http.Request, client *http.Client, proxy *ProxyEntry, wire appServerRequestWire, websocketWire ...appServerRequestWire) (*http.Response, error) {
	settings, err := a.owner.settingsValue()
	if err != nil {
		return nil, err
	}
	if route := gatewayWSAccountFrom(req.Context()); a.owner.wsDialSkipped(route.ID, proxy, gatewayObservedRoute(req.URL.Path)) {
		// Known-wasted dial for this account/egress/endpoint pairing. Run the
		// HTTP wire inside the already-begun attempt instead of opening a
		// websocket-labelled attempt that never reaches the network.
		return a.roundTripHTTP(req, client, wire)
	}
	up := req.Clone(req.Context())
	selected := wire
	if len(websocketWire) > 0 {
		selected = websocketWire[0]
	}
	up.Header = selected.Headers.Clone()
	for _, name := range []string{"Authorization", "ChatGPT-Account-Id"} {
		up.Header.Del(name)
		for _, value := range req.Header.Values(name) {
			up.Header.Add(name, value)
		}
	}
	response, err := a.owner.preferredResponsesWebSocket(up, proxy, selected.Body, settings)
	if !errors.Is(err, errPreferredWebSocketUnavailable) {
		return response, err
	}
	first, _ := req.Context().Value(gatewayTransportAttemptKey{}).(*gatewayAttemptHandle)
	finishGatewayObservedAttempt(first, response, err)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	fallback := gatewayAttemptObservationFrom(req.Context()).Begin("request", req.URL.EscapedPath(), time.Now())
	response, err = a.roundTripHTTP(req, client, wire)
	finishGatewayObservedAttempt(fallback, response, err)
	return response, err
}

func (a *appServerCLI) prefersWebSocket() bool {
	if a.owner == nil || a.owner.Settings == nil {
		return false
	}
	settings, err := a.owner.settingsValue()
	return err == nil && settings.PreferWebsocket
}

// roundTripHTTP sends a built profile through the admitted account's existing
// native transport, including native compression when available. Shared by
// standard hosted tools and safe pre-generation WebSocket fallback.
func (a *appServerCLI) roundTripHTTP(req *http.Request, client *http.Client, wire appServerRequestWire) (*http.Response, error) {
	ctx := withoutGatewayNativeRequestCompression(req.Context())
	if a.owner != nil && a.owner.nativeHTTPFor(ctx) != nil {
		ctx = withGatewayNativeRequestCompression(ctx)
	}
	up := req.Clone(ctx)
	up.Header = wire.Headers.Clone()
	up.Header.Del("Content-Encoding")
	up.Header.Del("Content-Length")
	up.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"Authorization", "ChatGPT-Account-Id"} {
		up.Header.Del(name)
		for _, value := range req.Header.Values(name) {
			up.Header.Add(name, value)
		}
	}
	up.Body = io.NopCloser(bytes.NewReader(wire.Body))
	up.ContentLength = int64(len(wire.Body))
	up.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(wire.Body)), nil }
	return client.Do(up)
}

// Mapping is per event with backpressure and propagates cancellation through
// Close to the original socket body. It never buffers the whole generation.
type appServerMappedStream struct {
	reader   *io.PipeReader
	upstream io.ReadCloser
	once     sync.Once
}

func newAppServerMappedStream(upstream io.ReadCloser, adapter appServerToolAdapter, unbounded ...bool) io.ReadCloser {
	r, w := io.Pipe()
	stream := &appServerMappedStream{reader: r, upstream: upstream}
	go func() {
		defer upstream.Close()
		unlimited := len(unbounded) > 0 && unbounded[0]
		var scanner gatewaySSELines
		if unlimited {
			scanner = newGatewaySSELines(upstream, true)
		} else {
			legacy := bufio.NewScanner(upstream)
			legacy.Buffer(make([]byte, 64<<10), maxBody)
			scanner = legacy
		}
		var data []byte
		type mappedLine struct {
			raw       []byte
			dataField bool
			bom       bool
		}
		var lines []mappedLine
		var started, hasData bool
		emit := func() error {
			if len(lines) == 0 {
				return nil
			}
			mapped := data
			if len(data) != 0 {
				var err error
				mapped, err = adapter.MapEvent(data)
				if err != nil {
					return err
				}
			}
			changed, dataWritten := !bytes.Equal(data, mapped), false
			for _, source := range lines {
				line := source.raw
				if source.dataField && changed {
					if dataWritten {
						continue
					}
					line = append([]byte("data: "), mapped...)
					if source.bom {
						line = append([]byte{0xef, 0xbb, 0xbf}, line...)
					}
					dataWritten = true
				}
				if _, err := w.Write(append(line, '\n')); err != nil {
					return err
				}
			}
			_, err := w.Write([]byte{'\n'})
			data = nil
			lines = nil
			hasData = false
			return err
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			parsed := line
			if unlimited {
				parsed = stripInitialSSEBOM(line, &started)
				if !validSSELine(parsed) {
					_ = w.CloseWithError(errors.New("upstream SSE contains invalid UTF-8"))
					return
				}
			}
			if len(parsed) == 0 {
				if err := emit(); err != nil {
					_ = w.CloseWithError(err)
					return
				}
				continue
			}
			// Preserve event/id/retry fields and heartbeat comments. Only data
			// whose tool identity actually changes is reserialized.
			dataField := bytes.HasPrefix(parsed, []byte("data:")) || unlimited && bytes.Equal(parsed, []byte("data"))
			lines = append(lines, mappedLine{raw: append([]byte(nil), line...), dataField: dataField, bom: len(parsed) != len(line)})
			if dataField {
				if len(data) > 0 || unlimited && hasData {
					data = append(data, '\n')
				}
				if len(parsed) >= 5 {
					data = append(data, bytes.TrimPrefix(parsed[5:], []byte(" "))...)
				}
				hasData = true
			}
		}
		if err := scanner.Err(); err != nil {
			_ = w.CloseWithError(err)
			return
		}
		_ = w.CloseWithError(emit())
	}()
	return stream
}
func (s *appServerMappedStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *appServerMappedStream) Close() error {
	s.once.Do(func() { _ = s.reader.Close(); _ = s.upstream.Close() })
	return nil
}
