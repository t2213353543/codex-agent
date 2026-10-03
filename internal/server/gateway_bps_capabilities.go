package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"codex-agent/internal/bps"
)

// A model access rejection can mean an obsolete Codex-only model selection.
// Consult the same account's current PowerPoint catalog before classifying it;
// never infer availability from a name, retry generation, or change the model.
// Ordinary successful requests pay no extra catalog round trip.
func (p *Proxy) bpsWriteCapabilityError(w http.ResponseWriter, r *http.Request, ctx context.Context, account Account, proxy *ProxyEntry, keyID string, prepared *bps.Request, settings Settings, rec *RequestRecord, status int) bool {
	if status != http.StatusForbidden && status != http.StatusUnprocessableEntity {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Keep inference attempt/body metadata distinct from the diagnostic GET.
	ctx = context.WithValue(ctx, gatewayAttemptObservationContextKey{}, (*gatewayAttemptObservation)(nil))
	diagnostic := RequestRecord{}
	res, err := p.bpsRequest(ctx, r, account, proxy, keyID, "/responses/access", nil, settings, &diagnostic)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	payload, err := io.ReadAll(io.LimitReader(res.Body, (8<<20)+1))
	if err != nil || len(payload) > 8<<20 {
		return false
	}
	catalog, err := bps.ParseAccessCatalog(payload)
	if err != nil {
		return false
	}
	var requested map[string]json.RawMessage
	_ = json.Unmarshal(prepared.Body, &requested)
	var effort string
	_ = json.Unmarshal(requested["reasoning_effort"], &effort)
	available := make([]map[string]any, 0, len(catalog.ModelCatalog.Models))
	found, effortSupported := false, effort == ""
	for _, model := range catalog.ModelCatalog.Models {
		levels := make([]string, 0, len(model.Efforts))
		for _, level := range model.Efforts {
			levels = append(levels, level.Value)
			if model.ID == prepared.Model && level.Value == effort {
				effortSupported = true
			}
		}
		if model.ID == prepared.Model {
			found = true
		}
		available = append(available, map[string]any{"id": model.ID, "supported_reasoning_levels": levels, "default_reasoning_level": model.DefaultEffort})
	}
	param, code, message := "model", "model_not_available", fmt.Sprintf("The upstream does not currently offer model %q for the selected account; choose an advertised model or switch inference mode", prepared.Model)
	if found {
		if effortSupported {
			return false
		}
		param, code, message = "reasoning.effort", "unsupported_parameter", fmt.Sprintf("The upstream does not currently advertise reasoning effort %q for model %q; choose an advertised level or switch inference mode", effort, prepared.Model)
	}
	rec.Status, rec.Error = "error", message
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": code, "param": param, "message": message, "upstream_status": status, "available_models": available}})
	return true
}
