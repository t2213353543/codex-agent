package bps

import "encoding/json"

// ToolPolicyItemSnapshot is transient, never attached to a RequestRecord. It
// borrows only the three identity scalars so successful passive/tool items do
// not scan the catalog. It does not retain an item, catalog, payload or schema.
// Existing NormalizeItem replaces map fields without modifying these bytes.
type ToolPolicyItemSnapshot struct {
	typeValue, nameValue, namespaceValue json.RawMessage
	itemKnown                            bool
}

func CaptureToolPolicyItem(item map[string]json.RawMessage) ToolPolicyItemSnapshot {
	if item == nil {
		return ToolPolicyItemSnapshot{}
	}
	return ToolPolicyItemSnapshot{typeValue: item["type"], nameValue: item["name"], namespaceValue: item["namespace"], itemKnown: true}
}

// Resolve is called only after this exact item's NormalizeItem or parallel
// policy check fails, before any later catalog operation. A failed
// NormalizeItem cannot change declarations: discovery validates all incoming
// definitions/conflicts before committing. An action-call parallel check also
// cannot mutate the catalog. The pre-normalize namespace is still necessary
// because forced tool_choice may fail after restoring a unique namespace.
// Do not use this API around ValidateResponse: its shallow catalog copy may
// publish earlier discoveries before a later response item fails. Capture the
// existing eager nil-item snapshot before that whole-response call instead.
func (s ToolPolicyItemSnapshot) Resolve(r *Request) ToolPolicyRejection {
	if !s.itemKnown {
		return CaptureToolPolicyRejection(r, nil)
	}
	return CaptureToolPolicyRejection(r, map[string]json.RawMessage{
		"type": s.typeValue, "name": s.nameValue, "namespace": s.namespaceValue,
	})
}
