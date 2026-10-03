package bps

import (
	"encoding/json"
	"fmt"
)

// NormalizeHistory restores namespaces omitted by older Codex history writers.
// Unlike output validation, history can legitimately reference tools no longer
// enabled on this turn. Preserve those items and every opaque payload verbatim;
// only a unique type/name match authorizes adding a missing namespace.
func (c *ToolCatalog) NormalizeHistory(items []json.RawMessage) ([]json.RawMessage, error) {
	for index, rawItem := range items {
		item, err := object(rawItem)
		if err != nil {
			return nil, err
		}
		kind := stringField(item, "type")
		toolType := "function"
		if kind == "custom_tool_call" {
			toolType = "custom"
		} else if kind != "function_call" {
			continue
		}
		if stringField(item, "namespace") != "" {
			continue
		}
		name := stringField(item, "name")
		var matches []ToolIdentity
		for identity := range c.definitions {
			if identity.Type == toolType && identity.Name == name {
				matches = append(matches, identity)
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("bps: historical tool %s/%s has %d matching namespaces; preserve its namespace", toolType, name, len(matches))
		}
		if len(matches) == 1 && matches[0].Namespace != "" {
			item["namespace"] = raw(matches[0].Namespace)
			items[index], err = json.Marshal(item)
			if err != nil {
				return nil, err
			}
		}
	}
	return items, nil
}
