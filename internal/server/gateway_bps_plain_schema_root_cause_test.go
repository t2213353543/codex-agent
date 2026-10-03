package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-agent/internal/bps"
)

func TestBPSPlaintextToolSchemaOwnerRecovery(t *testing.T) {
	for _, position := range []string{"tools", "additional_tools", "tool_search_output"} {
		t.Run(position, func(t *testing.T) {
			const definitions = `[{"type":"namespace","name":"documents","tools":[{"type":"function","name":"read_document","parameters":{"type":"object","properties":{"file_id":{"type":"string"},"encrypted_content":{"type":"string"}},"required":["file_id"]}}]}]`
			body := map[string]any{"model": "gpt-6-astra", "stream": false, "input": []any{map[string]string{"role": "user", "content": "Complete plaintext history"}}}
			if position == "tools" {
				body["tools"] = json.RawMessage(definitions)
			} else {
				body["input"] = []any{map[string]any{"type": position, "tools": json.RawMessage(definitions)}, map[string]string{"role": "user", "content": "Complete plaintext history"}}
			}
			raw, _ := json.Marshal(body)
			if _, err := bps.Prepare(raw, bps.Options{TaskID: "test", TurnID: "test", StructuredOutput: true}); err != nil {
				t.Fatal("valid plaintext schema was rejected before routing", err)
			}
			class := ""
			ctx := withBPSModeMigrationObserved(t.Context(), raw, &class)
			calls := 0
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.WriteString(w, bpsGatewayFrame("response.completed", "plain_schema_result", "[]"))
			})
			if _, err := p.sessionAccount(t.Context(), "admin", "schema_old_owner", "id:deleted:upstream:deleted:user:deleted:revision:deleted"); err != nil {
				t.Fatal(err)
			}
			req := proxyRequest(http.MethodPost, "/v1/responses", string(raw))
			req.Header.Set("Session-Id", "schema_old_owner")
			out := httptest.NewRecorder()
			p.gatewayBPS(out, req)
			t.Logf("position=%s replay_class=%s allowed=%t http=%d upstream_calls=%d", position, class, bpsAllowsModeMigration(ctx, inferencePowerPoint), out.Code, calls)
			if out.Code != http.StatusOK || calls != 1 {
				t.Fatalf("valid complete plaintext history cannot recover old owner: HTTP %d; classification %s", out.Code, class)
			}
		})
	}
}

func TestBPSPlaintextSchemaExemptionPreservesOpaqueGuards(t *testing.T) {
	for _, raw := range []string{
		`{"input":[{"type":"reasoning","encrypted_content":"actual-cipher"}]}`,
		`{"input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"owned-file"}]}]}`,
		`{"input":[{"type":"function_call","encrypted_function_args":["actual-cipher"]}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"actual-cipher"}]}`,
		`{"input":[{"type":"additional_tools","tools":[],"encrypted_content":"actual-cipher"}]}`,
		`{"input":[{"type":"tool_search_output","tools":[],"extra":{"file_id":"owned-file"}}]}`,
		`{"input":[{"type":"message","role":"user","extra":{"type":"additional_tools","tools":[{"type":"function","name":"nested","parameters":{"file_id":{"type":"string"}}}]}}]}`,
		`{"input":[{"type":"unknown","tools":[{"type":"function","name":"nested","parameters":{"file_id":{"type":"string"}}}]}]}`,
		`{"input":[{"type":"additional_tools","tools":{"file_id":"owned-file"}}]}`,
		`{"input":[{"type":"additional_tools","tools":[{"type":"unknown","name":"nested","parameters":{"file_id":{"type":"string"}}}]}]}`,
		`{"input":[{"type":"additional_tools","tools":[{"type":"function","parameters":{"file_id":{"type":"string"}}}]}]}`,
		`{"previous_response_id":"old-response","input":[{"type":"additional_tools","tools":[]}]}`,
	} {
		class := ""
		ctx := withBPSModeMigrationObserved(t.Context(), []byte(raw), &class)
		if bpsAllowsModeMigration(ctx, inferencePowerPoint) {
			t.Fatalf("declaration exemption bypassed actual owner state: %s", raw)
		}
	}
	// Ordinary JSON arguments are plaintext strings; their contents are not
	// parsed as protocol state, exactly as before the declaration fix.
	raw := []byte(`{"input":[{"type":"function_call","name":"echo","arguments":"{\"file_id\":\"literal-client-value\",\"encrypted_content\":\"literal\"}"}]}`)
	if !bpsAllowsModeMigration(withBPSModeMigration(t.Context(), raw), inferencePowerPoint) {
		t.Fatal("plaintext argument string changed migration semantics")
	}
}
