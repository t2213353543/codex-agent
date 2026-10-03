package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

//go:embed appserver_source_templates_0155.json
var appServerSourceTemplates0155 []byte

type appServerContextProfile struct {
	ModelMetadataRaw                           json.RawMessage
	SandboxMode, ApprovalPolicy, NetworkAccess string
	CollaborationMode                          string
	IncludeCollaborationMode                   bool
	CollaborationOverride                      *string
	CustomModelCatalog, UpdatePlanEnabled      bool
	MultiAgentVersion                          string
	RootRoleOverride, MultiAgentModeOverride   *string
	SelectedEffort                             string
	MaxConcurrentAgents                        int
	WaitAgentEnabled, ExposeModelOverrides     bool
	CWD, Shell, CurrentDate, Timezone          string
	WorkspaceRoots                             []string
}

type appServerContextPart struct{ Kind, Text string }
type appServerContextFragment struct {
	Role  string
	Parts []appServerContextPart
}
type appServerContextIdentity struct {
	ID, TurnID string
	CreateTime json.Number
}

type appServerContextModel struct {
	Slug              string `json:"slug"`
	MultiAgentVersion string `json:"multi_agent_version"`
	ModelMessages     struct {
		Permissions *struct {
			ReadOnly *string `json:"read_only"`
		} `json:"permissions"`
		Approvals *struct {
			Never *string `json:"never"`
		} `json:"approvals"`
		CollaborationModes *struct{ Default, Plan *string } `json:"collaboration_modes"`
		MultiAgent         *struct {
			Role *struct{ Root *string } `json:"role"`
			Mode *struct {
				Explicit, Proactive *string
				HintText            *string `json:"hint_text"`
			} `json:"mode"`
		} `json:"multi_agent"`
	} `json:"model_messages"`
}

