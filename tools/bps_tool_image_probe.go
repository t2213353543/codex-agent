//go:build ignore

// Synthetic custom-tool output image protocol probes, at most three generations.
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
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
type outcome struct {
	HTTP           int
	Complete, Stop bool
}

func digest(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func uuid() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("random unavailable")
	}
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	v := hex.EncodeToString(b[:])
	return v[:8] + "-" + v[8:12] + "-" + v[12:16] + "-" + v[16:20] + "-" + v[20:]
}
func emit(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
func headers(h http.Header, a account) {
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
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return
	}
	var claims, auth map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) == nil && json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil {
		var id string
		_ = json.Unmarshal(auth["chatgpt_account_user_id"], &id)
		h.Set("X-OpenAI-Account-User-Id", id)
	}
}
func safeError(raw []byte, a account) map[string]any {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	if e := obj["error"]; len(e) > 0 {
		_ = json.Unmarshal(e, &obj)
	}
	out := map[string]any{}
	for _, k := range []string{"message", "type", "code", "param"} {
		var s string
		if json.Unmarshal(obj[k], &s) == nil && s != "" {
			s = strings.ReplaceAll(s, a.Token, "[TOKEN]")
			if a.ID != "" {
				s = strings.ReplaceAll(s, a.ID, "[ACCOUNT]")
			}
			if len(s) > 1024 {
				s = s[:1024]
			}
			out[k] = s
		}
	}
	return out
}
func body(f fixture, imagePart map[string]any) []byte {
	callID := "call_" + strings.ReplaceAll(uuid(), "-", "")
	tool := map[string]any{"type": "custom", "name": "exec", "description": "Read a synthetic image fixture.", "format": map[string]string{"type": "grammar", "syntax": "lark", "definition": "start: /[\\s\\S]+/"}}
	defs := []any{map[string]any{"type": "namespace", "name": "functions", "description": "Synthetic diagnostic tools.", "tools": []any{tool}}}
	defsRaw, _ := json.Marshal(defs)
	output := []any{map[string]string{"type": "input_text", "text": "synthetic image reader result"}}
	if imagePart == nil {
		output = append(output, map[string]string{"type": "input_text", "text": "OK"})
	} else {
		output = append(output, imagePart)
	}
	input := []any{
		map[string]any{"type": "additional_tools", "id": "at_" + digest(append([]byte("codex-agent:bps-additional-tools:v1\x00"), defsRaw...))[:32], "role": "developer", "tools": defs},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "Read the synthetic image result using exec."}}},
		map[string]any{"type": "custom_tool_call", "id": "ctc_" + strings.ReplaceAll(uuid(), "-", ""), "call_id": callID, "namespace": "functions", "name": "exec", "input": "read_synthetic_image()", "status": "completed"},
		map[string]any{"type": "custom_tool_call_output", "call_id": callID, "output": output},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": "If the tool result contains an image, name its dominant color in one word. Otherwise repeat its final text item exactly. Do not call any more tools."}}},
	}
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "model_selection": "explicit", "stream": true, "store": false, "input": input, "metadata": map[string]string{"task_id": uuid(), "turn_id": uuid(), "bps_tools_version_id": f.Version, "agent_iteration": "1"}})
	return raw
}
func run(c *http.Client, f fixture, name string, part map[string]any) outcome {
	a := f.Accounts[0]
	raw := body(f, part)
	req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(raw))
	req.GetBody = nil
	headers(req.Header, a)
	started := time.Now()
	res, e := c.Do(req)
	row := map[string]any{"case": name, "request_sha256": digest(raw), "request_bytes": len(raw), "headers_ms": time.Since(started).Milliseconds(), "completed": false, "automatic_retries": 0, "synthetic_only": true, "tool_call_type": "custom_tool_call", "tool_output_type": "custom_tool_call_output", "image_present": part != nil, "detail_present": part != nil && part["detail"] != nil}
	result := outcome{}
	if e != nil {
		row["transport_error_type"] = fmt.Sprintf("%T", e)
		result.Stop = true
		emit(row)
		return result
	}
	result.HTTP = res.StatusCode
	result.Stop = res.StatusCode == 403 || res.StatusCode == 429
	row["http"] = res.StatusCode
	data, re := io.ReadAll(res.Body)
	_ = res.Body.Close()
	row["response_sha256"] = digest(data)
	row["response_bytes"] = len(data)
	row["read_complete"] = re == nil
	var answer strings.Builder
	for _, line := range bytes.Split(data, []byte("\n")) {
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
			result.Complete = kind == "response.completed"
			row["completed"] = result.Complete
			var response map[string]json.RawMessage
			_ = json.Unmarshal(event["response"], &response)
			if v := response["model"]; len(v) > 0 {
				row["returned_model"] = v
			}
			if v := response["usage"]; len(v) > 0 {
				row["usage"] = v
				var usage struct {
					Details map[string]json.RawMessage `json:"input_tokens_details"`
				}
				_ = json.Unmarshal(v, &usage)
				if read := usage.Details["cached_tokens"]; len(read) > 0 {
					row["cache_read_tokens"] = read
				}
				if write := usage.Details["cache_write_tokens"]; len(write) > 0 {
					row["cache_write_tokens"] = write
				}
			}
			if !result.Complete {
				row["error"] = safeError(event["response"], a)
			}
		}
	}
	if res.StatusCode >= 400 {
		row["error"] = safeError(data, a)
	}
	row["synthetic_answer"] = strings.TrimSpace(answer.String())
	row["elapsed_ms"] = time.Since(started).Milliseconds()
	emit(row)
	if re != nil {
		result.Stop = true
	}
	return result
}
func upload(c *http.Client, f fixture, picture []byte) string {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	p, _ := w.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="synthetic-tool-image.png"`}, "Content-Type": {"image/png"}})
	_, _ = p.Write(picture)
	_ = w.Close()
	req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/attachments", bytes.NewReader(buf.Bytes()))
	req.GetBody = nil
	headers(req.Header, f.Accounts[0])
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	res, e := c.Do(req)
	if e != nil {
		emit(map[string]any{"phase": "attachment", "error_type": fmt.Sprintf("%T", e)})
		return ""
	}
	raw, re := io.ReadAll(res.Body)
	_ = res.Body.Close()
	var obj struct {
		ID string `json:"openai_file_id"`
	}
	_ = json.Unmarshal(raw, &obj)
	emit(map[string]any{"phase": "attachment", "http": res.StatusCode, "file_id_present": obj.ID != "", "response_sha256": digest(raw), "elapsed_ms": time.Since(start).Milliseconds()})
	if res.StatusCode != 200 || re != nil {
		return ""
	}
	return obj.ID
}
func main() {
	var f fixture
	if json.NewDecoder(os.Stdin).Decode(&f) != nil || len(f.Accounts) == 0 {
		os.Exit(2)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	c := &http.Client{Transport: t, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	pic := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			pic.Set(x, y, color.RGBA{20, 70, 235, 255})
		}
	}
	var data bytes.Buffer
	_ = png.Encode(&data, pic)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())
	emit(map[string]any{"kind": "fixture", "image_width": 512, "image_height": 512, "image_png_bytes": data.Len(), "image_sha256": digest(data.Bytes()), "max_generation_requests": 3, "customer_history_replayed": false})
	control := run(c, f, "custom_tool_text_control", nil)
	if control.Stop || !control.Complete {
		return
	}
	inline := run(c, f, "custom_tool_inline_image_detail_auto", map[string]any{"type": "input_image", "image_url": uri, "detail": "auto"})
	if inline.Stop {
		return
	}
	if inline.Complete {
		_ = run(c, f, "custom_tool_inline_image_detail_missing", map[string]any{"type": "input_image", "image_url": uri})
		return
	}
	id := upload(c, f, data.Bytes())
	if id == "" {
		return
	}
	defer func() {
		req, _ := http.NewRequest(http.MethodDelete, "https://bps.openai.com/basispoints/api/attachments/delete/"+url.PathEscape(id), nil)
		headers(req.Header, f.Accounts[0])
		res, e := c.Do(req)
		row := map[string]any{"phase": "delete_synthetic_attachment", "created_by_this_probe": true}
		if e != nil {
			row["error_type"] = fmt.Sprintf("%T", e)
		} else {
			row["http"] = res.StatusCode
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
		}
		emit(row)
	}()
	_ = run(c, f, "custom_tool_attachment_image_detail_auto", map[string]any{"type": "input_image", "file_id": id, "detail": "auto"})
}
