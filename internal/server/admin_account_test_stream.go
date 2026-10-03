package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// This stream belongs to the management console, never the gateway stream.
func (p *Proxy) handleManagedAccountTestStream(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version          int64  `json:"version"`
		Model            string `json:"model"`
		Prompt           string `json:"prompt"`
		ConfirmInference bool   `json:"confirm_inference"`
	}
	if err := readJSONLimit(w, r, &input, 16<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 || !input.ConfirmInference || !validManagedModel(input.Model) || len(input.Prompt) > 4096 {
		adminError(w, errors.New("positive version, exact model, confirm_inference=true and prompt within 4096 bytes are required"))
		return
	}
	a, err := p.managedControlAccount(r, input.Version)
	if err != nil {
		p.storeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		adminAPIError(w, 500, "stream_unavailable", "management streaming is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	emit := func(event string, payload any) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, err := json.Marshal(payload)
		if err == nil {
			_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		}
		if err != nil {
			cancel()
			return err
		}
		flusher.Flush()
		return ctx.Err()
	}
	phase := func(value string) error { return emit("phase", map[string]string{"phase": value}) }
	if phase("connecting") != nil {
		return
	}
	snapshot := p.checkManagedUpstreamOptions(ctx, a, "stream", input.Model, input.Prompt, phase, func(text string) error { return emit("delta", map[string]string{"text": text}) })
	if ctx.Err() != nil {
		return
	}
	if err := p.cacheManagedUpstream(ctx, snapshot); err != nil {
		snapshot.OK = false
		snapshot.Error = &managedUpstreamError{"storage_error", "management result could not be persisted"}
	}
	if p.Store.DB != nil {
		result := "failed"
		if snapshot.OK {
			result = "ok"
		}
		if _, err := p.Store.DB.Pool.Exec(ctx, "INSERT INTO audit_logs(action,entity_id,result) VALUES('account.check.stream',$1,$2)", a.ID, result); err != nil {
			snapshot.OK = false
			snapshot.Error = &managedUpstreamError{"storage_error", "management audit could not be persisted"}
		}
	}
	_ = emit("result", snapshot)
}
