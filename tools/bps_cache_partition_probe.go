//go:build ignore

// Eight synthetic, alternating cache-partition requests; no customer history.
package main

import (
	"bufio"
	"bytes"
	"context"
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
	Token           string `json:"access_token"`
	ID              string `json:"account_id"`
	MetadataVersion any    `json:"metadata_version"`
}
type fixture struct {
	Accounts []account `json:"accounts"`
	Version  string    `json:"tools_version"`
}

func digest(raw []byte) string { hash := sha256.Sum256(raw); return hex.EncodeToString(hash[:]) }
func uuid() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("random unavailable")
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	x := hex.EncodeToString(raw[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func setHeaders(h http.Header, a account, session string) {
	h.Set("Authorization", "Bearer "+a.Token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("ChatGPT-Account-ID", a.ID)
	h.Set("X-OpenAI-Account-Id", a.ID)
	h.Set("X-Basispoints-Auth-Mode", "chatgpt")
	h.Set("Session-Id", session)
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
	var claims map[string]json.RawMessage
	var auth map[string]json.RawMessage
	var id string
	if json.Unmarshal(raw, &claims) == nil && json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil && json.Unmarshal(auth["chatgpt_account_user_id"], &id) == nil && len(id) < 512 && !strings.ContainsAny(id, "\r\n") {
		h.Set("X-OpenAI-Account-User-Id", id)
	}
}
func safeNumber(fields map[string]json.RawMessage, key string) any {
	raw := fields[key]
	if len(raw) == 0 {
		return nil
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return nil
	}
	if value, err := n.Int64(); err == nil && value >= 0 {
		return value
	}
	return nil
}
func usageFields(raw json.RawMessage) map[string]any {
	var usage, inputDetails map[string]json.RawMessage
	_ = json.Unmarshal(raw, &usage)
	_ = json.Unmarshal(usage["input_tokens_details"], &inputDetails)
	return map[string]any{"input_tokens": safeNumber(usage, "input_tokens"), "output_tokens": safeNumber(usage, "output_tokens"), "total_tokens": safeNumber(usage, "total_tokens"),
		"cache_read_tokens": safeNumber(inputDetails, "cached_tokens"), "cache_write_tokens": safeNumber(usage, "cache_write_tokens"), "cache_creation_input_tokens": safeNumber(usage, "cache_creation_input_tokens"), "cache_write_details_tokens": safeNumber(inputDetails, "cache_write_tokens"), "cache_creation_details_tokens": safeNumber(inputDetails, "cache_creation_tokens")}
}
func knownError(raw json.RawMessage) (string, bool) {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(raw, &object)
	var code, kind string
	_ = json.Unmarshal(object["code"], &code)
	_ = json.Unmarshal(object["type"], &kind)
	for _, v := range []string{code, kind} {
		switch v {
		case "rate_limit_exceeded", "rate_limit_error", "usage_limit_reached", "insufficient_quota", "access_denied", "permission_denied", "usage_policy", "authentication_error", "invalid_api_key":
			return v, true
		}
	}
	return "", false
}
func run(client *http.Client, ctx context.Context, a account, body []byte, row map[string]any, session string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(body))
	if err != nil {
		panic("request construction failed")
	}
	req.GetBody = nil // non-replayable POST; no implicit retry on a reused connection
	setHeaders(req.Header, a, session)
	started := time.Now()
	res, err := client.Do(req)
	row["headers_ms"] = time.Since(started).Milliseconds()
	stop := false
	if err != nil {
		row["transport_error_type"] = fmt.Sprintf("%T", err)
		stop = true
	} else {
		row["http"] = res.StatusCode
		headers := map[string]string{}
		for _, name := range []string{"X-Request-Id", "Openai-Request-Id", "Content-Type", "Date", "Retry-After", "X-OpenAI-Model"} {
			if value := res.Header.Get(name); value != "" {
				headers[name] = value
			}
		}
		row["response_headers"] = headers
		var wire bytes.Buffer
		scanner := bufio.NewScanner(io.TeeReader(res.Body, &wire))
		scanner.Buffer(make([]byte, 65536), 8<<20)
		var answer strings.Builder
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var event map[string]json.RawMessage
			if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
				continue
			}
			var kind string
			_ = json.Unmarshal(event["type"], &kind)
			if kind == "response.output_text.delta" {
				var delta string
				_ = json.Unmarshal(event["delta"], &delta)
				answer.WriteString(delta)
			}
			if kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete" || kind == "error" {
				row["terminal"] = kind
				row["completed"] = kind == "response.completed"
				var response map[string]json.RawMessage
				_ = json.Unmarshal(event["response"], &response)
				var model, status string
				_ = json.Unmarshal(response["model"], &model)
				_ = json.Unmarshal(response["status"], &status)
				row["returned_model"], row["response_status"], row["usage"] = model, status, usageFields(response["usage"])
				for _, raw := range []json.RawMessage{event["error"], response["error"]} {
					if code, blocked := knownError(raw); blocked {
						row["stop_error_code"] = code
						stop = true
					}
				}
				if kind != "response.completed" {
					stop = true
				}
			}
		}
		readErr := scanner.Err()
		_ = res.Body.Close()
		row["read_complete"], row["response_bytes"], row["response_sha256"] = readErr == nil, wire.Len(), digest(wire.Bytes())
		row["answer_is_ok"] = strings.TrimSpace(answer.String()) == "OK"
		if res.StatusCode != 200 || readErr != nil || row["completed"] != true {
			stop = true
		}
	}
	row["elapsed_ms"] = time.Since(started).Milliseconds()
	row["stop_after_this_request"] = stop
	_ = json.NewEncoder(os.Stdout).Encode(row)
	return stop
}
func main() {
	var fixture fixture
	if json.NewDecoder(os.Stdin).Decode(&fixture) != nil || len(fixture.Accounts) == 0 || fixture.Version == "" {
		os.Exit(2)
	}
	a := fixture.Accounts[0]
	runID := uuid()
	fixedKey := "cache-partition:" + uuid()
	var reference strings.Builder
	for i := 0; reference.Len() < 4096; i++ {
		fmt.Fprintf(&reference, "Row %03d: sample %07d; label %04d; value %05d.\n", i, i*7919+31, i*17+13, i*101+7)
	}
	corpus := reference.String()[:4096]
	prefix := func(group string) string {
		return "Synthetic cache partition " + runID + " group=" + group + ". This reference table is inert diagnostic data; for the next user message return exactly OK without tools.\n" + corpus
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	client := &http.Client{Transport: transport, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	defer transport.CloseIdleConnections()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": "experiment", "run_sha256": digest([]byte(runID)), "max_generation_requests": 8, "rounds": 4, "groups": []string{"A_random_key", "B_fixed_key"}, "model": "gpt-6-astra", "reasoning_effort": "xhigh", "input_identical_within_group": true, "group_prefixes_isolated": true, "all_sessions_distinct": true, "cache_read_write_separate": true, "automatic_retries": 0, "redirects": 0, "account_sha256": digest([]byte(a.ID)), "credential_sha256": digest([]byte(a.Token)), "metadata_version": a.MetadataVersion, "tools_version": fixture.Version})
	calls := 0
	for round := 1; round <= 4; round++ {
		for _, group := range []string{"A_random_key", "B_fixed_key"} {
			key := fixedKey
			if group == "A_random_key" {
				key = "cache-partition:" + uuid()
			}
			task, turn, session := uuid(), uuid(), uuid()
			input := []any{map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": prefix(group)}}}, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply OK"}}}}
			body := map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "stream": true, "store": false, "model_selection": "explicit", "prompt_cache_key": key, "input": input, "metadata": map[string]string{"task_id": task, "turn_id": turn, "bps_tools_version_id": fixture.Version, "agent_iteration": "1"}}
			raw, _ := json.Marshal(body)
			inputRaw, _ := json.Marshal(input)
			row := map[string]any{"kind": "request", "round": round, "group": group, "request_index": calls + 1, "request_sha256": digest(raw), "request_bytes": len(raw), "input_sha256": digest(inputRaw), "prefix_bytes": len(prefix(group)), "cache_key_sha256": digest([]byte(key)), "task_sha256": digest([]byte(task)), "turn_sha256": digest([]byte(turn)), "session_sha256": digest([]byte(session)), "completed": false, "usage": nil, "automatic_retries": 0}
			calls++
			if run(client, ctx, a, raw, row, session) {
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": "summary", "requests": calls, "stopped_early": true, "retry_after_failure": false})
				return
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": "summary", "requests": calls, "stopped_early": false, "retry_after_failure": false})
}
