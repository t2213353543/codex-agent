//go:build ignore

// At most two synthetic generations diagnose native large-integer schema fidelity.
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
	"sort"
	"strings"
	"time"

	"codex-agent/internal/bps"
)

type account struct {
	Token string `json:"access_token"`
	ID    string `json:"account_id"`
}
type fixture struct {
	Accounts []account `json:"accounts"`
	Version  string    `json:"tools_version"`
}

func raw(v any) json.RawMessage {
	b, e := json.Marshal(v)
	if e != nil {
		panic("fixture encoding failed")
	}
	return b
}
func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func emit(v any)             { _ = json.NewEncoder(os.Stdout).Encode(v) }
func uuid() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random unavailable")
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	v := hex.EncodeToString(b[:])
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}
func field(raw json.RawMessage, names ...string) json.RawMessage {
	for _, name := range names {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return nil
		}
		raw = m[name]
	}
	return raw
}
func numberEnum(def json.RawMessage) json.RawMessage {
	return field(def, "parameters", "properties", "data", "properties", "large", "enum")
}
func shape(raw json.RawMessage) any {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil && m != nil {
		result := map[string]any{}
		for key, v := range m {
			result[key] = shape(v)
		}
		return result
	}
	trim := bytes.TrimSpace(raw)
	if len(trim) == 0 {
		return "absent"
	}
	switch trim[0] {
	case '"':
		return "string"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}
func addUsage(row map[string]any, u json.RawMessage) {
	for _, name := range []string{"input_tokens", "output_tokens"} {
		var n int64
		if json.Unmarshal(field(u, name), &n) == nil {
			row[name] = n
		}
	}
	for _, pair := range []struct{ from, to string }{{"cached_tokens", "cache_read_tokens"}, {"cache_write_tokens", "cache_write_tokens"}} {
		var n int64
		known := json.Unmarshal(field(u, "input_tokens_details", pair.from), &n) == nil
		row[pair.to+"_known"] = known
		if known {
			row[pair.to] = n
		}
	}
}
func run(client *http.Client, f fixture, large string) (bool, bool) {
	marker := "numeric-marker-" + uuid()
	formatRaw := json.RawMessage(`{"format":{"type":"json_schema","name":"live_nested_answer","strict":true,"schema":{"type":"object","properties":{"marker":{"type":"string"},"data":{"type":"object","properties":{"answer":{"type":"string","enum":["SCHEMA_OK"]},"large":{"type":"integer","enum":[` + large + `]}},"required":["answer","large"],"additionalProperties":false}},"required":["marker","data"],"additionalProperties":false}}}`)
	parsed, e := bps.ParseStructuredFormat(formatRaw)
	if e != nil || parsed == nil {
		emit(map[string]any{"stage": "local_format", "failed": true})
		return false, true
	}
	inbound := raw(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]string{"effort": "xhigh"}, "stream": true, "tool_choice": "none", "tools": []any{}, "text": formatRaw, "input": []any{map[string]string{"role": "user", "content": "Remember this exact marker: " + marker + ". Produce an object with that marker and nested data containing answer SCHEMA_OK and integer large=" + large + ". Use the requested response format."}}})
	prepared, e := bps.Prepare(inbound, bps.Options{TaskID: uuid(), TurnID: uuid(), ToolsVersion: f.Version, SearchDisabled: true, StructuredOutput: true})
	if e != nil || prepared.Structured == nil || prepared.Structured.ToolName != parsed.ToolName {
		emit(map[string]any{"stage": "local_prepare", "failed": true})
		return false, true
	}
	payload := prepared.Body
	row := map[string]any{"case": "integer_" + large, "requested_integer": large, "request_sha256": digest(payload), "request_bytes": len(payload), "automatic_retries": 0, "requested_model": "gpt-6-astra", "requested_effort": "xhigh", "builder": "bps.Prepare", "formatter_name": parsed.ToolName, "request_schema_large_enum": numberEnum(parsed.ToolDefinition()), "completed": false}
	if len(field(payload, "tool_choice")) > 0 {
		row["unexpected_root_tool_choice"] = true
		emit(row)
		return false, true
	}
	req, _ := http.NewRequest(http.MethodPost, bps.BaseURL+"/responses", bytes.NewReader(payload))
	req.GetBody = nil
	setHeaders(req.Header, f.Accounts[0])
	start := time.Now()
	res, e := client.Do(req)
	row["headers_ms"] = time.Since(start).Milliseconds()
	if e != nil {
		row["transport_error_type"] = fmt.Sprintf("%T", e)
		emit(row)
		return false, true
	}
	row["http"] = res.StatusCode
	wire, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	row["elapsed_ms"] = time.Since(start).Milliseconds()
	row["response_sha256"] = digest(wire)
	row["response_bytes"] = len(wire)
	row["read_complete"] = readErr == nil
	if res.StatusCode != 200 || readErr != nil {
		emit(row)
		return false, true
	}
	var terminal json.RawMessage
	for _, line := range bytes.Split(wire, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event struct {
			Type     string
			Response json.RawMessage
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) == nil && (event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "error") {
			row["terminal"] = event.Type
			if event.Type == "response.completed" {
				row["completed"] = true
				terminal = event.Response
			}
		}
	}
	if terminal == nil {
		emit(row)
		return false, true
	}
	addUsage(row, field(terminal, "usage"))
	var definitions []json.RawMessage
	_ = json.Unmarshal(field(terminal, "tools"), &definitions)
	schemaChanged := false
	schemaSeen := false
	for _, def := range definitions {
		var name string
		_ = json.Unmarshal(field(def, "name"), &name)
		if name == parsed.ToolName {
			schemaSeen = true
			v := numberEnum(def)
			row["upstream_schema_large_enum"] = v
			schemaChanged = !bytes.Equal(bytes.TrimSpace(v), []byte("["+large+"]"))
			row["upstream_schema_large_enum_unchanged"] = !schemaChanged
		}
	}
	row["upstream_formatter_schema_present"] = schemaSeen
	var output []json.RawMessage
	_ = json.Unmarshal(field(terminal, "output"), &output)
	calls := 0
	numericCorrect := false
	fullCorrect := false
	for _, item := range output {
		var kind, name, args string
		_ = json.Unmarshal(field(item, "type"), &kind)
		if kind == "reasoning" {
			var cipher string
			_ = json.Unmarshal(field(item, "encrypted_content"), &cipher)
			row["encrypted_reasoning_present"] = cipher != ""
			continue
		}
		if kind != "function_call" {
			continue
		}
		_ = json.Unmarshal(field(item, "name"), &name)
		if name != parsed.ToolName {
			row["unexpected_function_name"] = true
			continue
		}
		calls++
		if json.Unmarshal(field(item, "arguments"), &args) != nil {
			row["arguments_type_string"] = false
			continue
		}
		row["arguments_type_string"] = true
		safe := strings.ReplaceAll(args, f.Accounts[0].Token, "[REDACTED]")
		if f.Accounts[0].ID != "" {
			safe = strings.ReplaceAll(safe, f.Accounts[0].ID, "[REDACTED]")
		}
		if len(safe) > 8192 {
			safe = safe[:8192]
		}
		row["synthetic_arguments"] = safe
		row["arguments_shape"] = shape([]byte(args))
		var obj map[string]json.RawMessage
		_ = json.Unmarshal([]byte(args), &obj)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		row["argument_root_fields"] = keys
		var returnedMarker, answer string
		_ = json.Unmarshal(field([]byte(args), "marker"), &returnedMarker)
		_ = json.Unmarshal(field([]byte(args), "data", "answer"), &answer)
		numericCorrect = bytes.Equal(bytes.TrimSpace(field([]byte(args), "data", "large")), []byte(large))
		row["large_exact_match"] = numericCorrect
		row["marker_match"] = returnedMarker == marker
		row["answer_match"] = answer == "SCHEMA_OK"
		_, validationErr := parsed.ValidateArguments(args)
		row["offline_real_schema_validator_passed"] = validationErr == nil
		if validationErr != nil {
			row["offline_validator_error"] = validationErr.Error()
		}
		fullCorrect = numericCorrect && returnedMarker == marker && answer == "SCHEMA_OK" && validationErr == nil
	}
	row["internal_function_count"], row["all_fields_and_validator_match"] = calls, calls == 1 && fullCorrect
	emit(row)
	return calls == 1 && (!numericCorrect || schemaChanged), false
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
	smaller, stop := run(client, f, "9007199254740993")
	if smaller && !stop {
		_, _ = run(client, f, "2147483647")
	} else {
		emit(map[string]any{"second_request_skipped": true, "reason": "no_numeric_mismatch_or_first_request_unavailable"})
	}
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
