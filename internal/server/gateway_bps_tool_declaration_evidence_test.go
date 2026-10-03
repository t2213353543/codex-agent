package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBPSGatewayToolDeclarationEvidenceWriterAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, tools, choice, tool string
		declared, other, success  bool
	}{
		{"implicit_planner", `[]`, `"auto"`, `update_plan`, false, false, false},
		{"other_family", `[{"type":"custom","name":"PRIVATE_CLIENT_TOOL","format":{"type":"text"}}]`, `"auto"`, `PRIVATE_CLIENT_TOOL`, false, true, false},
		{"declared_but_none", `[{"type":"namespace","name":"PRIVATE_NS","tools":[{"type":"function","name":"PRIVATE_CLIENT_TOOL"}]}]`, `"none"`, `PRIVATE_CLIENT_TOOL`, true, false, false},
		{"declared_success", `[{"type":"namespace","name":"PRIVATE_NS","tools":[{"type":"function","name":"PRIVATE_CLIENT_TOOL"}]}]`, `"auto"`, `PRIVATE_CLIENT_TOOL`, true, false, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var sent []byte
				p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
					sent, _ = io.ReadAll(r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("X-OpenAI-Internal-Basispoints-Agent-Tools-Sha256", strings.Repeat("a", 64))
					w.Header().Set("X-OpenAI-Internal-Basispoints-Agent-Prompt-Sha256", strings.Repeat("b", 64))
					call := rootGatewayToolPolicyCall(tc.tool, "", "probe")
					payload, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_evidence", "status": "completed", "model": "gpt-6-astra", "output": []any{call}, "usage": map[string]int{"input_tokens": 10, "output_tokens": 3, "total_tokens": 13}}})
					fmt.Fprintf(w, "data: %s\n\n", payload)
				})
				source := fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":"PRIVATE_BODY","tools":%s,"tool_choice":%s}`, stream, tc.tools, tc.choice)
				w := httptest.NewRecorder()
				rootGatewayToolPolicyRun(p, w, proxyRequest(http.MethodPost, "/v1/responses", source))
				record, ok := p.Metrics.LastFor("test")
				if !ok || len(sent) == 0 {
					t.Fatal("no request evidence")
				}
				o := record.ToolPolicy
				if o == nil {
					t.Fatal("no observation")
				}
				encoded, _ := json.Marshal(o)
				for _, secret := range []string{"PRIVATE_BODY", "PRIVATE_CLIENT_TOOL", "PRIVATE_NS", "PRIVATE_ARGUMENT"} {
					if bytes.Contains(encoded, []byte(secret)) {
						t.Fatal("private payload retained")
					}
				}
				if tc.success {
					if record.Status != "ok" || o.RejectionPhase != "" || o.RejectedNameSHA256 != "" || o.InboundFamilyNameDeclared != nil || o.SerializedInitialFamilyNameDeclared != nil || o.ResolvedToolsSHA256 != "" || o.ResolvedPromptSHA256 != "" {
						t.Fatal("successful request gained rejection metadata")
					}
					return
				}
				if record.Status != "error" || o.InboundFamilyNameDeclared == nil || *o.InboundFamilyNameDeclared != tc.declared || o.SerializedInitialFamilyNameDeclared == nil || *o.SerializedInitialFamilyNameDeclared != tc.declared || o.RejectedNameOtherFamily == nil || *o.RejectedNameOtherFamily != tc.other || len(o.RejectedNameSHA256) != 64 || o.ResolvedToolsSHA256 != strings.Repeat("a", 64) || o.ResolvedPromptSHA256 != strings.Repeat("b", 64) {
					t.Fatalf("wrong evidence: %s", encoded)
				}
				if tc.tool == "update_plan" && o.RejectedKnownBuiltin != "update_plan" {
					t.Fatal("known builtin missing")
				}
				// Outbound declarations were independently captured at the local HTTP writer.
				if bytes.Contains(sent, []byte(`"type":"additional_tools"`)) != (tc.tools != `[]`) {
					t.Fatal("writer evidence fixture mismatch")
				}
				if bytes.Contains(w.Body.Bytes(), []byte(`"type":"response.completed"`)) {
					t.Fatal("invalid tool was forwarded")
				}
				t.Logf("DECLARATION_EVIDENCE %s", encoded)
			})
		}
	}
}