// buildAppServerInitialContext ports the captured initial managed/read-only,
// never-approval profile. Unsupported profiles fail explicitly until their
// source/capture adapters exist. Catalog text wins over pinned source fallback;
// an explicit empty catalog/config string suppresses fallback, as Rust does.
func buildAppServerInitialContext(p appServerContextProfile) ([]appServerContextFragment, error) {
	if p.SandboxMode != "read-only" || p.ApprovalPolicy != "never" {
		return nil, errors.New("App Server context profile requires read-only sandbox and never approvals")
	}
	if p.NetworkAccess != "restricted" && p.NetworkAccess != "enabled" {
		return nil, errors.New("App Server context requires explicit network access")
	}
	if p.CWD == "" {
		return nil, errors.New("App Server context requires caller environment cwd")
	}
	var model appServerContextModel
	if err := json.Unmarshal(p.ModelMetadataRaw, &model); err != nil || model.Slug == "" {
		return nil, errors.New("invalid App Server context model metadata")
	}
	var source struct {
		Templates map[string]string `json:"templates"`
	}
	if err := json.Unmarshal(appServerSourceTemplates0155, &source); err != nil {
		return nil, err
	}
	t := source.Templates
	sandbox := t["permission_read_only"]
	if v := model.ModelMessages.Permissions; v != nil && v.ReadOnly != nil {
		sandbox = *v.ReadOnly
	}
	sandbox = strings.ReplaceAll(sandbox, "{{ network_access }}", p.NetworkAccess)
	approval := t["approval_never"]
	if v := model.ModelMessages.Approvals; v != nil && v.Never != nil {
		approval = *v.Never
	}
	var permission strings.Builder
	appendSection := func(s string) {
		if !strings.HasSuffix(permission.String(), "\n") {
			permission.WriteByte('\n')
		}
		permission.WriteString(s)
	}
	if sandbox != "" {
		appendSection(sandbox)
	}
	appendSection(approval)
	if !strings.HasSuffix(permission.String(), "\n") {
		permission.WriteByte('\n')
	}
	first := appServerContextFragment{Role: "developer", Parts: []appServerContextPart{{Kind: "permissions.instructions", Text: "<permissions instructions>" + permission.String() + "</permissions instructions>"}}}
	if p.IncludeCollaborationMode {
		if p.CollaborationMode != "default" && p.CollaborationMode != "plan" {
			return nil, errors.New("unsupported App Server collaboration mode")
		}
		var catalog *string
		if modes := model.ModelMessages.CollaborationModes; modes != nil {
			if p.CollaborationMode == "plan" {
				catalog = modes.Plan
			} else {
				catalog = modes.Default
			}
		}
		selected := catalog
		if selected == nil && p.CollaborationOverride != nil && *p.CollaborationOverride != "" {
			selected = p.CollaborationOverride
		}
		if selected != nil {
			text := *selected
			builtin := catalog != nil && !p.CustomModelCatalog || catalog == nil && (text == t["collaboration_default"] || text == t["collaboration_plan"])
			if !p.UpdatePlanEnabled && builtin {
				text = appServerWithoutUpdatePlan(text)
			}
			first.Parts = append(first.Parts, appServerContextPart{Kind: "collaboration_mode.instructions", Text: "<collaboration_mode>" + text + "</collaboration_mode>"})
		}
	}
	fragments := []appServerContextFragment{first}
	version := p.MultiAgentVersion
	if version == "" {
		version = model.MultiAgentVersion
	}
	if version == "v2" {
		role, marked := t["multiagent_root"], false
		var catalog *string
		if m := model.ModelMessages.MultiAgent; m != nil && m.Role != nil {
			catalog = m.Role.Root
		}
		if p.RootRoleOverride != nil {
			role = *p.RootRoleOverride
		} else {
			if catalog != nil {
				role = *catalog
				marked = true
			}
			if role != "" {
				if !p.UpdatePlanEnabled && !p.CustomModelCatalog {
					role = appServerWithoutUpdatePlan(role)
				}
				if p.MaxConcurrentAgents <= 0 {
					return nil, errors.New("App Server multi-agent context requires explicit concurrency")
				}
				role += "\n" + t["multiagent_shared"] + "\n"
				if p.WaitAgentEnabled {
					role += t["multiagent_wait"] + "\n\n"
				}
				role += fmt.Sprintf("There are %d available concurrency slots, meaning that up to %d agents can be active at once, including you.", p.MaxConcurrentAgents, p.MaxConcurrentAgents)
				if p.ExposeModelOverrides {
					role += "\n\n" + t["multiagent_model_overrides"]
				}
			}
		}
		if role != "" {
			if marked {
				role = "<multi_agent_role>" + role + "</multi_agent_role>"
			}
			fragments = append(fragments, appServerContextFragment{Role: "developer", Parts: []appServerContextPart{{Kind: "multi_agent.role_instructions", Text: role}}})
		}
		var mode *string
		if p.MultiAgentModeOverride != nil {
			mode = p.MultiAgentModeOverride
		} else if m := model.ModelMessages.MultiAgent; m != nil && m.Mode != nil {
			mode = m.Mode.HintText
			if mode == nil {
				if p.SelectedEffort == "ultra" {
					mode = m.Mode.Proactive
				} else {
					mode = m.Mode.Explicit
				}
			}
		}
		modeText := t["multiagent_explicit"]
		if p.SelectedEffort == "ultra" {
			modeText = t["multiagent_proactive"]
		}
		if mode != nil {
			modeText = *mode
		}
		if modeText != "" {
			fragments = append(fragments, appServerContextFragment{Role: "developer", Parts: []appServerContextPart{{Kind: "multi_agent.mode_instructions", Text: "<multi_agent_mode>" + modeText + "</multi_agent_mode>"}}})
		}
	} else if version != "" && version != "v1" {
		return nil, errors.New("unsupported App Server multi-agent version")
	}
	var env strings.Builder
	env.WriteString("<environment_context>\n")
	for _, field := range [][2]string{{"cwd", p.CWD}, {"shell", p.Shell}, {"current_date", p.CurrentDate}, {"timezone", p.Timezone}} {
		if field[1] != "" {
			fmt.Fprintf(&env, "  <%s>%s</%s>\n", field[0], appServerXMLEscape(field[1]), field[0])
		}
	}
	env.WriteString("  <filesystem>")
	if len(p.WorkspaceRoots) > 0 {
		env.WriteString("<workspace_roots>")
		for _, root := range p.WorkspaceRoots {
			env.WriteString("<root>" + appServerXMLEscape(root) + "</root>")
		}
		env.WriteString("</workspace_roots>")
	}
	env.WriteString("<permission_profile type=\"managed\"><file_system type=\"restricted\"><entry access=\"read\"><special>:root</special></entry></file_system></permission_profile></filesystem>\n</environment_context>")
	fragments = append(fragments, appServerContextFragment{Role: "user", Parts: []appServerContextPart{{Kind: "environments.environment_context", Text: env.String()}}})
	return fragments, nil
}

