package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type managedResetCredit struct {
	ID          string  `json:"id"`
	ResetType   string  `json:"reset_type"`
	Status      string  `json:"status"`
	GrantedAt   string  `json:"granted_at"`
	ExpiresAt   *string `json:"expires_at"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}
type managedResetCredits struct {
	AvailableCount    *int64               `json:"available_count"`
	Credits           []managedResetCredit `json:"credits"`
	CreditListPresent bool                 `json:"credit_list_present"`
	CountSource       string               `json:"count_source"`
}

func parseManagedResetCredits(raw []byte) (any, error) {
	data := managedResetCredits{Credits: []managedResetCredit{}, CountSource: "unknown"}
	var values []json.RawMessage
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errInvalid
	}
	if raw[0] == '[' {
		if json.Unmarshal(raw, &values) != nil {
			return nil, errInvalid
		}
		data.CreditListPresent = true
	} else {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return nil, errInvalid
		}
		for _, key := range []string{"available_count", "availableCount"} {
			if value := object[key]; len(value) > 0 && string(value) != "null" {
				var count int64
				if json.Unmarshal(value, &count) != nil {
					var text string
					if json.Unmarshal(value, &text) != nil {
						return nil, errInvalid
					}
					var err error
					count, err = strconv.ParseInt(strings.TrimSpace(text), 10, 64)
					if err != nil {
						return nil, errInvalid
					}
				}
				if count < 0 {
					return nil, errInvalid
				}
				data.AvailableCount = &count
				data.CountSource = "upstream"
				break
			}
		}
		for _, key := range []string{"credits", "rate_limit_reset_credits", "items", "data"} {
			if value := object[key]; len(value) > 0 && string(value) != "null" {
				if json.Unmarshal(value, &values) != nil {
					return nil, errInvalid
				}
				data.CreditListPresent = true
				break
			}
		}
	}
	for _, value := range values {
		if string(value) == "null" {
			continue
		}
		var row map[string]json.RawMessage
		if json.Unmarshal(value, &row) != nil {
			return nil, errInvalid
		}
		read := func(keys ...string) string {
			for _, key := range keys {
				var v string
				if json.Unmarshal(row[key], &v) == nil && v != "" {
					return v
				}
			}
			return ""
		}
		credit := managedResetCredit{ID: read("id", "credit_id", "creditId"), ResetType: read("reset_type", "resetType"), Status: read("status"), GrantedAt: read("granted_at", "grantedAt")}
		if len(credit.ID) > 256 {
			return nil, errInvalid
		}
		if value := read("expires_at", "expiresAt"); value != "" {
			credit.ExpiresAt = &value
		}
		if value := read("title"); value != "" {
			credit.Title = &value
		}
		if value := read("description"); value != "" {
			credit.Description = &value
		}
		data.Credits = append(data.Credits, credit)
	}
	if data.AvailableCount == nil && data.CreditListPresent {
		var count int64
		for _, credit := range data.Credits {
			if credit.Status == "available" && credit.ResetType == "codex_rate_limits" {
				count++
			}
		}
		data.AvailableCount = &count
		data.CountSource = "credit_list"
	}
	if !data.CreditListPresent && data.AvailableCount == nil {
		return nil, errInvalid
	}
	return data, nil
}

type managedResetOperation struct {
	CreditID          string                  `json:"credit_id"`
	CredentialVersion int64                   `json:"credential_version"`
	Phase             string                  `json:"phase"`
	Snapshot          managedUpstreamSnapshot `json:"snapshot"`
}

func (p *Proxy) handleManagedAccountPrivacy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version         int64 `json:"version"`
		DisableTraining bool  `json:"disable_training"`
		Confirm         bool  `json:"confirm"`
	}
	if err := readJSONLimit(w, r, &input, 8<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 || !input.DisableTraining || !input.Confirm {
		adminError(w, errors.New("disabling upstream training requires version, disable_training=true and confirm=true"))
		return
	}
	a, err := p.managedControlAccount(r, input.Version)
	if err != nil {
		p.storeError(w, err)
		return
	}
	base, err := p.managedUpstreamURL("usage")
	if err != nil {
		p.storeError(w, err)
		return
	}
	endpoint, _ := url.Parse(base)
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/wham/usage") + "/settings/user"
	endpoint.RawQuery = url.Values{"feature": {"training_allowed"}, "value": {"false"}}.Encode()
	snapshot := p.runManagedControl(r.Context(), a, "privacy", http.MethodPatch, endpoint.String(), nil)
	if snapshot.OK {
		snapshot.Data = map[string]any{"training_allowed": false, "verification": "upstream_acknowledged"}
	}
	if err := p.persistManagedControl(r.Context(), snapshot); err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, snapshot)
}

func (p *Proxy) managedControlAccount(r *http.Request, version int64) (Account, error) {
	a, err := p.Store.Get(r.PathValue("name"))
	if err != nil {
		return Account{}, err
	}
	if a.ID != r.PathValue("name") {
		return Account{}, errNotFound
	}
	if a.Version != version {
		return Account{}, errVersionConflict
	}
	return a, nil
}

func (p *Proxy) handleManagedAccountResetCredit(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Version   int64  `json:"version"`
		CreditID  string `json:"credit_id"`
		RequestID string `json:"request_id"`
		Confirm   bool   `json:"confirm"`
	}
	if err := readJSONLimit(w, r, &input, 8<<10); err != nil {
		adminError(w, err)
		return
	}
	if input.Version < 1 || !input.Confirm || len(input.CreditID) > 256 || !validRotationRequestID(input.CreditID) || !validRotationRequestID(input.RequestID) {
		adminError(w, errors.New("reset credit consumption requires version, a credit_id, a unique request_id and confirm=true"))
		return
	}
	a, err := p.managedControlAccount(r, input.Version)
	if err != nil {
		p.storeError(w, err)
		return
	}
	snapshot, err := p.managedConsumeResetCredit(r.Context(), a, input.CreditID, input.RequestID)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, snapshot)
}

// Caller authorization is explicit (manual confirmation or enabled policy).
// Every first attempt rechecks the live credit immediately before consumption.
func (p *Proxy) managedConsumeResetCredit(ctx context.Context, a Account, creditID, requestID string) (managedUpstreamSnapshot, error) {
	var zero managedUpstreamSnapshot
	if !validRotationRequestID(creditID) || !validRotationRequestID(requestID) {
		return zero, errInvalid
	}
	current, err := p.Store.Get(a.ID)
	if err != nil {
		return zero, err
	}
	if current.ID != a.ID || current.Version != a.Version {
		return zero, errVersionConflict
	}
	a = current
	credentialVersion := a.CredentialVersion
	key := a.ID + ":" + requestID
	var previous *managedUpstreamSnapshot
	err = p.managementState().Update(ctx, "account-reset-credit", key, func(entry *managementStateEntry) error {
		if entry.Version > 0 {
			var operation managedResetOperation
			if err := json.Unmarshal(entry.Payload, &operation); err != nil {
				return err
			}
			if operation.CreditID != creditID || operation.CredentialVersion != credentialVersion {
				return errConflict
			}
			if operation.Phase != "finished" {
				return errConflict
			}
			previous = &operation.Snapshot
			return nil
		}
		payload := managedResetOperation{CreditID: creditID, CredentialVersion: credentialVersion, Phase: "started"}
		entry.Payload, err = json.Marshal(payload)
		return err
	})
	if err != nil {
		return zero, err
	}
	if previous != nil {
		return *previous, nil
	}
	endpoint, err := p.managedUpstreamURL("reset_credits")
	if err != nil {
		return zero, err
	}
	raw, _ := json.Marshal(map[string]string{"redeem_request_id": requestID, "credit_id": creditID})
	// Replays use the same idempotency key. Unknown outcomes are recorded and
	// never automatically retried or represented as a successful quota reset.
	checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
	fresh := p.checkManagedUpstream(checkCtx, a, "reset_credits", "")
	checkCancel()
	snapshot := fresh
	snapshot.Kind = "reset_credit"
	snapshot.Source = "upstream_reset_credit"
	snapshot.OK = false
	snapshot.Error = &managedUpstreamError{"credit_unavailable", "credit is unavailable, expired or could not be verified"}
	snapshot.Data = map[string]bool{"outcome_unknown": false}
	available := false
	if fresh.OK {
		data, _ := fresh.Data.(managedResetCredits)
		if data.AvailableCount != nil && *data.AvailableCount > 0 {
			for _, credit := range data.Credits {
				if credit.ID != creditID || credit.Status != "available" || credit.ResetType != "codex_rate_limits" || credit.ExpiresAt == nil {
					continue
				}
				if credit.ExpiresAt != nil {
					expiry, err := time.Parse(time.RFC3339, *credit.ExpiresAt)
					if err != nil || !expiry.After(time.Now()) {
						continue
					}
				}
				available = true
			}
		}
	}
	latest, latestErr := p.Store.GetMetadata(a.ID)
	if latestErr == nil && latest.Version == a.Version && available && ctx.Err() == nil {
		snapshot = p.runManagedControl(ctx, a, "reset_credit", http.MethodPost, endpoint+"/consume", raw)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err = p.managementState().Update(ctx, "account-reset-credit", key, func(entry *managementStateEntry) error {
		var operation managedResetOperation
		if json.Unmarshal(entry.Payload, &operation) != nil {
			return errInvalid
		}
		operation.Phase = "finished"
		operation.Snapshot = snapshot
		entry.Payload, err = json.Marshal(operation)
		return err
	})
	if err != nil {
		return snapshot, err
	}
	if err := p.persistManagedControl(ctx, snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (p *Proxy) runManagedControl(ctx context.Context, a Account, kind, method, endpoint string, body []byte) (snapshot managedUpstreamSnapshot) {
	started := time.Now()
	snapshot = managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: a.CredentialVersion, Kind: kind, CheckedAt: started.Unix(), Source: "upstream_" + kind, Data: nil}
	defer func() { snapshot.LatencyMs = time.Since(started).Milliseconds() }()
	fail := func(code, message string, unknown bool) managedUpstreamSnapshot {
		snapshot.Error = &managedUpstreamError{code, message}
		snapshot.Data = map[string]bool{"outcome_unknown": unknown}
		return snapshot
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	client, err := p.managedAccountClient(a)
	if err != nil {
		return fail("proxy_unavailable", "configured account proxy is unavailable", false)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return fail("configuration_error", "invalid management endpoint", false)
	}
	if err := p.managedUpstreamHeaders(request, a); err != nil {
		return fail("configuration_error", "cannot load management settings", false)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if kind == "privacy" {
		request.Header.Set("Origin", "https://chatgpt.com")
		request.Header.Set("Referer", "https://chatgpt.com/")
	}
	response, err := client.Do(request)
	if err != nil {
		return fail("outcome_unknown", "upstream operation outcome is unknown; refresh upstream status before another operation", true)
	}
	defer response.Body.Close()
	snapshot.HTTPCode = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if kind == "reset_credit" && response.StatusCode >= 500 {
			return fail("outcome_unknown", "upstream server failure left the credit consumption outcome unknown; inspect quota before another operation", true)
		}
		return fail("upstream_http_error", "upstream rejected the management operation; inspect HTTP status", false)
	}
	if kind == "reset_credit" {
		var data struct {
			Code         string `json:"code"`
			WindowsReset int64  `json:"windows_reset"`
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
		if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &data) != nil || !accountHas([]string{"reset", "nothing_to_reset", "no_credit", "already_redeemed"}, data.Code) || data.WindowsReset < 0 {
			return fail("outcome_unknown", "upstream returned an unrecognized reset outcome; refresh quota before another operation", true)
		}
		snapshot.Data = data
		snapshot.OK = true
	} else {
		snapshot.OK = true
	}
	return snapshot
}

func (p *Proxy) persistManagedControl(ctx context.Context, snapshot managedUpstreamSnapshot) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := p.cacheManagedUpstream(ctx, snapshot); err != nil {
		return err
	}
	if snapshot.Kind == "reset_credit" {
		if err := p.managementState().Update(ctx, "account-upstream", snapshot.ID, func(entry *managementStateEntry) error {
			var rows map[string]managedUpstreamSnapshot
			if json.Unmarshal(entry.Payload, &rows) != nil {
				return errInvalid
			}
			for _, kind := range []string{"usage", "reset_credits"} {
				if old, ok := rows[kind]; ok {
					old.Stale = true
					rows[kind] = old
				}
			}
			raw, err := json.Marshal(rows)
			entry.Payload = raw
			return err
		}); err != nil {
			return err
		}
	}
	if p.Store.DB != nil {
		result := "ok"
		if !snapshot.OK {
			result = "failed"
		}
		_, err := p.Store.DB.Pool.Exec(ctx, "INSERT INTO audit_logs(action,entity_id,result) VALUES($1,$2,$3)", "account.control."+snapshot.Kind, snapshot.ID, result)
		return err
	}
	return nil
}
