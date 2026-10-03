package server

import (
	"bytes"
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	gatewayPromptCacheObservationBodyLimit = 2 << 20
	gatewayPromptCacheObservationScopes    = 4096
	gatewayPromptCacheObservationItems     = 128
	gatewayPromptCacheWireBlockBytes       = 16 << 10
	gatewayPromptCacheWireBlocks           = 64
)

// GatewayPromptCacheObservation contains process-salted hashes and finite
// classifications only. It is passive request metadata: reported usage and the
// accounting denominator remain the responsibility of RequestRecord.
type GatewayPromptCacheObservation struct {
	Version                    int    `json:"version"`
	Instance                   string `json:"instance"`
	Coverage                   string `json:"coverage"`
	BodyBytes                  int    `json:"body_bytes"`
	ScopeKind                  string `json:"scope_kind"`
	ScopeHash                  string `json:"scope_hash,omitempty"`
	HeaderSessionPresent       bool   `json:"header_session_present"`
	HeaderThreadPresent        bool   `json:"header_thread_present"`
	HeaderAliasPresent         bool   `json:"header_alias_present"`
	KeyMode                    string `json:"key_mode"`
	CacheKeyPresent            bool   `json:"cache_key_present"`
	CacheKeyHash               string `json:"cache_key_hash,omitempty"`
	AccountHash                string `json:"account_hash,omitempty"`
	ModelHash                  string `json:"model_hash,omitempty"`
	InstructionsHash           string `json:"instructions_hash,omitempty"`
	ToolsHash                  string `json:"tools_hash,omitempty"`
	ControlsHash               string `json:"controls_hash,omitempty"`
	PrefixHash                 string `json:"prefix_hash,omitempty"`
	InputKind                  string `json:"input_kind,omitempty"`
	InputItems                 int    `json:"input_items"`
	InputComparedItems         int    `json:"input_compared_items"`
	InputLimited               bool   `json:"input_limited"`
	StatefulInput              bool   `json:"stateful_input"`
	Comparison                 string `json:"comparison"`
	PreviousAgeMS              *int64 `json:"previous_age_ms,omitempty"`
	AccountChanged             bool   `json:"account_changed"`
	CacheKeyChanged            bool   `json:"cache_key_changed"`
	ModelChanged               bool   `json:"model_changed"`
	InstructionsChanged        bool   `json:"instructions_changed"`
	ToolsChanged               bool   `json:"tools_changed"`
	ControlsChanged            bool   `json:"controls_changed"`
	InputHistory               string `json:"input_history,omitempty"`
	SharedInputItems           int    `json:"shared_input_items"`
	PreviousInputItems         int    `json:"previous_input_items"`
	InputWireBytes             int    `json:"input_wire_bytes"`
	SharedInputWirePrefixBytes int    `json:"shared_input_wire_prefix_bytes"`
	InputWirePrefixLimited     bool   `json:"input_wire_prefix_limited"`
}

type gatewayPromptCacheSnapshot struct {
	scope          [32]byte
	account        [32]byte
	cacheKey       [32]byte
	model          [32]byte
	instructions   [32]byte
	tools          [32]byte
	controls       [32]byte
	input          [][32]byte
	inputWire      [][32]byte
	inputWireBytes int
	inputCount     int
	inputKind      string
	stateful       bool
	coverage       string
	at             time.Time
}

// The LRU contains no request bodies, identifiers, credentials, or exported
// records. HMAC keys are generated once per process observer, never persisted.
type gatewayPromptCacheObserver struct {
	mu       sync.Mutex
	key      [32]byte
	instance string
	latest   map[[32]byte]*list.Element
	order    list.List
}

func newGatewayPromptCacheObserver() (*gatewayPromptCacheObserver, error) {
	o := &gatewayPromptCacheObserver{latest: make(map[[32]byte]*list.Element)}
	if _, err := rand.Read(o.key[:]); err != nil {
		return nil, errors.New("cannot initialize prompt cache observation")
	}
	id := o.digest("instance", nil)
	o.instance = hex.EncodeToString(id[:16])
	return o, nil
}

func (o *gatewayPromptCacheObserver) digest(domain string, raw []byte) [32]byte {
	mac := hmac.New(sha256.New, o.key[:])
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(raw)
	var sum [32]byte
	copy(sum[:], mac.Sum(nil))
	return sum
}

func (o *gatewayPromptCacheObserver) fieldDigest(fields map[string][]byte, name string) [32]byte {
	raw, exists := fields[name]
	if !exists {
		return o.digest(name+":absent", nil)
	}
	canonical, _ := canonicalGatewayCacheJSON(raw)
	return o.digest(name, canonical)
}

func gatewayPromptCacheHashString(sum [32]byte) string { return hex.EncodeToString(sum[:]) }

