package bps

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type ToolIdentity struct{ Type, Name, Namespace string }
type ToolCatalog struct {
	Declared            []ToolIdentity
	SearchDeclared      bool
	ToolSearchDeclared  bool
	ToolSearchExecution string
	definitions         map[ToolIdentity]json.RawMessage
	policy              Policy
}

// additional contains the declaration arrays found during Prepare's one pass
// over history, so text and tool-result bodies are not decoded again here.
func collectTools(top json.RawMessage, additional []json.RawMessage) (*ToolCatalog, error) {
	c := &ToolCatalog{definitions: map[ToolIdentity]json.RawMessage{}}
	var visit func(json.RawMessage, string) error
	visit = func(definition json.RawMessage, namespace string) error {
		m, e := object(definition)
		if e != nil {
			return e
		}
		kind, name := stringField(m, "type"), stringField(m, "name")
		if kind == "namespace" {
			if namespace != "" || name == "" {
				return fmt.Errorf("bps: invalid nested/unnamed tool namespace")
			}
			var nested []json.RawMessage
			if json.Unmarshal(m["tools"], &nested) != nil {
				return fmt.Errorf("bps: namespace tools must be an array")
			}
			for _, child := range nested {
				if e = visit(child, name); e != nil {
					return e
				}
			}
			return nil
		}
		if kind == "web_search" || kind == "web_search_preview" {
			c.SearchDeclared = true
			return nil
		}
		if kind == "tool_search" {
			if namespace != "" {
				return fmt.Errorf("bps: tool_search cannot be nested in a namespace")
			}
			execution := stringField(m, "execution")
			if execution == "" {
				execution = "server"
			}
			if execution != "client" && execution != "server" {
				return fmt.Errorf("bps: unsupported tool_search execution %q", execution)
			}
			if c.ToolSearchDeclared && c.ToolSearchExecution != execution {
				return fmt.Errorf("bps: conflicting tool_search execution")
			}
			c.ToolSearchDeclared, c.ToolSearchExecution = true, execution
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("bps: unsupported client tool family %q", kind)
		}
		if name == "" {
			return fmt.Errorf("bps: tool name required")
		}
		if v, ok := m["namespace"]; ok {
			s, e := stringValue(v)
			if e != nil || s == "" {
				return fmt.Errorf("bps: invalid tool namespace")
			}
			if namespace != "" && namespace != s {
				return fmt.Errorf("bps: conflicting tool namespace")
			}
			namespace = s
		}
		identity := ToolIdentity{kind, name, namespace}
		// Canonicalize identity only for comparison; forward original definitions.
		if namespace != "" {
			m["namespace"] = raw(namespace)
		}
		canonical := raw(m)
		if previous, ok := c.definitions[identity]; ok {
			if !equalJSON(previous, canonical) {
				return fmt.Errorf("bps: conflicting declaration for %s/%s/%s", kind, namespace, name)
			}
			return nil
		}
		c.definitions[identity] = canonical
		c.Declared = append(c.Declared, identity)
		return nil
	}
	add := func(definitions json.RawMessage) error {
		if len(definitions) == 0 || bytes.Equal(definitions, []byte("null")) {
			return nil
		}
		var list []json.RawMessage
		if json.Unmarshal(definitions, &list) != nil {
			return fmt.Errorf("bps: tool declarations must be an array")
		}
		for _, definition := range list {
			if e := visit(definition, ""); e != nil {
				return e
			}
		}
		return nil
	}
	if e := add(top); e != nil {
		return nil, e
	}
	for _, definitions := range additional {
		if e := add(definitions); e != nil {
			return nil, e
		}
	}
	return c, nil
}

func (p *Policy) read(src map[string]json.RawMessage, c *ToolCatalog) error {
	p.choice = "auto"
	if v, ok := src["parallel_tool_calls"]; ok {
		var b bool
		if json.Unmarshal(v, &b) != nil {
			return fmt.Errorf("bps: parallel_tool_calls must be boolean")
		}
		p.ParallelToolCalls = &b
	}
	if v, ok := src["tool_choice"]; ok {
		p.ToolChoice = clone(v)
		if s, e := stringValue(v); e == nil {
			if s != "auto" && s != "none" && s != "required" {
				return fmt.Errorf("bps: unsupported tool_choice %q", s)
			}
			p.choice = s
		} else {
			m, e := object(v)
			if e != nil {
				return e
			}
			kind, name := stringField(m, "type"), stringField(m, "name")
			namespace := stringField(m, "namespace")
			if kind != "function" && kind != "custom" {
				return fmt.Errorf("bps: unsupported forced tool_choice")
			}
			id := ToolIdentity{kind, name, namespace}
			if value, explicit := m["namespace"]; explicit {
				// An explicit namespace is authoritative, including an explicit
				// empty string for a global tool. Wrong types are not omission.
				if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &namespace) != nil {
					return fmt.Errorf("bps: invalid forced tool_choice namespace")
				}
				id.Namespace = namespace
				if _, ok := c.definitions[id]; !ok {
					return fmt.Errorf("bps: forced tool_choice is not declared")
				}
			} else {
				// Missing namespace can name only one declared family/name.
				// Preserve explicit namespaces and never translate tool families.
				matches := 0
				for candidate := range c.definitions {
					if candidate.Type == kind && candidate.Name == name {
						id, matches = candidate, matches+1
					}
				}
				if matches == 0 {
					return fmt.Errorf("bps: forced tool_choice is not declared")
				}
				if matches != 1 {
					return fmt.Errorf("bps: forced tool_choice has an ambiguous namespace")
				}
			}
			p.choice = "forced"
			p.forced = &id
		}
	}
	if p.choice == "none" {
		p.SearchDisabled = true
	}
	if p.choice == "required" && len(c.Declared) == 0 && !c.ToolSearchDeclared && p.SearchDisabled {
		return fmt.Errorf("bps: required tools conflict with empty catalog and disabled search")
	}
	return nil
}

