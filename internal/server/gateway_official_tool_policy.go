package server

import (
	"encoding/json"
	"errors"
)

// The client owns these tools. Feature gates alone are insufficient: 0.155
// also enables utilities and collaboration from its model catalog. This policy
// applies only to the Responses client-tool bridge, never hosted tasks.
func officialClientToolFeaturePolicy(config map[string]any) {
	for _, feature := range []string{"goals", "current_time_reminder", "sleep_tool", "token_budget", "context_management", "request_permissions_tool", "deferred_executor"} {
		config["features."+feature] = false
	}
}

var officialClientToolModelFields = []string{"tool_mode", "apply_patch_tool_type", "multi_agent_version", "experimental_supported_tools"}

func officialClientToolModelPolicy(model map[string]json.RawMessage) error {
	model["tool_mode"] = json.RawMessage(`"direct"`)
	model["apply_patch_tool_type"] = json.RawMessage(`null`)
	model["multi_agent_version"] = json.RawMessage(`"disabled"`)
	if raw, ok := model["experimental_supported_tools"]; ok && string(raw) != "null" {
		var tools []string
		if json.Unmarshal(raw, &tools) != nil {
			return errors.New("official model utility tool metadata is invalid")
		}
		kept := make([]string, 0, len(tools))
		for _, tool := range tools {
			switch tool {
			case "clock", "request_user_input_async", "send_user_message_async", "send_message_to_user_async", "test_sync_tool":
			default:
				kept = append(kept, tool)
			}
		}
		model["experimental_supported_tools"], _ = json.Marshal(kept)
	}
	return nil
}

// Official DynamicToolSpec only accepts function schemas. For a custom tool,
// unwrap the advertised {input:string} transport when present. Some upstreams
// emit the original free-form input in the arguments string instead; preserve
// that exact string, without inventing, repairing or extracting code fields.
// An explicit malformed wrapper remains an error rather than silently changing
// the meaning of a supplied input field.
func officialCustomToolInput(raw json.RawMessage) (string, error) {
	var arguments string
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &arguments) != nil {
		return "", errors.New("custom tool arguments must contain an exact string")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &fields) == nil && fields != nil {
		if input, exists := fields["input"]; exists {
			var value string
			if len(fields) != 1 || string(input) == "null" || json.Unmarshal(input, &value) != nil {
				return "", errors.New("custom tool wrapper must contain only a string input")
			}
			return value, nil
		}
	}
	return arguments, nil
}