// Observe receives the original inbound identity headers and FINAL prepared
// JSON (after account cache-key rewriting, before compression). It never reads
// Authorization. True thread identity is separate from a weak shared-cache or
// configuration anchor; weak scopes cannot prove conversation continuity.
func (o *gatewayPromptCacheObserver) Observe(r *http.Request, prepared []byte, tenant string, account Account, keyMode string) *GatewayPromptCacheObservation {
	return o.observeAt(r, prepared, tenant, account, keyMode, time.Now())
}

func (o *gatewayPromptCacheObserver) observeAt(r *http.Request, prepared []byte, tenant string, account Account, keyMode string, now time.Time) *GatewayPromptCacheObservation {
	if o == nil {
		return nil
	}
	rec := &GatewayPromptCacheObservation{Version: 1, Instance: o.instance, Coverage: "complete", BodyBytes: len(prepared),
		ScopeKind: "none", KeyMode: gatewayPromptCacheSafeKeyMode(keyMode), Comparison: "unscoped"}
	next := gatewayPromptCacheSnapshot{at: now, coverage: "complete"}
	next.account = o.digest("account", []byte(gatewayAccountRef(account)))
	rec.AccountHash = gatewayPromptCacheHashString(next.account)
	session, thread := gatewayPromptCacheHeaderIdentity(r)
	rec.HeaderSessionPresent, rec.HeaderThreadPresent = session != "", thread != ""
	alias := gatewayPromptCacheAliasIdentity(r)
	rec.HeaderAliasPresent = alias != ""
	var fields map[string][]byte
	switch {
	case len(prepared) > gatewayPromptCacheObservationBodyLimit:
		rec.Coverage, rec.Comparison = "body_limit", "skipped"
	case !json.Valid(prepared) || len(bytes.TrimSpace(prepared)) == 0 || bytes.TrimSpace(prepared)[0] != '{':
		rec.Coverage, rec.Comparison = "invalid_body", "skipped"
	default:
		fields = gatewayPromptCacheObservationFields(prepared)
		metadata := fields["client_metadata"]
		if len(metadata) > 0 && bytes.TrimSpace(metadata)[0] == '{' {
			identities := gatewayPromptCacheObservationFields(metadata)
			if session == "" {
				session = gatewayPromptCacheScalar(identities["session_id"])
			}
			if thread == "" {
				thread = gatewayPromptCacheScalar(identities["thread_id"])
			}
		}
		next.model, next.instructions = o.fieldDigest(fields, "model"), o.fieldDigest(fields, "instructions")
		next.tools, next.cacheKey = o.fieldDigest(fields, "tools"), o.fieldDigest(fields, "prompt_cache_key")
		var controls []byte
		for _, name := range []string{"reasoning", "reasoning_effort", "text", "tool_choice", "parallel_tool_calls", "prompt_cache_options", "prompt_cache_retention", "access_programs", "service_tier"} {
			sum := o.fieldDigest(fields, name)
			controls = append(controls, sum[:]...)
		}
		next.controls = o.digest("controls", controls)
		var prefix []byte
		for _, sum := range [][32]byte{next.model, next.instructions, next.tools, next.controls} {
			prefix = append(prefix, sum[:]...)
		}
		prefixHash := o.digest("prefix", prefix)
		rec.PrefixHash = gatewayPromptCacheHashString(prefixHash)
		rec.ModelHash, rec.InstructionsHash = gatewayPromptCacheHashString(next.model), gatewayPromptCacheHashString(next.instructions)
		rec.ToolsHash, rec.ControlsHash = gatewayPromptCacheHashString(next.tools), gatewayPromptCacheHashString(next.controls)
		rec.CacheKeyHash = gatewayPromptCacheHashString(next.cacheKey)
		rec.CacheKeyPresent = gatewayPromptCacheScalar(fields["prompt_cache_key"]) != ""
		next.stateful = gatewayPromptCacheScalar(fields["previous_response_id"]) != ""
		rec.StatefulInput = next.stateful
		o.observeInput(&next, fields["input"])
		rec.InputKind, rec.InputItems, rec.InputComparedItems = next.inputKind, next.inputCount, len(next.input)
		rec.InputLimited = next.inputCount > len(next.input)
		rec.InputWireBytes = next.inputWireBytes
		rec.InputWirePrefixLimited = next.inputWireBytes > gatewayPromptCacheWireBlockBytes*gatewayPromptCacheWireBlocks
	}
	var anchor string
	aliasScope := session == "" && thread == "" && alias != ""
	if aliasScope {
		session = alias
	}
	switch {
	case aliasScope:
		rec.ScopeKind = "alias_session"
	case session != "" && thread != "":
		rec.ScopeKind = "session_thread"
	case session != "":
		rec.ScopeKind = "session"
	case thread != "":
		rec.ScopeKind = "thread"
	case rec.Coverage == "complete" && rec.CacheKeyPresent:
		rec.ScopeKind, anchor = "cache_key", rec.CacheKeyHash
	case rec.Coverage == "complete":
		rec.ScopeKind, anchor = "prefix", rec.PrefixHash
	}
	if rec.ScopeKind == "none" || tenant == "" {
		return rec
	}
	scope, _ := json.Marshal([]string{tenant, rec.ScopeKind, session, thread, anchor})
	next.scope = o.digest("scope", scope)
	next.coverage = rec.Coverage
	rec.ScopeHash = gatewayPromptCacheHashString(next.scope)
	o.mu.Lock()
	defer o.mu.Unlock()
	if element := o.latest[next.scope]; element != nil {
		previous := element.Value.(gatewayPromptCacheSnapshot)
		if rec.Coverage == "complete" {
			o.compare(rec, previous, next)
		}
		element.Value = next
		o.order.MoveToFront(element)
	} else {
		if rec.Coverage == "complete" {
			rec.Comparison = "first_observed"
		}
		if len(o.latest) >= gatewayPromptCacheObservationScopes {
			oldest := o.order.Back()
			delete(o.latest, oldest.Value.(gatewayPromptCacheSnapshot).scope)
			o.order.Remove(oldest)
		}
		o.latest[next.scope] = o.order.PushFront(next)
	}
	return rec
}

