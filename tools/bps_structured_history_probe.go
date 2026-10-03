//go:build ignore

// Four synthetic upstream calls, without retries or persistent credential data.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type account struct {
	Token string `json:"access_token"`
	ID    string `json:"account_id"`
}
type fixture struct {
	Accounts []account `json:"accounts"`
	Version  string    `json:"tools_version"`
}
type result struct {
	Output                       []json.RawMessage
	Row                          map[string]any
	Complete, Encrypted, Compact bool
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random unavailable")
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	v := hex.EncodeToString(b[:])
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}
func raw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("synthetic fixture serialization failed")
	}
	return b
}
func emit(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
func text(role, value string) json.RawMessage {
	return raw(map[string]any{"type": "message", "role": role, "content": []any{map[string]string{"type": "input_text", "text": value}}})
}
func metadata(f fixture, task string, turn int) map[string]string {
	return map[string]string{"task_id": task, "turn_id": uuid(), "bps_tools_version_id": f.Version, "agent_iteration": fmt.Sprint(turn)}
}

func declaration() json.RawMessage {
	schema := map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string", "enum": []string{"SCHEMA_OK"}}, "marker": map[string]string{"type": "string"}}, "required": []string{"answer", "marker"}, "additionalProperties": false}
	tools := raw([]any{map[string]any{"type": "function", "name": "emit_structured_output", "strict": true, "parameters": schema}})
	return raw(map[string]any{"type": "additional_tools", "role": "developer", "id": "at_" + digest(append([]byte("codex-agent:bps-additional-tools:v1\x00"), tools...))[:32], "tools": tools})
}
func generation(f fixture, task string, turn int, history []json.RawMessage) []byte {
	input := []json.RawMessage{declaration(), text("developer", "Call emit_structured_output exactly once with the final answer and remembered marker. Emit no ordinary text. The marker must come only from the conversation, never invent one.")}
	input = append(input, history...)
	return raw(map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "stream": true, "store": false, "model_selection": "explicit", "input": input, "metadata": metadata(f, task, turn)})
}
func addUsage(row map[string]any, usage json.RawMessage) {
	var value map[string]json.RawMessage
	if json.Unmarshal(usage, &value) != nil {
		return
	}
	for _, name := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		var n int64
		if json.Unmarshal(value[name], &n) == nil {
			row[name] = n
		}
	}
	var detail map[string]json.RawMessage
	_ = json.Unmarshal(value["input_tokens_details"], &detail)
	for _, field := range []struct{ source, target string }{{"cached_tokens", "cache_read_tokens"}, {"cache_write_tokens", "cache_write_tokens"}} {
		var n int64
		known := json.Unmarshal(detail[field.source], &n) == nil
		row[field.target+"_known"] = known
		if known {
			row[field.target] = n
		}
	}
}
func inspectOutput(r *result) {
	for _, item := range r.Output {
		var v struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		}
		if json.Unmarshal(item, &v) == nil {
			r.Encrypted = r.Encrypted || (v.Type == "reasoning" && v.Encrypted != "")
			r.Compact = r.Compact || (v.Type == "compaction" && v.Encrypted != "")
		}
	}
	r.Row["encrypted_reasoning_present"] = r.Encrypted
	r.Row["real_compaction_present"] = r.Compact
}
func call(client *http.Client, f fixture, name, path string, payload []byte) result {
	r := result{Row: map[string]any{"case": name, "request_sha256": digest(payload), "request_bytes": len(payload), "automatic_retries": 0, "synthetic_prompt": true, "completed": false, "requested_model": "gpt-6-astra"}}
	if path == "/responses" {
		r.Row["requested_effort"] = "xhigh"
	}
	req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api"+path, bytes.NewReader(payload))
	req.GetBody = nil
	setHeaders(req.Header, f.Accounts[0])
	if path != "/responses" {
		req.Header.Set("Accept", "application/json")
	}
	start := time.Now()
	res, err := client.Do(req)
	r.Row["headers_ms"] = time.Since(start).Milliseconds()
	if err != nil {
		r.Row["transport_error_type"] = fmt.Sprintf("%T", err)
		r.Row["elapsed_ms"] = time.Since(start).Milliseconds()
		return r
	}
	r.Row["http"] = res.StatusCode
	data, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	r.Row["elapsed_ms"] = time.Since(start).Milliseconds()
	r.Row["response_sha256"] = digest(data)
	r.Row["response_bytes"] = len(data)
	r.Row["read_complete"] = readErr == nil
	if res.StatusCode == 403 || res.StatusCode == 429 {
		r.Row["stop_policy_or_rate_limit"] = true
		return r
	}
	if res.StatusCode != 200 || readErr != nil {
		return r
	}
	if path == "/responses/compact" {
		var reply struct {
			Object string            `json:"object"`
			Output []json.RawMessage `json:"output"`
			Usage  json.RawMessage   `json:"usage"`
		}
		if json.Unmarshal(data, &reply) == nil {
			r.Output = reply.Output
			inspectOutput(&r)
			r.Complete = reply.Object == "response.compaction" && r.Compact
			r.Row["terminal"] = reply.Object
			addUsage(r.Row, reply.Usage)
		}
	} else {
		for _, line := range bytes.Split(data, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var event struct {
				Type     string `json:"type"`
				Response struct {
					Output []json.RawMessage `json:"output"`
					Usage  json.RawMessage   `json:"usage"`
					Model  string            `json:"model"`
				} `json:"response"`
			}
			if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
				continue
			}
			if event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "error" {
				r.Row["terminal"] = event.Type
				r.Complete = event.Type == "response.completed"
				r.Output = event.Response.Output
				addUsage(r.Row, event.Response.Usage)
				if event.Response.Model == "gpt-6-astra" {
					r.Row["returned_model"] = event.Response.Model
				}
			}
		}
		inspectOutput(&r)
	}
	r.Row["completed"] = r.Complete
	return r
}

