package bps

import (
	"encoding/json"
	"fmt"
)

// AccessCatalog follows the actual BPS /responses/access?include_models=true
// response. Unknown effort values remain strings, as Codex 0.155 permits them.
type AccessCatalog struct {
	Allowed      bool            `json:"allowed"`
	DenialReason json.RawMessage `json:"denial_reason,omitempty"`
	ModelCatalog struct {
		Models           []AccessModel     `json:"models"`
		DefaultModel     string            `json:"default_model"`
		Source           string            `json:"source"`
		RestrictedModels []json.RawMessage `json:"restricted_models"`
	} `json:"model_catalog"`
}
type AccessModel struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Efforts     []struct {
		Value       string `json:"value"`
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"efforts"`
	DefaultEffort string `json:"default_effort"`
}

func ParseAccessCatalog(input []byte) (*AccessCatalog, error) {
	var catalog AccessCatalog
	if err := json.Unmarshal(input, &catalog); err != nil {
		return nil, fmt.Errorf("bps: invalid access catalog: %w", err)
	}
	if !catalog.Allowed {
		return nil, fmt.Errorf("bps: access catalog reports access denied")
	}
	seen := map[string]bool{}
	for _, model := range catalog.ModelCatalog.Models {
		if model.ID == "" || seen[model.ID] {
			return nil, fmt.Errorf("bps: empty or duplicate model id")
		}
		seen[model.ID] = true
		efforts := map[string]bool{}
		for _, effort := range model.Efforts {
			if effort.Value == "" || efforts[effort.Value] {
				return nil, fmt.Errorf("bps: empty or duplicate reasoning effort for %s", model.ID)
			}
			efforts[effort.Value] = true
		}
		if model.DefaultEffort != "" && !efforts[model.DefaultEffort] {
			return nil, fmt.Errorf("bps: default effort is not advertised for %s", model.ID)
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("bps: access catalog contains no models")
	}
	return &catalog, nil
}

// ModelsFromAccess returns the Codex /models envelope. Required fields and enums
// follow codex-rs/protocol/src/openai_models.rs (0.155). Shell/truncation entries
// configure the local client, not upstream context limits. No context size, max
// output, provider instructions, pricing or effort is invented from model names.
func ModelsFromAccess(input []byte) ([]byte, error) {
	return ModelsFromAccessWithInstructions(input, DefaultClientInstructions)
}

// DefaultClientInstructions is gateway-owned client configuration, not a BPS
// provider prompt. Codex 0.155 requires a base/template instruction string for
// every catalog model; omitting both makes its decoder reject the model.
const DefaultClientInstructions = "You are a coding agent working in the user's workspace. Follow the user's request and applicable workspace instructions. Use the tools supplied by the client to inspect, edit, and verify work. Report actual tool results accurately."

func ModelsFromAccessWithInstructions(input []byte, instructions string) ([]byte, error) {
	catalog, err := ParseAccessCatalog(input)
	if err != nil {
		return nil, err
	}
	models := make([]map[string]any, 0, len(catalog.ModelCatalog.Models))
	for index, m := range catalog.ModelCatalog.Models {
		efforts := make([]map[string]string, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			description := e.Description
			if description == "" {
				description = e.Label
			}
			efforts = append(efforts, map[string]string{"effort": e.Value, "description": description})
		}
		label := m.Label
		if label == "" {
			label = m.ID
		}
		priority := index + 1
		if m.ID == catalog.ModelCatalog.DefaultModel {
			priority = 0
		}
		model := map[string]any{
			"slug": m.ID, "display_name": label, "description": m.Description,
			"supported_reasoning_levels": efforts, "shell_type": "unified_exec",
			"visibility": "list", "supported_in_api": true, "priority": priority,
			// This transport does not accept explicit priority/ultrafast tiers.
			// Declare an empty list: downstream catalog completion must not fill
			// missing fields with model-name-based capabilities from another API.
			"service_tiers": []any{}, "additional_speed_tiers": []string{},
			"availability_nux": nil, "upgrade": nil, "support_verbosity": false,
			"default_verbosity": nil, "apply_patch_tool_type": "freeform",
			"truncation_policy":            map[string]any{"mode": "bytes", "limit": 10000},
			"experimental_supported_tools": []string{}, "input_modalities": []string{"text"},
			"supports_search_tool": true, "web_search_tool_type": "text",
			"supports_reasoning_summary_parameter": false,
			"base_instructions":                    instructions,
		}
		if m.DefaultEffort != "" {
			model["default_reasoning_level"] = m.DefaultEffort
		}
		models = append(models, model)
	}
	return json.Marshal(map[string]any{"models": models})
}

// OpenAIModelsFromAccess is the /v1/models list form, for clients which do not
// consume Codex-specific model metadata.
func OpenAIModelsFromAccess(input []byte) ([]byte, error) {
	catalog, err := ParseAccessCatalog(input)
	if err != nil {
		return nil, err
	}
	data := make([]map[string]any, 0, len(catalog.ModelCatalog.Models))
	for _, m := range catalog.ModelCatalog.Models {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "openai"})
	}
	return json.Marshal(map[string]any{"object": "list", "data": data})
}
