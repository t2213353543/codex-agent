package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type sub2Capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *sub2Capture) Header() http.Header { return w.header }
func (w *sub2Capture) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
func (w *sub2Capture) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}

func (p *Proxy) sub2AccountAction(w http.ResponseWriter, r *http.Request) {
	row, err := p.sub2LoadAccount(r)
	if err != nil {
		compatError(w, r, err)
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	switch action {
	case "test":
		p.sub2TestAccount(w, r, row)
		return
	case "set-privacy":
		p.sub2Privacy(w, r, row)
		return
	case "refresh":
		_, err = p.compatRefreshAccount(r.Context(), row.Account.ID)
		if err == nil {
			row, err = p.compatGetAccount(r.Context(), row.Account.ID)
		}
	case "schedulable":
		var input map[string]json.RawMessage
		if err = compatReadJSON(w, r, &input, 8<<10); err == nil {
			if value, ok := input["schedulable"]; !ok || string(value) == "null" {
				err = sub2Invalid("schedulable is required")
			} else {
				row, err = p.sub2ApplyAccount(r.Context(), row.Account.ID, map[string]json.RawMessage{"schedulable": input["schedulable"]})
			}
		}
	case "clear-error":
		row, err = p.compatMutateAccount(r.Context(), row.Account.ID, 0, func(a *Account, record *adminCompatRecord) error {
			a.LastError = ""
			a.CredentialStatus = "unverified"
			if record.Sub2 == nil {
				record.Sub2 = map[string]json.RawMessage{}
			}
			record.Sub2["status"] = sub2Raw("active")
			a.Enabled = sub2Bool(record.Sub2, "schedulable", a.Enabled)
			return nil
		})
		if err == nil {
			key := gatewayRefreshFailureKey(row.Account, p.TokenURL)
			p.refreshCooldown.clear(key)
			if db := p.Store.DB; db != nil && db.Redis != nil {
				err = db.Redis.Del(r.Context(), db.Namespace+"gateway:oauth-failure:"+key).Err()
			}
		}
	}
	if err != nil {
		compatError(w, r, err)
		return
	}
	sub2Success(w, sub2AccountDTO(row))
}

func (p *Proxy) sub2TestAccount(w http.ResponseWriter, r *http.Request, row adminCompatAccount) {
	var input struct {
		Model  string `json:"model_id"`
		Prompt string `json:"prompt"`
		Mode   string `json:"mode"`
	}
	if err := compatReadJSON(w, r, &input, 16<<10); err != nil {
		compatError(w, r, err)
		return
	}
	if !validManagedModel(input.Model) || len(input.Prompt) > 4096 || (input.Mode != "" && input.Mode != "default") {
		compatError(w, r, sub2Invalid("model_id, a prompt within 4096 bytes and default mode are required"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		compatError(w, r, fmt.Errorf("management streaming unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	emit := func(kind string, data map[string]any) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data["type"] = kind
		raw, err := json.Marshal(data)
		if err == nil {
			_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		}
		if err != nil {
			cancel()
			return err
		}
		flusher.Flush()
		return nil
	}
	phase := func(phase string) error { return emit("test_start", map[string]any{"text": phase}) }
	if err := phase("connecting"); err != nil {
		return
	}
	snapshot := p.checkManagedUpstreamOptions(ctx, row.Account, "stream", input.Model, input.Prompt, phase, func(text string) error { return emit("content", map[string]any{"text": text}) })
	if ctx.Err() != nil {
		return
	}
	if err := p.cacheManagedUpstream(ctx, snapshot); err != nil {
		snapshot.OK = false
		snapshot.Error = &managedUpstreamError{"storage_error", "management result could not be persisted"}
	}
	if db := p.Store.DB; db != nil {
		result := "failed"
		if snapshot.OK {
			result = "ok"
		}
		if _, err := db.Pool.Exec(ctx, "INSERT INTO audit_logs(action,entity_id,result) VALUES('account.check.sub2',$1,$2)", row.Account.ID, result); err != nil {
			snapshot.OK = false
			snapshot.Error = &managedUpstreamError{"storage_error", "management audit could not be persisted"}
		}
	}
	if !snapshot.OK {
		message := "upstream test failed"
		if snapshot.Error != nil {
			message = snapshot.Error.Message
		}
		_ = emit("error", map[string]any{"error": message})
	}
	_ = emit("test_complete", map[string]any{"success": snapshot.OK})
}

func (p *Proxy) sub2Privacy(w http.ResponseWriter, r *http.Request, row adminCompatAccount) {
	base, err := p.managedUpstreamURL("usage")
	if err != nil {
		compatError(w, r, err)
		return
	}
	endpoint, _ := url.Parse(base)
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/wham/usage") + "/settings/user"
	endpoint.RawQuery = url.Values{"feature": {"training_allowed"}, "value": {"false"}}.Encode()
	snapshot := p.runManagedControl(r.Context(), row.Account, "privacy", http.MethodPatch, endpoint.String(), nil)
	if snapshot.OK {
		snapshot.Data = map[string]any{"training_allowed": false, "verification": "upstream_acknowledged"}
	}
	if err = p.persistManagedControl(r.Context(), snapshot); err != nil {
		compatError(w, r, err)
		return
	}
	if !snapshot.OK {
		compatError(w, r, sub2Invalid("upstream privacy setting failed"))
		return
	}
	row, err = p.compatMutateAccount(r.Context(), row.Account.ID, row.Account.Version, func(a *Account, record *adminCompatRecord) error {
		if record.Sub2 == nil {
			record.Sub2 = map[string]json.RawMessage{}
		}
		extra := sub2Object(record.Sub2["extra"])
		extra["privacy_mode"] = sub2Raw("training_off")
		record.Sub2["extra"] = sub2Raw(extra)
		return nil
	})
	if err != nil {
		compatError(w, r, err)
		return
	}
	sub2Success(w, sub2AccountDTO(row))
}