// Keep reasoning bytes exactly as delivered. Only the observed internal call is
// projected; no function result or reconstructed encrypted reasoning is made.
func project(r *result, marker string) ([]json.RawMessage, bool) {
	var projected []json.RawMessage
	calls := 0
	matched := false
	for _, item := range r.Output {
		var fields map[string]json.RawMessage
		if json.Unmarshal(item, &fields) != nil {
			r.Row["projection_failure"] = "invalid_output_item"
			return nil, false
		}
		var kind string
		_ = json.Unmarshal(fields["type"], &kind)
		if kind == "reasoning" {
			projected = append(projected, item)
			continue
		}
		if kind != "function_call" {
			r.Row["projection_failure"] = "unexpected_output_item_family"
			return nil, false
		}
		var name, args, namespace string
		if json.Unmarshal(fields["name"], &name) != nil || name != "emit_structured_output" || json.Unmarshal(fields["arguments"], &args) != nil {
			r.Row["projection_failure"] = "unexpected_internal_call"
			return nil, false
		}
		if len(fields["namespace"]) > 0 {
			_ = json.Unmarshal(fields["namespace"], &namespace)
		}
		r.Row["function_namespace_present"] = namespace != ""
		if namespace != "" && namespace != "functions" {
			r.Row["projection_failure"] = "unexpected_call_namespace"
			return nil, false
		}
		var v map[string]json.RawMessage
		valid := json.Unmarshal([]byte(args), &v) == nil && len(v) == 2
		var answer, got string
		valid = valid && json.Unmarshal(v["answer"], &answer) == nil && json.Unmarshal(v["marker"], &got) == nil
		r.Row["arguments_valid_json"] = json.Valid([]byte(args))
		r.Row["answer_matches"] = valid && answer == "SCHEMA_OK"
		r.Row["marker_matches"] = valid && got == marker
		matched = valid && answer == "SCHEMA_OK" && got == marker
		calls++
		// The content is the exact decoded arguments string, including whitespace.
		projected = append(projected, raw(map[string]any{"type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": args, "annotations": []any{}}}}))
	}
	r.Row["internal_function_count"] = calls
	r.Row["reasoning_raw_preserved"] = true
	r.Row["projected_from_actual_arguments"] = calls == 1
	return projected, calls == 1 && matched
}
func finish(boundary string, calls int, passed, encrypted bool) {
	emit(map[string]any{"kind": "history_assessment", "boundary": boundary, "upstream_requests": calls, "max_upstream_requests": 4, "passed": passed, "real_encrypted_reasoning_seen": encrypted, "encrypted_reasoning_compatibility_proven": passed && encrypted, "dual_history_required_for_this_case": false, "dual_history_need_conclusion_available": passed, "customer_history_replayed": false, "production_mutations": false})
}
func main() {
	var f fixture
	if json.NewDecoder(os.Stdin).Decode(&f) != nil || len(f.Accounts) == 0 || f.Version == "" {
		os.Exit(2)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	client := &http.Client{Transport: transport, Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	task, marker := uuid(), "synthetic-marker-"+uuid()
	history := []json.RawMessage{text("user", "Remember this exact synthetic marker for later turns: "+marker+". Return the marker and answer SCHEMA_OK.")}
	one := call(client, f, "strict_function_initial", "/responses", generation(f, task, 1, history))
	publicOne, ok := project(&one, marker)
	emit(one.Row)
	if !one.Complete || !ok {
		finish("initial_generation_or_projection", 1, false, one.Encrypted)
		return
	}
	history = append(history, publicOne...)
	history = append(history, text("user", "Return the same marker and answer from your previous assistant JSON response. Do not invent or change either value."))
	two := call(client, f, "public_history_with_raw_reasoning", "/responses", generation(f, task, 2, history))
	publicTwo, ok := project(&two, marker)
	emit(two.Row)
	encrypted := one.Encrypted || two.Encrypted
	if !two.Complete || !ok {
		finish("projected_full_history_continuation", 2, false, encrypted)
		return
	}
	history = append(history, publicTwo...)
	compactBody := raw(map[string]any{"model": "gpt-6-astra", "input": history, "metadata": metadata(f, task, 3)})
	three := call(client, f, "public_history_compact", "/responses/compact", compactBody)
	emit(three.Row)
	if !three.Complete {
		finish("public_history_compaction", 3, false, encrypted)
		return
	}
	resumed := append([]json.RawMessage(nil), three.Output...)
	resumed = append(resumed, text("user", "From the compacted conversation, return the exact marker and answer you previously reported. Do not invent either value."))
	four := call(client, f, "compacted_public_history_resume", "/responses", generation(f, task, 4, resumed))
	_, ok = project(&four, marker)
	emit(four.Row)
	finish("compacted_public_history_resume", 4, four.Complete && ok, encrypted)
}
func setHeaders(h http.Header, a account) {
	h.Set("Authorization", "Bearer "+a.Token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("ChatGPT-Account-ID", a.ID)
	h.Set("X-OpenAI-Account-Id", a.ID)
	h.Set("X-Basispoints-Auth-Mode", "chatgpt")
	for k, v := range map[string]string{"Product": "basispoints-powerpoint-plugin", "Platform": "powerpoint", "Agent-Profile": "powerpoint", "Editor": "powerpoint", "Host": "office", "Runtime": "web", "Platform-Class": "web"} {
		h.Set("X-Openai-Internal-Basispoints-Client-"+k, v)
	}
	parts := strings.Split(a.Token, ".")
	if len(parts) != 3 {
		return
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return
	}
	var claims, auth map[string]json.RawMessage
	if json.Unmarshal(b, &claims) == nil && json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil {
		var id string
		_ = json.Unmarshal(auth["chatgpt_account_user_id"], &id)
		if !strings.ContainsAny(id, "\r\n") && len(id) < 512 {
			h.Set("X-OpenAI-Account-User-Id", id)
		}
	}
}
