//go:build ignore

// Two synthetic image requests; no captured customer history is replayed.
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
	Accounts   []account `json:"accounts"`
	Version    string    `json:"tools_version"`
	Control    bool      `json:"image_control"`
	Attachment bool      `json:"image_attachment"`
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
func main() {
	var f fixture
	if json.NewDecoder(os.Stdin).Decode(&f) != nil || len(f.Accounts) == 0 {
		os.Exit(2)
	}
	a := f.Accounts[0]
	size := 32
	if f.Control || f.Attachment {
		size = 512
	}
	picture := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			picture.Set(x, y, color.RGBA{40, 120, 210, 255})
		}
	}
	var pictureBytes bytes.Buffer
	_ = png.Encode(&pictureBytes, picture)
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pictureBytes.Bytes())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	client := &http.Client{Transport: transport, Timeout: 120 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	fileID := ""
	if f.Attachment {
		var upload bytes.Buffer
		writer := multipart.NewWriter(&upload)
		part, _ := writer.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="synthetic-image-probe.png"`}, "Content-Type": {"image/png"}})
		_, _ = part.Write(pictureBytes.Bytes())
		_ = writer.Close()
		req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/attachments", bytes.NewReader(upload.Bytes()))
		setHeaders(req.Header, a)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("Accept", "application/json")
		started := time.Now()
		res, err := client.Do(req)
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": "attachment", "error_type": fmt.Sprintf("%T", err)})
			return
		}
		raw, readErr := io.ReadAll(res.Body)
		_ = res.Body.Close()
		var response map[string]json.RawMessage
		_ = json.Unmarshal(raw, &response)
		_ = json.Unmarshal(response["openai_file_id"], &fileID)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"phase": "attachment", "http": res.StatusCode, "response_sha256": digest(raw), "response_bytes": len(raw), "file_id_present": fileID != "", "image_bytes": pictureBytes.Len(), "elapsed_ms": time.Since(started).Milliseconds()})
		if res.StatusCode < 200 || res.StatusCode >= 300 || readErr != nil || fileID == "" {
			return
		}
		defer func() {
			req, _ := http.NewRequest(http.MethodDelete, "https://bps.openai.com/basispoints/api/attachments/delete/"+url.PathEscape(fileID), nil)
			setHeaders(req.Header, a)
			req.Header.Set("Accept", "application/json")
			res, err := client.Do(req)
			row := map[string]any{"phase": "delete_synthetic_attachment", "created_by_this_probe": true}
			if err != nil {
				row["error_type"] = fmt.Sprintf("%T", err)
			} else {
				row["http"] = res.StatusCode
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
			_ = json.NewEncoder(os.Stdout).Encode(row)
		}()
	}
	for _, detail := range []bool{false, true} {
		if f.Attachment && !detail {
			continue
		}
		part := map[string]any{"type": "input_image", "image_url": dataURL}
		if fileID != "" {
			delete(part, "image_url")
			part["file_id"] = fileID
		}
		if detail {
			part["detail"] = "auto"
		}
		content := []any{map[string]any{"type": "input_text", "text": "Reply exactly OK."}}
		if !f.Control || detail {
			content = append(content, part)
		}
		body := map[string]any{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "stream": true, "store": false, "model_selection": "explicit", "input": []any{map[string]any{"type": "message", "role": "user", "content": content}}, "metadata": map[string]string{"task_id": uuid(), "turn_id": uuid(), "bps_tools_version_id": f.Version, "agent_iteration": "1"}}
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, "https://bps.openai.com/basispoints/api/responses", bytes.NewReader(raw))
		setHeaders(req.Header, a)
		started := time.Now()
		row := map[string]any{"case": "synthetic_image_detail", "detail_present": detail, "request_sha256": digest(raw), "image_bytes": pictureBytes.Len(), "account_token_sha256": digest([]byte(a.Token)), "completed": false, "automatic_retries": 0}
		row["image_present"] = !f.Control || detail
		row["image_width"] = size
		row["attachment_file_id_present"] = fileID != ""
		res, err := client.Do(req)
		row["headers_ms"] = time.Since(started).Milliseconds()
		if err != nil {
			row["transport_error_type"] = fmt.Sprintf("%T", err)
		} else {
			row["http"] = res.StatusCode
			row["request_id"] = res.Header.Get("X-Request-Id")
			data, readErr := io.ReadAll(res.Body)
			_ = res.Body.Close()
			row["response_sha256"] = digest(data)
			row["response_bytes"] = len(data)
			row["read_complete"] = readErr == nil
			var output strings.Builder
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
					output.WriteString(delta)
				}
				if kind == "response.completed" || kind == "response.failed" || kind == "error" {
					row["terminal"] = kind
					row["completed"] = kind == "response.completed"
					var response map[string]json.RawMessage
					_ = json.Unmarshal(event["response"], &response)
					row["usage"] = response["usage"]
					row["returned_model"] = response["model"]
				}
			}
			row["answer_is_ok"] = strings.TrimSpace(output.String()) == "OK"
		}
		row["elapsed_ms"] = time.Since(started).Milliseconds()
		_ = json.NewEncoder(os.Stdout).Encode(row)
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
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var claims map[string]json.RawMessage
	var auth map[string]json.RawMessage
	if json.Unmarshal(raw, &claims) == nil && json.Unmarshal(claims["https://api.openai.com/auth"], &auth) == nil {
		var id string
		_ = json.Unmarshal(auth["chatgpt_account_user_id"], &id)
		h.Set("X-OpenAI-Account-User-Id", id)
	}
}
