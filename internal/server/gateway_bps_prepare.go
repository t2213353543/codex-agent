package server

import (
	"bytes"
	"encoding/json"
	"fmt"

	"codex-agent/internal/bps"
)

// bpsBindPreparedAccount reuses the fully validated protocol request after
// account admission. Only task_id and scoped cache/client identities depend on
// the account. Body comes from bps.Prepare and scoped comes from the validated
// original request plus prepareGatewayAccountPromptCache; the span scanner must
// never be used here with arbitrary, unvalidated JSON.
// Keep TurnID and generated additional_tools IDs stable for the entire request.
func bpsBindPreparedAccount(prepared *bps.Request, scoped []byte, taskID string) error {
	body := prepared.Body
	fields := gatewayValidatedJSONFieldSpans(body, "metadata", "prompt_cache_key")
	metadata := fields[0]
	if metadata.end == 0 {
		return fmt.Errorf("prepared BPS metadata is missing")
	}
	task := gatewayValidatedJSONFieldSpans(body[metadata.start:metadata.end], "task_id", "")[0]
	if task.end == 0 {
		return fmt.Errorf("prepared BPS task ID is missing")
	}
	taskRaw, _ := json.Marshal(taskID)
	edits := []gatewayJSONEdit{{gatewayJSONFieldSpan{metadata.start + task.start, metadata.start + task.end}, taskRaw}}
	identities := gatewayValidatedJSONFieldSpans(scoped, "prompt_cache_key", "client_metadata")
	if cache := identities[0]; cache.end != 0 {
		value := scoped[cache.start:cache.end]
		if target := fields[1]; target.end != 0 && !bytes.Equal(body[target.start:target.end], value) {
			edits = append(edits, gatewayJSONEdit{target, value})
		}
		if _, ok := prepared.Policy.ClientControls["prompt_cache_key"]; ok {
			prepared.Policy.ClientControls["prompt_cache_key"] = append(json.RawMessage(nil), value...)
		}
	}
	if metadata := identities[1]; metadata.end != 0 {
		prepared.Policy.ClientControls["client_metadata"] = append(json.RawMessage(nil), scoped[metadata.start:metadata.end]...)
	}
	prepared.Body = gatewayApplyJSONEdits(body, edits, gatewayJSONEditedLength(body, edits))
	prepared.TaskID = taskID
	return nil
}