func (o *gatewayPromptCacheObserver) compare(rec *GatewayPromptCacheObservation, previous, next gatewayPromptCacheSnapshot) {
	age := max(int64(0), next.at.Sub(previous.at).Milliseconds())
	rec.PreviousAgeMS = &age
	if previous.coverage != "complete" {
		rec.Comparison = "previous_unobserved"
		return
	}
	rec.Comparison = "compared"
	rec.AccountChanged, rec.CacheKeyChanged = previous.account != next.account, previous.cacheKey != next.cacheKey
	rec.ModelChanged, rec.InstructionsChanged = previous.model != next.model, previous.instructions != next.instructions
	rec.ToolsChanged, rec.ControlsChanged = previous.tools != next.tools, previous.controls != next.controls
	rec.InputHistory = "not_comparable"
	if previous.stateful || next.stateful || previous.inputKind != next.inputKind || (next.inputKind != "array" && next.inputKind != "string") {
		return
	}
	rec.PreviousInputItems = previous.inputCount
	// This is only an exact wire-byte prefix lower bound, not a token count.
	// Object ordering/whitespace may reduce it while semantic item hashes match;
	// a changed short final block cannot reveal its partially shared bytes.
	for index := 0; index < min(len(previous.inputWire), len(next.inputWire)); index++ {
		if previous.inputWire[index] != next.inputWire[index] {
			break
		}
		rec.SharedInputWirePrefixBytes += min(gatewayPromptCacheWireBlockBytes, previous.inputWireBytes-index*gatewayPromptCacheWireBlockBytes)
	}
	shared := 0
	for shared < min(len(previous.input), len(next.input)) && previous.input[shared] == next.input[shared] {
		shared++
	}
	rec.SharedInputItems = shared
	switch {
	case shared < min(len(previous.input), len(next.input)):
		rec.InputHistory = "changed"
	case next.inputCount < previous.inputCount:
		rec.InputHistory = "shortened"
	case previous.inputCount > len(previous.input):
		rec.InputHistory = "comparison_limited"
	case next.inputCount > previous.inputCount:
		rec.InputHistory = "extended"
	default:
		rec.InputHistory = "unchanged"
	}
}

func (o *gatewayPromptCacheObserver) observeInput(next *gatewayPromptCacheSnapshot, raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		next.inputKind = "absent"
		return
	}
	next.inputWireBytes = len(raw)
	for start := 0; start < min(len(raw), gatewayPromptCacheWireBlockBytes*gatewayPromptCacheWireBlocks); start += gatewayPromptCacheWireBlockBytes {
		end := min(start+gatewayPromptCacheWireBlockBytes, len(raw))
		next.inputWire = append(next.inputWire, o.digest("input-wire-block", raw[start:end]))
	}
	switch raw[0] {
	case '"':
		next.inputKind, next.inputCount = "string", 1
		canonical, _ := canonicalGatewayCacheJSON(raw)
		next.input = append(next.input, o.digest("input-item", canonical))
	case '[':
		next.inputKind = "array"
		position := gatewayJSONSkipSpace(raw, 1)
		for raw[position] != ']' {
			end := gatewayJSONValueEnd(raw, position)
			next.inputCount++
			if len(next.input) < gatewayPromptCacheObservationItems {
				canonical, _ := canonicalGatewayCacheJSON(raw[position:end])
				next.input = append(next.input, o.digest("input-item", canonical))
			}
			position = gatewayJSONSkipSpace(raw, end)
			if raw[position] == ',' {
				position = gatewayJSONSkipSpace(raw, position+1)
			}
		}
	default:
		next.inputKind = "other"
	}
}

