//go:build ignore

// Bounded, synthetic-only structured-output protocol evidence. No retries.
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
	"regexp"
	"strings"
	"time"
)

type account struct {
	Token string `json:"access_token"`
	ID    string `json:"account_id"`
}
type fixture struct {
	Accounts          []account `json:"accounts"`
	Version           string    `json:"tools_version"`
	ToolSchema        bool      `json:"tool_schema"`
	ToolSchemaOnce    bool      `json:"tool_schema_once"`
	ToolSchemaInvalid bool      `json:"tool_schema_invalid"`
}
type outcome struct {
	HTTP                int
	Complete            bool
	Answer              string
	Stopped             bool
	FunctionCount       int
	FunctionSchemaValid bool
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func uuid() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("random unavailable")
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	v := hex.EncodeToString(b[:])
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}
func emit(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }

var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

func safeText(s string, a account) string {
	s = strings.ReplaceAll(s, a.Token, "[REDACTED_TOKEN]")
	if a.ID != "" {
		s = strings.ReplaceAll(s, a.ID, "[REDACTED_ACCOUNT]")
	}
	s = emailPattern.ReplaceAllString(s, "[REDACTED_EMAIL]")
	if len(s) > 2048 {
		s = s[:2048] + "[TRUNCATED]"
	}
	return s
}
func errorFields(raw []byte, a account) map[string]any {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	if nested, ok := body["error"]; ok {
		var e map[string]json.RawMessage
		if json.Unmarshal(nested, &e) == nil {
			body = e
		}
	}
	result := map[string]any{}
	for _, k := range []string{"type", "code", "param", "message", "detail"} {
		var s string
		if json.Unmarshal(body[k], &s) == nil && s != "" {
			result[k] = safeText(s, a)
		}
	}
	return result
}
func schema(invalid bool) map[string]any {
	typ := "string"
	if invalid {
		typ = "bogus"
	}
	return map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": typ, "enum": []string{"SCHEMA_OK"}}}, "required": []string{"answer"}, "additionalProperties": false}
}
func textFormat(strict, invalid bool) map[string]any {
	return map[string]any{"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "probe", "strict": strict, "schema": schema(invalid)}}}
}
func legacyFormat(invalid bool) map[string]any {
	return map[string]any{"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "probe", "strict": true, "schema": schema(invalid)}}}
}

func toolFormat(invalid bool) map[string]any {
	tools := []any{map[string]any{"type": "function", "name": "emit_structured_output", "parameters": schema(invalid), "strict": true}}
	toolBytes, _ := json.Marshal(tools)
	return map[string]any{"input": []any{
		map[string]any{"type": "additional_tools", "id": "at_" + digest(append([]byte("codex-agent:bps-additional-tools:v1\x00"), toolBytes...))[:32], "role": "developer", "tools": tools},
		map[string]any{"type": "message", "role": "developer", "content": []any{map[string]string{"type": "input_text", "text": "Call emit_structured_output once with the final answer. Do not emit any text."}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "The answer must be PROMPT_VALUE."}}},
	}}
}

func summarizeFunction(raw json.RawMessage, a account) map[string]any {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	var kind, name, args string
	_ = json.Unmarshal(item["type"], &kind)
	if kind != "function_call" {
		return nil
	}
	_ = json.Unmarshal(item["name"], &name)
	isString := json.Unmarshal(item["arguments"], &args) == nil
	if !isString {
		args = string(item["arguments"])
	}
	var value map[string]any
	_ = json.Unmarshal([]byte(args), &value)
	return map[string]any{"type": kind, "name": safeText(name, a), "arguments_type_string": isString, "arguments": safeText(args, a), "arguments_valid_json": json.Valid([]byte(args)), "schema_enum_satisfied": value["answer"] == "SCHEMA_OK" && len(value) == 1, "prompt_value_satisfied": value["answer"] == "PROMPT_VALUE" && len(value) == 1}
}

func run(client *http.Client, f fixture, name string, extra map[string]any) outcome {
	a := f.Accounts[0]
	body := map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "stream": true, "store": false, "model_selection": "explicit", "input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Return a JSON object whose answer property equals PROMPT_VALUE. Do not include any other properties or markdown."}}}}, "metadata": map[string]string{"task_id": uuid(), "turn_id": uuid(), "bps_tools_version_id": f.Version, "agent_iteration": "1"}}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(raw))
	req.GetBody = nil
	setHeaders(req.Header, a)
	started := time.Now()
	res, err := client.Do(req)
	row := map[string]any{"case": name, "request_sha256": digest(raw), "request_bytes": len(raw), "requested_model": "gpt-6-astra", "requested_effort": "xhigh", "headers_ms": time.Since(started).Milliseconds(), "completed": false, "automatic_retries": 0, "synthetic_prompt": true}
	result := outcome{}
	if err != nil {
		row["transport_error_type"] = fmt.Sprintf("%T", err)
		result.Stopped = true
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		emit(row)
		return result
	}
	result.HTTP = res.StatusCode
	result.Stopped = res.StatusCode == 403 || res.StatusCode == 429
	row["http"] = res.StatusCode
	data, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	row["response_sha256"] = digest(data)
	row["response_bytes"] = len(data)
	row["read_complete"] = readErr == nil
	var output strings.Builder
	var functions []map[string]any
	var eventFunctions []map[string]any
	for _, line := range bytes.Split(data, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		eventBytes := bytes.TrimSpace(line[5:])
		var event map[string]json.RawMessage
		if json.Unmarshal(eventBytes, &event) != nil {
			continue
		}
		var kind string
		_ = json.Unmarshal(event["type"], &kind)
		if kind == "response.output_text.delta" {
			var delta string
			_ = json.Unmarshal(event["delta"], &delta)
			output.WriteString(delta)
		}
		if kind == "response.output_item.done" {
			if item := summarizeFunction(event["item"], a); item != nil {
				eventFunctions = append(eventFunctions, item)
			}
		}
		if kind == "response.completed" || kind == "response.failed" || kind == "error" {
			row["terminal"] = kind
			result.Complete = kind == "response.completed"
			row["completed"] = result.Complete
			var response map[string]json.RawMessage
			_ = json.Unmarshal(event["response"], &response)
			if v := response["usage"]; len(v) > 0 {
				row["usage"] = v
			}
			if v := response["model"]; len(v) > 0 {
				row["returned_model"] = v
			}
			var items []json.RawMessage
			_ = json.Unmarshal(response["output"], &items)
			for _, item := range items {
				if call := summarizeFunction(item, a); call != nil {
					functions = append(functions, call)
				}
			}
			if kind != "response.completed" {
				errBody := eventBytes
				if len(response) > 0 {
					errBody = event["response"]
				}
				row["error"] = errorFields(errBody, a)
			}
			if result.Complete && output.Len() == 0 {
				var items []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				}
				_ = json.Unmarshal(response["output"], &items)
				for _, item := range items {
					for _, part := range item.Content {
						output.WriteString(part.Text)
					}
				}
			}
		}
	}
	if res.StatusCode >= 400 {
		row["error"] = errorFields(data, a)
	}
	result.Answer = strings.TrimSpace(output.String())
	row["synthetic_answer"] = safeText(result.Answer, a)
	row["answer_valid_json"] = json.Valid([]byte(result.Answer))
	var answer map[string]any
	_ = json.Unmarshal([]byte(result.Answer), &answer)
	row["schema_enum_satisfied"] = answer["answer"] == "SCHEMA_OK" && len(answer) == 1
	row["prompt_value_satisfied"] = answer["answer"] == "PROMPT_VALUE" && len(answer) == 1
	if len(functions) == 0 {
		functions = eventFunctions
	}
	row["function_calls"] = functions
	result.FunctionCount = len(functions)
	result.FunctionSchemaValid = len(functions) == 1 && functions[0]["name"] == "emit_structured_output" && functions[0]["schema_enum_satisfied"] == true && functions[0]["arguments_type_string"] == true
	row["single_function_schema_valid"] = result.FunctionSchemaValid
	row["elapsed_ms"] = time.Since(started).Milliseconds()
	emit(row)
	if readErr != nil {
		result.Stopped = true
	}
	return result
}
func main() {
	var f fixture
	if json.NewDecoder(os.Stdin).Decode(&f) != nil || len(f.Accounts) == 0 {
		os.Exit(2)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	client := &http.Client{Transport: t, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if f.ToolSchema {
		if f.ToolSchemaInvalid {
			_ = run(client, f, "additional_tools_invalid_schema_type", toolFormat(true))
			return
		}
		valid := run(client, f, "additional_tools_strict_function", toolFormat(false))
		if f.ToolSchemaOnce {
			return
		}
		if valid.Stopped {
			return
		}
		if valid.Complete && valid.FunctionSchemaValid {
			invalid := run(client, f, "additional_tools_invalid_schema_type", toolFormat(true))
			if invalid.Stopped {
				return
			}
			emit(map[string]any{"kind": "strictness_assessment", "positive_function_output_matches_schema": true, "invalid_schema_rejected_before_success": invalid.HTTP >= 400 && !invalid.Complete, "strict_decoding_proven": false, "reason": "One positive output and invalid-schema rejection can establish validation but do not prove every generated value is constrained."})
		} else {
			emit(map[string]any{"kind": "strictness_assessment", "positive_function_output_matches_schema": valid.FunctionSchemaValid, "strict_decoding_proven": false, "invalid_schema_skipped": true})
		}
		return
	}
	control := run(client, f, "control_no_format", nil)
	if control.Stopped || !control.Complete {
		emit(map[string]any{"stopped": "control_unavailable"})
		return
	}
	strict := run(client, f, "text_json_schema_strict", textFormat(true, false))
	if strict.Stopped {
		return
	}
	loose := run(client, f, "text_json_schema_non_strict", textFormat(false, false))
	if loose.Stopped {
		return
	}
	if !strict.Complete && !loose.Complete {
		emit(map[string]any{"skipped": "text_json_object", "reason": "two_consecutive_text_format_failures_switch_method"})
		legacy := run(client, f, "legacy_json_schema_strict", legacyFormat(false))
		if legacy.Stopped {
			return
		}
		if legacy.Complete {
			_ = run(client, f, "legacy_json_schema_invalid_type", legacyFormat(true))
		} else {
			emit(map[string]any{"stopped": "both_structured_field_shapes_rejected", "generation_attempts": 4})
		}
		return
	}
	object := run(client, f, "text_json_object", map[string]any{"text": map[string]any{"format": map[string]string{"type": "json_object"}}})
	if object.Stopped {
		return
	}
	invalid := run(client, f, "text_json_schema_invalid_type", textFormat(true, true))
	if invalid.Stopped {
		return
	}
	_ = run(client, f, "final_control_no_format", nil)
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
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims, auth map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) == nil && json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil {
		var id string
		_ = json.Unmarshal(auth["chatgpt_account_user_id"], &id)
		h.Set("X-OpenAI-Account-User-Id", id)
	}
}