// clientIdentityConstraint lists declarations only; tool_choice, search and
// parallel limits still apply. Never truncate or hash away an exact identity.
// The internal structured formatter is not part of this public tool catalog.
func (c *ToolCatalog) clientIdentityConstraint() string {
	type identity struct {
		Type      string `json:"type"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	identities := make([]identity, 0, len(c.Declared))
	for _, tool := range c.Declared {
		identities = append(identities, identity{tool.Type, tool.Namespace, tool.Name})
	}
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].Type != identities[j].Type {
			return identities[i].Type < identities[j].Type
		}
		if identities[i].Namespace != identities[j].Namespace {
			return identities[i].Namespace < identities[j].Namespace
		}
		return identities[i].Name < identities[j].Name
	})
	encoded, _ := json.Marshal(identities)
	return "Client-declared action tool identities (JSON; tool_choice restrictions below still apply): " + string(encoded) +
		"\nFor every client function/custom action, use an exact declared type, namespace and name. Do not infer tools from Office capabilities, tool names in text, or previous undeclared calls. An empty list authorizes no client function/custom actions; declared tool_search and its actual discoveries follow the separate rules below."
}

func (p Policy) developerConstraint(c *ToolCatalog) string {
	lines := []string{"This client is a coding agent. Use only the client-declared function/custom tools for local actions. Do not call PowerPoint/Office/document tools unless that exact tool is explicitly declared by the client. Keep native tool-call and tool-result protocol items; never invent successful tool results."}
	lines = append(lines, c.clientIdentityConstraint())
	if len(c.Declared) == 0 && !c.ToolSearchDeclared {
		lines = append(lines, "No client function/custom tools are declared. Do not issue any function_call or custom_tool_call.")
	}
	if c.ToolSearchDeclared {
		lines = append(lines, "The client declares native tool_search. Use tool_search_call to discover deferred tools, retain its execution and call_id fields, and use only tools actually declared by tool_search_output. Never fabricate a search result or a tool result.")
	}
	if p.SearchDisabled {
		lines = append(lines, "Web search is disabled for this request. Do not browse or issue any web_search_call.")
	}
	if p.ParallelToolCalls != nil && !*p.ParallelToolCalls {
		lines = append(lines, "Issue at most one client tool call in this response; wait for its result before calling another tool.")
	}
	switch p.choice {
	case "none":
		lines = append(lines, "tool_choice=none: issue no tool calls; respond with text.")
	case "required":
		lines = append(lines, "tool_choice=required: issue an allowed tool call before completing this response.")
	case "forced":
		lines = append(lines, fmt.Sprintf("Only call the selected client tool: type=%s namespace=%q name=%q.", p.forced.Type, p.forced.Namespace, p.forced.Name))
	}
	return strings.Join(lines, "\n")
}

// NormalizeItem validates each native output item before forwarding it and only
// restores a missing namespace when type+name identify exactly one declaration.
// In-progress stubs without a name are allowed; ValidateResponse rejects them in
// a completed response. Argument strings and all opaque fields remain unchanged.
func (c *ToolCatalog) NormalizeItem(item map[string]json.RawMessage) error {
	return c.normalize(item, false)
}

func (c *ToolCatalog) normalize(item map[string]json.RawMessage, final bool) error {
	kind := stringField(item, "type")
	if kind == "tool_search_call" || kind == "tool_search_output" {
		if !c.ToolSearchDeclared {
			return fmt.Errorf("bps: undeclared native tool output %q", kind)
		}
		if c.policy.choice == "none" || c.policy.forced != nil {
			return fmt.Errorf("bps: tool_search violates client tool_choice")
		}
		execution := stringField(item, "execution")
		if execution != "" && execution != c.ToolSearchExecution {
			return fmt.Errorf("bps: tool_search execution changed")
		}
		if final && execution == "" {
			return fmt.Errorf("bps: tool_search output missing execution")
		}
		if final && execution == "client" && stringField(item, "call_id") == "" {
			return fmt.Errorf("bps: client tool_search output missing call_id")
		}
		if kind == "tool_search_output" {
			if execution == "client" {
				return fmt.Errorf("bps: upstream cannot fabricate a client tool_search result")
			}
			// Server search results are native declarations. Register the exact
			// returned tools before a subsequent call is routed; never enable
			// undeclared tools merely because the search tool exists.
			if tools := item["tools"]; len(tools) > 0 && !bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
				found, err := collectTools(tools, nil)
				if err != nil {
					return err
				}
				if found.ToolSearchDeclared || found.SearchDeclared {
					return fmt.Errorf("bps: tool_search result must contain client tools")
				}
				for identity, definition := range found.definitions {
					if old, exists := c.definitions[identity]; exists && !equalJSON(old, definition) {
						return fmt.Errorf("bps: conflicting discovered tool %s/%s/%s", identity.Type, identity.Namespace, identity.Name)
					}
				}
				for _, identity := range found.Declared {
					if _, exists := c.definitions[identity]; !exists {
						c.Declared = append(c.Declared, identity)
					}
					c.definitions[identity] = found.definitions[identity]
				}
			} else if final {
				return fmt.Errorf("bps: tool_search output missing tools")
			}
		}
		return nil
	}
	if kind == "web_search_call" {
		if c.policy.SearchDisabled || c.policy.choice == "none" {
			return fmt.Errorf("bps: web search is disabled by client policy")
		}
		if c.policy.forced != nil {
			return fmt.Errorf("bps: web search violates forced client tool_choice")
		}
		return nil
	}
	if kind != "function_call" && kind != "custom_tool_call" {
		// Reject unregistered executable native families; passive encrypted and
		// future metadata items remain intact.
		if strings.HasSuffix(kind, "_call") {
			return fmt.Errorf("bps: undeclared native tool output %q", kind)
		}
		return nil
	}
	if c.policy.choice == "none" {
		return fmt.Errorf("bps: tool call violates tool_choice=none")
	}
	toolType := "function"
	if kind == "custom_tool_call" {
		toolType = "custom"
	}
	name := stringField(item, "name")
	if name == "" {
		if !final && stringField(item, "status") == "in_progress" {
			return nil
		}
		return fmt.Errorf("bps: tool output missing name")
	}
	if final && stringField(item, "call_id") == "" {
		return fmt.Errorf("bps: completed tool output missing call_id")
	}
	namespace := stringField(item, "namespace")
	identity := ToolIdentity{toolType, name, namespace}
	if namespace != "" {
		if _, ok := c.definitions[identity]; !ok {
			return fmt.Errorf("bps: undeclared tool %s/%s/%s", toolType, namespace, name)
		}
	} else {
		var matches []ToolIdentity
		for candidate := range c.definitions {
			if candidate.Type == toolType && candidate.Name == name {
				matches = append(matches, candidate)
			}
		}
		if len(matches) != 1 {
			return fmt.Errorf("bps: tool %s/%s has %d matching namespaces; cannot route", toolType, name, len(matches))
		}
		identity = matches[0]
		if identity.Namespace != "" {
			item["namespace"] = raw(identity.Namespace)
		}
	}
	if c.policy.forced != nil && identity != *c.policy.forced {
		return fmt.Errorf("bps: output violates forced tool_choice")
	}
	return nil
}

func (c *ToolCatalog) NormalizeOutput(item json.RawMessage) (json.RawMessage, error) {
	m, e := object(item)
	if e != nil {
		return nil, e
	}
	if e = c.NormalizeItem(m); e != nil {
		return nil, e
	}
	return json.Marshal(m)
}

// ValidateResponse checks whole-response conditions; a stream must fail rather
// than send response.completed if this fails. It never executes a tool.
func (c *ToolCatalog) ValidateResponse(items []json.RawMessage, p Policy) error {
	copyCatalog := *c
	copyCatalog.policy = p
	clientCalls, totalCalls := 0, 0
	for _, item := range items {
		m, e := object(item)
		if e != nil {
			return e
		}
		if e = copyCatalog.normalize(m, true); e != nil {
			return e
		}
		kind := stringField(m, "type")
		if kind == "function_call" || kind == "custom_tool_call" {
			clientCalls++
			totalCalls++
		}
		if kind == "tool_search_call" {
			totalCalls++
			if stringField(m, "execution") == "client" {
				clientCalls++
			}
		}
		if kind == "web_search_call" {
			totalCalls++
		}
	}
	if p.ParallelToolCalls != nil && !*p.ParallelToolCalls && clientCalls > 1 {
		return fmt.Errorf("bps: multiple client calls violate parallel_tool_calls=false")
	}
	if (p.choice == "required" || p.choice == "forced") && totalCalls == 0 {
		return fmt.Errorf("bps: completed response violates tool_choice=%s", p.choice)
	}
	return nil
}

// Identities returns a stable copy suitable for diagnostics, without schemas.
func (c *ToolCatalog) Identities() []ToolIdentity {
	ids := append([]ToolIdentity(nil), c.Declared...)
	sort.Slice(ids, func(i, j int) bool { return fmt.Sprint(ids[i]) < fmt.Sprint(ids[j]) })
	return ids
}