// The caller validates this JSON once. Retain slices only during observation,
// skipping opaque unselected fields without unmarshalling or copying them.
func gatewayPromptCacheObservationFields(raw []byte) map[string][]byte {
	fields := make(map[string][]byte)
	position := gatewayJSONSkipSpace(raw, 0)
	if position >= len(raw) || raw[position] != '{' {
		return fields
	}
	position = gatewayJSONSkipSpace(raw, position+1)
	for raw[position] != '}' {
		nameStart := position
		nameEnd := gatewayJSONStringEnd(raw, position)
		position = gatewayJSONSkipSpace(raw, nameEnd)
		position = gatewayJSONSkipSpace(raw, position+1)
		end := gatewayJSONValueEnd(raw, position)
		// These are exact, case-sensitive protocol names. Unknown fields and
		// duplicate aliases cannot change the observation's scope or controls.
		for _, name := range []string{"model", "instructions", "tools", "input", "prompt_cache_key", "client_metadata", "session_id", "thread_id", "previous_response_id", "reasoning", "reasoning_effort", "text", "tool_choice", "parallel_tool_calls", "prompt_cache_options", "prompt_cache_retention", "access_programs", "service_tier"} {
			if gatewayJSONFieldName(raw[nameStart:nameEnd], name) {
				fields[name] = raw[position:end]
				break
			}
		}
		position = gatewayJSONSkipSpace(raw, end)
		if raw[position] == ',' {
			position = gatewayJSONSkipSpace(raw, position+1)
		}
	}
	return fields
}

func gatewayPromptCacheScalar(raw []byte) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func gatewayPromptCacheHeaderIdentity(r *http.Request) (session, thread string) {
	if r == nil {
		return "", ""
	}
	headers := make(http.Header)
	copyGatewayHeaders(headers, r.Header, func(name string) bool {
		return name == "session-id" || name == "session_id" || name == "thread-id" || name == "conversation_id"
	}, func(string) bool { return true })
	for _, name := range []string{"session-id", "session_id"} {
		if session = strings.Trim(headers.Get(name), " \t"); session != "" {
			break
		}
	}
	for _, name := range []string{"thread-id", "conversation_id"} {
		if thread = strings.Trim(headers.Get(name), " \t"); thread != "" {
			break
		}
	}
	return session, thread
}

func gatewayPromptCacheAliasIdentity(r *http.Request) string {
	if r == nil {
		return ""
	}
	headers := make(http.Header)
	copyGatewayHeaders(headers, r.Header, func(name string) bool {
		return name == "x-session-affinity" || name == "x-session-id" || name == "x-opencode-session" || name == "x-conversation-id"
	}, func(string) bool { return true })
	for _, name := range []string{"x-session-affinity", "x-session-id", "x-opencode-session", "x-conversation-id"} {
		if value := strings.Trim(headers.Get(name), " \t"); value != "" {
			return value
		}
	}
	return ""
}

// Call before policy changes the body. The mode is a classification only; the
// observer hashes the final sent cache key separately. Local session/content
// affinity never implies that an upstream cache key was generated.
func gatewayPromptCacheObservationKeyMode(r *http.Request, inbound []byte, settings Settings) string {
	if !settings.PromptCacheKeyEnabled {
		return "disabled"
	}
	if len(inbound) > gatewayPromptCacheObservationBodyLimit || !json.Valid(inbound) {
		return "unobserved"
	}
	fields := gatewayPromptCacheObservationFields(inbound)
	if key := gatewayPromptCacheScalar(fields["prompt_cache_key"]); key != "" {
		metadata := gatewayPromptCacheObservationFields(fields["client_metadata"])
		if key == gatewayPromptCacheScalar(metadata["session_id"]) {
			return "codex_default"
		}
		session, _ := gatewayPromptCacheHeaderIdentity(r)
		if session != "" && key == session {
			return "explicit_session_match"
		}
		return "explicit_override"
	}
	return "absent"
}

func gatewayPromptCacheSafeKeyMode(mode string) string {
	switch mode {
	case "disabled", "absent", "explicit", "explicit_override", "explicit_session_match", "codex_default":
		return mode
	case "generated_session", "generated_prefix", "shared_api_key":
		// BPS fills a reusable account-scoped prefix key after preparation.
		return mode
	}
	return "unobserved"
}
