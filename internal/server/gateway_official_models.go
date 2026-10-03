package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func officialModels(ctx context.Context, rpc *officialRPC) ([]map[string]json.RawMessage, error) {
	var entries []map[string]json.RawMessage
	cursor := ""
	seen := map[string]bool{}
	for {
		params := map[string]any{"includeHidden": true, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := rpc.call(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Data []map[string]json.RawMessage `json:"data"`
			Next string                       `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Data == nil {
			return nil, errors.New("invalid official model catalog")
		}
		entries = append(entries, page.Data...)
		if len(entries) > 512 {
			return nil, errors.New("official model catalog too large")
		}
		if page.Next == "" {
			break
		}
		if seen[page.Next] {
			return nil, errors.New("invalid model catalog pagination")
		}
		seen[page.Next] = true
		cursor = page.Next
	}
	// Prefer the official process's just-fetched rich catalog. It preserves CLI
	// tool/model metadata which the public RPC picker intentionally omits.
	cached := map[string]map[string]json.RawMessage{}
	if f, err := os.Open(filepath.Join(rpc.home, "models_cache.json")); err == nil {
		var value struct {
			Models []map[string]json.RawMessage `json:"models"`
		}
		if json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&value) == nil {
			for _, m := range value.Models {
				var slug string
				_ = json.Unmarshal(m["slug"], &slug)
				if slug != "" {
					cached[slug] = m
				}
			}
		}
		_ = f.Close()
	}
	models := make([]map[string]json.RawMessage, 0, len(entries))
	set := func(m map[string]json.RawMessage, k string, v any) { m[k], _ = json.Marshal(v) }
	for i, entry := range entries {
		var id string
		_ = json.Unmarshal(entry["model"], &id)
		if id == "" {
			_ = json.Unmarshal(entry["id"], &id)
		}
		if id == "" {
			return nil, errors.New("official model has no identifier")
		}
		m := cached[id]
		if m == nil {
			m = map[string]json.RawMessage{}
			for _, pair := range [][2]string{{"displayName", "display_name"}, {"description", "description"}, {"defaultReasoningEffort", "default_reasoning_level"}, {"inputModalities", "input_modalities"}} {
				if v := entry[pair[0]]; len(v) > 0 {
					m[pair[1]] = v
				}
			}
			var effortValues []struct {
				Effort      string `json:"reasoningEffort"`
				Description string `json:"description"`
			}
			_ = json.Unmarshal(entry["supportedReasoningEfforts"], &effortValues)
			levels := []map[string]string{}
			for _, v := range effortValues {
				levels = append(levels, map[string]string{"effort": v.Effort, "description": v.Description})
			}
			set(m, "supported_reasoning_levels", levels)
			set(m, "shell_type", "unified_exec")
			set(m, "visibility", "list")
			set(m, "supported_in_api", true)
			set(m, "priority", i)
			set(m, "truncation_policy", map[string]any{"mode": "bytes", "limit": 10000})
			set(m, "apply_patch_tool_type", "freeform")
			set(m, "experimental_supported_tools", []string{})
			set(m, "base_instructions", "You are a coding agent. Follow the user's instructions and use the tools supplied by the client.")
		}
		set(m, "id", id)
		set(m, "slug", id)
		set(m, "object", "model")
		set(m, "owned_by", "openai")
		models = append(models, m)
	}
	return models, nil
}
