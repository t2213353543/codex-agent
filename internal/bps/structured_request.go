package bps

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Match only the function registered for this request; a namespaced client
// function with the same name is a different tool and remains public.
func IsStructuredFormattingItem(format *StructuredFormat, item map[string]json.RawMessage) bool {
	return format != nil && stringField(item, "type") == "function_call" &&
		stringField(item, "name") == format.ToolName && stringField(item, "namespace") == ""
}

func (p Policy) structuredDeveloperConstraint(c *ToolCatalog, format *StructuredFormat) string {
	lines := []string{"This client is a coding agent. For local actions, use only client-declared function/custom tools. Do not call Office/document tools unless that exact tool is declared by the client. Preserve native calls and results. Never invent successful tool results."}
	lines = append(lines, c.clientIdentityConstraint())
	if len(c.Declared) == 0 && !c.ToolSearchDeclared {
		lines = append(lines, "No client action tools are declared; do not issue any client action calls.")
	}
	if c.ToolSearchDeclared {
		lines = append(lines, "Native tool_search may discover deferred client tools. Retain its execution and call_id and use only tools declared by tool_search_output. Do not fabricate discovery results.")
	}
	if p.SearchDisabled {
		lines = append(lines, "Web search is disabled; do not browse or issue web_search_call.")
	}
	if p.ParallelToolCalls != nil && !*p.ParallelToolCalls {
		lines = append(lines, "Issue at most one client action call in a response and wait for its result before another call.")
	}
	switch p.choice {
	case "none":
		lines = append(lines, "tool_choice=none: do not call client action tools or search. The separate formatting function below is permitted to serialize your final answer.")
	case "required":
		lines = append(lines, "tool_choice=required: call a client-declared action tool in this response and stop to await its result. Do not call the formatting function in this response; formatting does not satisfy the required client action.")
	case "forced":
		lines = append(lines, fmt.Sprintf("Call only the selected client action tool in this response: type=%s namespace=%q name=%q. Stop to await its result. Do not call the formatting function in this response.", p.forced.Type, p.forced.Namespace, p.forced.Name))
	}
	lines = append(lines, fmt.Sprintf("The internal function %s is an answer serializer, not a client action. When ready to give the final answer, call it exactly once with the complete answer as its JSON arguments. Follow its supplied JSON schema. Do not combine it with client action calls or assistant answer text in the same response. Native web search or server-executed tool search permitted by the client policy may finish before the final formatter in the same response; retain their completed native transcript. An unfinished search or client-executed tool search is not a completed result. If a client action is needed first, call only the client tool and wait for its result. Do not invent a tool result for the serializer. A genuine refusal must remain a refusal.", format.ToolName))
	return strings.Join(lines, "\n")
}