// materializeAppServerContext adds only fragments not already retained with the
// same role, kind and complete text (or exact text when kinds were omitted by
// the writer). It returns additions; the caller decides
// the insertion boundary, preserving its existing history and output IDs.
func materializeAppServerContext(fragments []appServerContextFragment, history []json.RawMessage, identities []appServerContextIdentity) ([]json.RawMessage, error) {
	retained := map[string]bool{}
	retainedWithoutKinds := map[string]bool{}
	for _, raw := range history {
		var item struct {
			Role     string
			Content  []struct{ Type, Text string }
			Metadata struct {
				Kinds []string `json:"content_item_kinds"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		for i, part := range item.Content {
			if part.Type == "input_text" && i < len(item.Metadata.Kinds) {
				retained[item.Role+"\x00"+item.Metadata.Kinds[i]+"\x00"+part.Text] = true
			} else if part.Type == "input_text" {
				retainedWithoutKinds[item.Role+"\x00"+part.Text] = true
			}
		}
	}
	var missing []appServerContextFragment
	for _, fragment := range fragments {
		next := appServerContextFragment{Role: fragment.Role}
		for _, part := range fragment.Parts {
			if !retained[fragment.Role+"\x00"+part.Kind+"\x00"+part.Text] && !retainedWithoutKinds[fragment.Role+"\x00"+part.Text] {
				next.Parts = append(next.Parts, part)
			}
		}
		if len(next.Parts) > 0 {
			missing = append(missing, next)
		}
	}
	if len(missing) != len(identities) {
		return nil, errors.New("App Server context needs one explicit identity per new message")
	}
	out := make([]json.RawMessage, 0, len(missing))
	for i, fragment := range missing {
		identity := identities[i]
		if !strings.HasPrefix(identity.ID, "msg_") || len(identity.ID) <= 4 || identity.TurnID == "" || identity.CreateTime == "" {
			return nil, errors.New("invalid App Server context message identity")
		}
		parts := make([]map[string]string, 0, len(fragment.Parts))
		kinds := make([]string, 0, len(fragment.Parts))
		for _, part := range fragment.Parts {
			parts = append(parts, map[string]string{"type": "input_text", "text": part.Text})
			kinds = append(kinds, part.Kind)
		}
		raw, err := marshalGatewayRequestJSON(map[string]any{"type": "message", "id": identity.ID, "role": fragment.Role, "content": parts, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": identity.TurnID, "create_time": identity.CreateTime, "content_item_kinds": kinds}})
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

func appServerXMLEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(value)
}

// Exact port of core/src/context/update_plan_instructions.rs, applied only to
// Codex-owned templates, never arbitrary caller instructions.
func appServerWithoutUpdatePlan(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var out strings.Builder
	for i := 0; i < len(lines); {
		line := strings.TrimRight(lines[i], " \t\r\n")
		if line == "## Planning" || line == "## `update_plan`" || line == "## Plan tool" || line == "## Plan Mode vs update_plan tool" {
			end := i + 1
			for end < len(lines) && !strings.HasPrefix(lines[end], "# ") && !strings.HasPrefix(lines[end], "## ") {
				end++
			}
			checklist := line != "## Planning"
			for _, part := range lines[i:end] {
				if strings.HasPrefix(part, "You have access to an `update_plan` tool") || strings.HasPrefix(part, "When `update_plan` is available, follow this section") {
					checklist = true
				}
			}
			if checklist {
				i = end
				continue
			}
		}
		if line == "Progress visibility:" && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "If update_plan is available") {
			i += 2
			if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			continue
		}
		if strings.HasPrefix(line, "- Use the plan tool ") || strings.HasPrefix(line, "- If you create a checklist or task list,") {
			i++
			for i < len(lines) && (strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")) {
				i++
			}
			continue
		}
		out.WriteString(lines[i])
		i++
	}
	return out.String()
}
