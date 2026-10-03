package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Management checks use short-lived official processes, outside business
// admission, conversation caches and request accounting. Usage/reset credits
// stay on the existing ChatGPT account endpoints.
func (p *Proxy) checkOfficialManaged(ctx context.Context, a Account, kind, model, prompt string, phase func(string) error, delta func(string) error) (snapshot managedUpstreamSnapshot) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	snapshot = managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: a.CredentialVersion, Kind: kind, CheckedAt: started.Unix(), Source: "official_app_server_" + kind}
	if kind == "stream" || kind == "compact" {
		snapshot.Model = model
	}
	defer func() { snapshot.LatencyMs = time.Since(started).Milliseconds() }()
	fail := func(code, message string) managedUpstreamSnapshot {
		if ctx.Err() != nil {
			code, message = "canceled", "management check was canceled or timed out"
		}
		snapshot.Error = &managedUpstreamError{Code: code, Message: message}
		return snapshot
	}
	if p.Official == nil {
		return fail("configuration_error", "official runtime is unavailable")
	}
	if kind != "models" && kind != "stream" && kind != "compact" {
		return fail("configuration_error", "unsupported official runtime management check")
	}
	var proxy *ProxyEntry
	for _, id := range a.Proxies {
		if entry, ok := p.Proxies.Get(id); ok && entry.Enabled {
			proxy = entry
			break
		}
	}
	if len(a.Proxies) > 0 && proxy == nil {
		return fail("proxy_unavailable", "no available configured account proxy")
	}
	if kind == "models" {
		rpc, err := p.Official.startRPC(ctx, a, proxy)
		if err != nil {
			return fail("connection_failed", "official App Server initialization failed")
		}
		defer rpc.close()
		data, err := managedOfficialModels(ctx, rpc)
		if err != nil {
			return fail("invalid_upstream_response", "official App Server did not return a complete model catalog")
		}
		if ctx.Err() != nil {
			return fail("canceled", "management check was canceled or timed out")
		}
		snapshot.OK, snapshot.HTTPCode, snapshot.Data = true, http.StatusOK, data
		return snapshot
	}
	if !validManagedModel(model) || len(prompt) > 4096 {
		return fail("configuration_error", "invalid management inference input")
	}
	if prompt == "" {
		prompt = "Reply with exactly OK."
	}
	raw, _ := json.Marshal(prompt)
	input := officialResponseInput{Model: model, Instructions: "You are an assistant performing a connectivity test. Reply briefly.", Input: raw, Stream: kind == "stream"}
	input.Reasoning.Effort = "xhigh"
	items, err := officialInputItems(raw)
	if err != nil {
		return fail("configuration_error", "cannot prepare management input")
	}
	key, session := "management-check", bpsUUID()
	thread, pending, err := p.Official.responseThread(ctx, key, session, a, proxy, input, items)
	if err != nil {
		return fail("connection_failed", "official App Server test thread could not start")
	}
	// responseThread returns with thread.mu held. runResponse owns its unlock;
	// compactResponse deliberately leaves that ownership to this caller.
	defer func() {
		thread.rpc.close()
		mapkey := key + "\x00" + gatewayAccountRef(a) + "\x00" + session
		p.Official.mu.Lock()
		if p.Official.threads[mapkey] == thread {
			delete(p.Official.threads, mapkey)
		}
		p.Official.mu.Unlock()
	}()
	var data any
	if kind == "compact" {
		output, compactErr := thread.compactResponse(ctx, items)
		thread.mu.Unlock()
		if compactErr != nil {
			return fail("invalid_upstream_response", "official App Server compaction failed")
		}
		response, _ := json.Marshal(map[string]any{"output": output})
		data, err = parseManagedCompact(response)
	} else {
		if phase != nil && phase("streaming") != nil {
			thread.mu.Unlock()
			return fail("canceled", "management check client disconnected")
		}
		var wire bytes.Buffer
		delivered := 0
		emit := func(event map[string]any) error {
			raw, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if wire.Len()+len(raw)+8 > managedUpstreamBodyLimit {
				return errors.New("management stream exceeds response limit")
			}
			wire.WriteString("data: ")
			wire.Write(raw)
			wire.WriteString("\n\n")
			if event["type"] == "response.output_text.delta" {
				text, _ := event["delta"].(string)
				if delivered+len(text) <= 4096 {
					delivered += len(text)
					if delta != nil {
						return delta(text)
					}
				}
			}
			return nil
		}
		response, _, runErr := thread.runResponse(ctx, input, pending, emit)
		if runErr != nil {
			return fail("invalid_upstream_response", "official App Server did not return a successful complete stream result")
		}
		if err = emit(map[string]any{"type": "response.completed", "response": json.RawMessage(response)}); err == nil {
			data, err = readManagedTestStreamEvents(bytes.NewReader(wire.Bytes()), nil)
		}
	}
	if err != nil {
		return fail("invalid_upstream_response", "official App Server did not return a successful complete "+kind+" result")
	}
	if ctx.Err() != nil {
		return fail("canceled", "management check was canceled or timed out")
	}
	snapshot.OK, snapshot.HTTPCode, snapshot.Data = true, http.StatusOK, data
	return snapshot
}

// Convert only advertised fields. App Server model identifiers differ from its
// optional display name; no context length or model is inferred locally.
func managedOfficialModels(ctx context.Context, rpc *officialRPC) (any, error) {
	models := []map[string]json.RawMessage{}
	cursor := ""
	seen := map[string]bool{}
	total := 0
	for {
		params := map[string]any{"includeHidden": true, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := rpc.call(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		total += len(raw)
		if total > managedUpstreamBodyLimit {
			return nil, errors.New("model catalog exceeds response limit")
		}
		var page struct {
			Data       []map[string]json.RawMessage `json:"data"`
			NextCursor string                       `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Data == nil {
			return nil, errors.New("missing official model catalog")
		}
		for _, entry := range page.Data {
			id := entry["model"]
			var name string
			_ = json.Unmarshal(id, &name)
			if strings.TrimSpace(name) == "" {
				id = entry["id"]
			}
			value := map[string]json.RawMessage{"id": id}
			if display := entry["displayName"]; len(display) > 0 {
				value["display_name"] = display
			}
			if window := entry["contextWindow"]; len(window) > 0 {
				value["context_window"] = window
			}
			models = append(models, value)
		}
		if page.NextCursor == "" {
			break
		}
		if seen[page.NextCursor] {
			return nil, errors.New("repeated official model catalog cursor")
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	raw, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		return nil, err
	}
	return parseManagedModels(raw)
}
