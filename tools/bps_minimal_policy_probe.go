//go:build ignore

// Bounded server-only diagnosis; credentials enter stdin and are never written.
package main

import (
	"bufio"
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
	Alias           string `json:"alias"`
	Token           string `json:"access_token"`
	ID              string `json:"account_id"`
	MetadataVersion any    `json:"metadata_version"`
}
type fixture struct {
	Accounts     []account `json:"accounts"`
	ToolsVersion string    `json:"tools_version"`
	Mode         string    `json:"mode"`
	BodyBase64   string    `json:"body_base64"`
}

func digest(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func uuid() string {
	var x [16]byte
	if _, e := rand.Read(x[:]); e != nil {
		panic("random unavailable")
	}
	x[6] = x[6]&15 | 64
	x[8] = x[8]&63 | 128
	s := hex.EncodeToString(x[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func userID(token string) string {
	p := strings.Split(token, ".")
	if len(p) != 3 {
		return ""
	}
	b, e := base64.RawURLEncoding.DecodeString(p[1])
	if e != nil {
		return ""
	}
	var c map[string]json.RawMessage
	var auth map[string]json.RawMessage
	var id string
	if json.Unmarshal(b, &c) == nil && json.Unmarshal(c["https://api.openai.com/auth"], &auth) == nil {
		_ = json.Unmarshal(auth["chatgpt_account_user_id"], &id)
	}
	if strings.ContainsAny(id, "\r\n") || len(id) >= 512 {
		return ""
	}
	return id
}
func safeScalar(v any) any {
	if v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok || len(s) > 100 || strings.ContainsAny(s, " \r\n") {
		return "non_scalar"
	}
	return s
}
func classify(data []byte, row map[string]any) {
	var obj map[string]any
	if json.Unmarshal(data, &obj) != nil {
		return
	}
	if detail, ok := obj["error"].(map[string]any); ok {
		row["error_type"] = safeScalar(detail["type"])
		row["error_code"] = safeScalar(detail["code"])
		m, _ := detail["message"].(string)
		row["error_message_sha256"] = digest([]byte(m))
		row["usage_policy_message"] = m == "403: This request was blocked by our usage policy." || m == "This request was blocked by our usage policy."
	}
	kind, _ := obj["type"].(string)
	if response, ok := obj["response"].(map[string]any); ok {
		if kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete" {
			row["terminal"] = kind
			row["returned_model"] = response["model"]
			row["usage"] = response["usage"]
			b, _ := json.Marshal(response)
			classify(b, row)
		}
	}
}
func execute(client *http.Client, a account, body []byte, platform string) map[string]any {
	row := map[string]any{"account_alias": a.Alias, "metadata_version": a.MetadataVersion, "case": "official_minimal", "platform_class": platform, "request_body_sha256": digest(body), "request_body_bytes": len(body), "completed": false, "terminal": nil, "usage": nil}
	row["access_token_sha256"] = digest([]byte(a.Token))
	req, _ := http.NewRequest("POST", "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(body))
	h := req.Header
	h.Set("Authorization", "Bearer "+a.Token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("X-Basispoints-Auth-Mode", "chatgpt")
	for k, v := range map[string]string{"Product": "basispoints-powerpoint-plugin", "Platform": "powerpoint", "Agent-Profile": "powerpoint", "Editor": "powerpoint", "Host": "office", "Runtime": "web", "Platform-Class": platform} {
		h.Set("X-Openai-Internal-Basispoints-Client-"+k, v)
	}
	if a.ID != "" {
		h.Set("ChatGPT-Account-ID", a.ID)
		h.Set("X-OpenAI-Account-Id", a.ID)
	}
	if v := userID(a.Token); v != "" {
		h.Set("X-OpenAI-Account-User-Id", v)
	}
	headerJSON, _ := json.Marshal(h)
	row["request_headers_sha256"] = digest(headerJSON)
	start := time.Now()
	res, e := client.Do(req)
	row["header_ms"] = time.Since(start).Milliseconds()
	if e != nil {
		row["transport_error_type"] = fmt.Sprintf("%T", e)
		row["transport_error_sha256"] = digest([]byte(e.Error()))
		return row
	}
	defer res.Body.Close()
	row["http"] = res.StatusCode
	row["http_protocol"] = res.Proto
	for _, k := range []string{"X-Request-Id", "CF-Ray", "Content-Type", "Retry-After"} {
		row[strings.ToLower(k)] = res.Header.Get(k)
	}
	data, e := io.ReadAll(res.Body)
	row["elapsed_ms"] = time.Since(start).Milliseconds()
	row["response_bytes"] = len(data)
	row["response_sha256"] = digest(data)
	row["body_read_ok"] = e == nil
	classify(data, row)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			classify(line[6:], row)
		}
	}
	row["completed"] = row["terminal"] == "response.completed"
	return row
}
func main() {
	var f fixture
	if json.NewDecoder(os.Stdin).Decode(&f) != nil || !strings.HasPrefix(f.ToolsVersion, "tools-powerpoint-") || (f.Mode == "egress_once" && len(f.Accounts) != 1) || (f.Mode != "egress_once" && len(f.Accounts) != 2) {
		panic("invalid server-only fixture")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = transport.MaxIdleConns
	transport.ResponseHeaderTimeout = 180 * time.Second
	transport.DisableCompression = true
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: 180 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if f.Mode == "egress_once" {
		body, err := base64.StdEncoding.DecodeString(f.BodyBase64)
		if err != nil || !json.Valid(body) {
			panic("invalid fixed synthetic body")
		}
		row := execute(client, f.Accounts[0], body, "OfficeOnline")
		row["case"] = "fixed_body_egress_control"
		_ = json.NewEncoder(os.Stdout).Encode(row)
		return
	}
	var bodies [][]byte
	var rows []map[string]any
	for _, a := range f.Accounts {
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply exactly OK."}}}}, "model": "gpt-6-astra", "reasoning_effort": "xhigh", "stream": true, "store": false, "model_selection": "explicit", "metadata": map[string]string{"task_id": uuid(), "turn_id": uuid(), "bps_tools_version_id": f.ToolsVersion, "agent_iteration": "1"}})
		bodies = append(bodies, body)
		row := execute(client, a, body, "web")
		rows = append(rows, row)
		_ = json.NewEncoder(os.Stdout).Encode(row)
	}
	if rows[0]["usage_policy_message"] == true && rows[1]["usage_policy_message"] == true {
		row := execute(client, f.Accounts[0], bodies[0], "OfficeOnline")
		row["case"] = "same_body_platform_class_only"
		_ = json.NewEncoder(os.Stdout).Encode(row)
	}
}
