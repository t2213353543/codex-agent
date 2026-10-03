package bps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ToolPolicyObservation contains only trusted enums, counts, booleans and hashes. A
// RequestRecord should hold an optional pointer; old/unobserved records omit it.
// Counts cover the effective catalog, including additional_tools/history and
// successful server discovery, not only the request's top-level tools array.
type ToolPolicyObservation struct {
	RejectedNameSHA256                  string `json:"rejected_name_sha256,omitempty"`
	RejectedNameOtherFamily             *bool  `json:"rejected_name_other_family,omitempty"`
	InboundFamilyNameDeclared           *bool  `json:"inbound_family_name_declared,omitempty"`
	SerializedInitialFamilyNameDeclared *bool  `json:"serialized_initial_family_name_declared,omitempty"`
	RejectedKnownBuiltin                string `json:"rejected_known_builtin,omitempty"`
	ResolvedToolsSHA256                 string `json:"resolved_tools_sha256,omitempty"`
	ResolvedPromptSHA256                string `json:"resolved_prompt_sha256,omitempty"`
	EffectiveChoice                     string `json:"effective_choice"`
	CatalogAtPrepareCount               int    `json:"catalog_at_prepare_count"`
	NamedNamespacesAtPrepareCount       int    `json:"named_namespaces_at_prepare_count"`
	ToolSearchAtPrepare                 bool   `json:"tool_search_at_prepare"`
	Structured                          bool   `json:"structured"`
	CatalogAtRejectionCount             *int   `json:"catalog_at_rejection_count,omitempty"`
	NamedNamespacesAtRejectionCount     *int   `json:"named_namespaces_at_rejection_count,omitempty"`
	RejectionPhase                      string `json:"rejection_phase,omitempty"`
	RejectedFamily                      string `json:"rejected_family,omitempty"`
	RejectedIdentity                    string `json:"rejected_identity,omitempty"`
	RejectedMatchesExactDeclaration     *bool  `json:"rejected_matches_exact_declaration,omitempty"`
}

// EffectiveToolChoice reads the validated policy, never a free-form error or
// request string. Unknown/zero policies remain unobserved instead of invented.
func (p Policy) EffectiveToolChoice() (string, bool) {
	switch p.choice {
	case "auto", "none", "required", "forced":
		return p.choice, true
	default:
		return "", false
	}
}

func toolPolicyCatalogCounts(c *ToolCatalog) (int, int) {
	if c == nil {
		return 0, 0
	}
	namespaces := make(map[string]struct{})
	for identity := range c.definitions {
		// The empty namespace is a valid global declaration, not unknown.
		// The metric explicitly counts named namespaces only.
		if identity.Namespace != "" {
			namespaces[identity.Namespace] = struct{}{}
		}
	}
	return len(c.definitions), len(namespaces)
}

func ObserveToolPolicy(r *Request) *ToolPolicyObservation {
	if r == nil || r.Tools == nil {
		return nil
	}
	choice, known := r.Policy.EffectiveToolChoice()
	if !known {
		return nil
	}
	count, namespaces := toolPolicyCatalogCounts(r.Tools)
	return &ToolPolicyObservation{EffectiveChoice: choice, CatalogAtPrepareCount: count,
		NamedNamespacesAtPrepareCount: namespaces, ToolSearchAtPrepare: r.Tools.ToolSearchDeclared,
		Structured: r.Structured != nil}
}

// ToolPolicyRejection retains no item, tool identity, schema, payload or error.
// Capture before calling the existing normalizer; commit only if that exact
// normalizer/budget check fails. This avoids repeating validation or observing
// a namespace/catalog mutation performed by the failing operation.
type ToolPolicyRejection struct {
	nameHash                                  [32]byte
	nameKnown                                 bool
	nameFamily                                uint8
	nameOtherFamily                           bool
	knownBuiltin                              string
	inboundFamilyNameDeclared                 *bool
	serializedInitialFamilyNameDeclared       *bool
	resolvedToolsSHA256, resolvedPromptSHA256 string
	known                                     bool
	count                                     int
	namespaces                                int
	family                                    string
	identity                                  string
	exact                                     bool
	exactKnown                                bool
}

func CaptureToolPolicyRejection(r *Request, item map[string]json.RawMessage) ToolPolicyRejection {
	if r == nil || r.Tools == nil {
		return ToolPolicyRejection{}
	}
	count, namespaces := toolPolicyCatalogCounts(r.Tools)
	value := ToolPolicyRejection{known: true, count: count, namespaces: namespaces}
	if item == nil {
		// Whole-response policy failures (e.g. required with no calls) do not
		// identify a particular returned item. Do not invent one from the error.
		return value
	}
	kind := stringField(item, "type")
	switch kind {
	case "function_call", "custom_tool_call", "tool_search_call", "tool_search_output", "web_search_call":
		value.family = kind
	default:
		value.family = "other"
		if strings.HasSuffix(kind, "_call") {
			value.family = "other_call"
		}
	}
	if IsStructuredFormattingItem(r.Structured, item) {
		value.identity = "internal_formatter"
		return value
	}
	if kind != "function_call" && kind != "custom_tool_call" {
		value.identity = "not_client_action"
		return value
	}
	name := stringField(item, "name")
	if name == "" {
		value.identity = "unobserved"
		return value
	}
	typeName := "function"
	if kind == "custom_tool_call" {
		typeName = "custom"
	}
	value.nameHash, value.nameKnown = sha256.Sum256([]byte(name)), true
	value.nameFamily = toolPolicyFamilyMask(typeName)
	value.knownBuiltin = "not_builtin"
	if name == "update_plan" {
		value.knownBuiltin = "update_plan"
	}
	for identity := range r.Tools.definitions {
		if identity.Name == name && identity.Type != typeName {
			value.nameOtherFamily = true
			break
		}
	}
	namespace := stringField(item, "namespace")
	_, value.exact = r.Tools.definitions[ToolIdentity{Type: typeName, Name: name, Namespace: namespace}]
	value.exactKnown = true
	if namespace != "" {
		value.identity = "undeclared"
		if value.exact {
			value.identity = "exact"
		}
		return value
	}
	matches := 0
	for identity := range r.Tools.definitions {
		if identity.Type == typeName && identity.Name == name {
			matches++
		}
	}
	switch {
	case matches == 0:
		value.identity = "undeclared"
	case matches > 1:
		value.identity = "ambiguous_namespace"
	case value.exact:
		value.identity = "exact"
	default:
		value.identity = "unique_namespace"
	}
	return value
}

// RecordRejection has no effect on execution, routing, failover or accounting.
// An existing first actual rejection is not overwritten by later cleanup.
func (o *ToolPolicyObservation) RecordRejection(phase string, value ToolPolicyRejection) {
	if o == nil || o.RejectionPhase != "" || !value.known {
		return
	}
	switch phase {
	case "normalize_item", "parallel_policy", "validate_response":
	default:
		return
	}
	o.RejectionPhase, o.RejectedFamily, o.RejectedIdentity = phase, value.family, value.identity
	o.ResolvedToolsSHA256, o.ResolvedPromptSHA256 = value.resolvedToolsSHA256, value.resolvedPromptSHA256
	if value.nameKnown {
		o.RejectedNameSHA256 = hex.EncodeToString(value.nameHash[:])
		otherFamily := value.nameOtherFamily
		o.RejectedNameOtherFamily = &otherFamily
		o.RejectedKnownBuiltin = value.knownBuiltin
		o.InboundFamilyNameDeclared = value.inboundFamilyNameDeclared
		o.SerializedInitialFamilyNameDeclared = value.serializedInitialFamilyNameDeclared
	}
	count, namespaces := value.count, value.namespaces
	o.CatalogAtRejectionCount, o.NamedNamespacesAtRejectionCount = &count, &namespaces
	if value.exactKnown {
		exact := value.exact
		o.RejectedMatchesExactDeclaration = &exact
	}
}

// WithRequestDeclarationEvidence is invoked once, only for the first actual
// rejection. Successful streams do no additional body parsing or name hashing.
// Both bodies are borrowed writer bytes; neither is retained in the observation.
// SerializedInitial names the catalog sent before any response-side discovery.
// A discovered tool can therefore match the effective catalog but not this
// initial snapshot; false alone is never evidence of a lost declaration.
func (value ToolPolicyRejection) WithRequestDeclarationEvidence(inbound, serialized []byte, toolsHash, promptHash string) ToolPolicyRejection {
	value.resolvedToolsSHA256 = toolPolicyValidSHA256(toolsHash)
	value.resolvedPromptSHA256 = toolPolicyValidSHA256(promptHash)
	if !value.nameKnown {
		return value
	}
	if snapshot, known := toolPolicyDeclarationSnapshot(inbound); known {
		matched := snapshot[value.nameHash]&value.nameFamily != 0
		value.inboundFamilyNameDeclared = &matched
	}
	if snapshot, known := toolPolicyDeclarationSnapshot(serialized); known {
		matched := snapshot[value.nameHash]&value.nameFamily != 0
		value.serializedInitialFamilyNameDeclared = &matched
	}
	return value
}

func toolPolicyFamilyMask(kind string) uint8 {
	if kind == "function" {
		return 1
	}
	if kind == "custom" {
		return 2
	}
	return 0
}

// Use Prepare's declaration parser, including namespaces and discovered tools;
// schemas and arbitrary names live only during this rejected request's parse.
// The returned temporary lookup contains fixed hashes and two-bit family masks.
func toolPolicyDeclarationSnapshot(body []byte) (map[[32]byte]uint8, bool) {
	src, err := object(body)
	if err != nil {
		return nil, false
	}
	var additions []json.RawMessage
	if input := src["input"]; len(input) > 0 && !bytes.Equal(bytes.TrimSpace(input), []byte("null")) {
		if _, err := stringValue(input); err != nil {
			var items []struct {
				Type  string          `json:"type"`
				Tools json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(input, &items) != nil {
				return nil, false
			}
			for _, item := range items {
				if item.Type == "additional_tools" || item.Type == "tool_search_output" {
					additions = append(additions, item.Tools)
				}
			}
		}
	}
	catalog, err := collectTools(src["tools"], additions)
	if err != nil {
		return nil, false
	}
	lookup := make(map[[32]byte]uint8, len(catalog.Declared))
	for _, identity := range catalog.Declared {
		nameHash := sha256.Sum256([]byte(identity.Name))
		lookup[nameHash] |= toolPolicyFamilyMask(identity.Type)
	}
	return lookup, true
}

func toolPolicyValidSHA256(value string) string {
	if len(value) != 64 {
		return ""
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return ""
	}
	return strings.ToLower(value)
}
