package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cpaResponseLimit = 16 << 20

type cpaAPICallRequest struct {
	AuthIndex        string            `json:"auth_index"`
	AuthIndexCamel   string            `json:"authIndex"`
	AuthIndexPascal  string            `json:"AuthIndex"`
	EnsureFresh      bool              `json:"ensure_fresh_token"`
	EnsureFreshCamel bool              `json:"ensureFreshToken"`
	Method           string            `json:"method"`
	URL              string            `json:"url"`
	Header           map[string]string `json:"header"`
	Data             string            `json:"data"`
}

func cpaAPICallIndex(input cpaAPICallRequest) string {
	for _, v := range []string{input.AuthIndex, input.AuthIndexCamel, input.AuthIndexPascal} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// The compatibility raw proxy also exposes the standard inference endpoints.
// Model generation uses the same admission lease as the ordinary gateway;
// metadata GETs and unrelated administrative calls retain their old behavior.
func cpaAPICallInference(method string, endpoint *url.URL) bool {
	if method != http.MethodPost {
		return false
	}
	path := strings.TrimRight(endpoint.Path, "/")
	for _, suffix := range []string{"/responses", "/responses/compact", "/chat/completions", "/completions"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (p *Proxy) handleCPAAPICall(w http.ResponseWriter, r *http.Request) {
	var input cpaAPICallRequest
	if err := compatReadJSON(w, r, &input, cpaFileLimit); err != nil {
		compatError(w, r, err)
		return
	}
	input.Method = strings.ToUpper(strings.TrimSpace(input.Method))
	if input.Method == "" || len(input.Method) > 32 {
		compatError(w, r, cpaInvalid("missing or invalid method"))
		return
	}
	endpoint, err := url.Parse(strings.TrimSpace(input.URL))
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		compatError(w, r, cpaInvalid("invalid url"))
		return
	}
	if _, err = cpaCustomHeaders(cpaRaw(input.Header)); err != nil {
		compatError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var row adminCompatAccount
	index := cpaAPICallIndex(input)
	if index != "" {
		row, err = p.cpaFind(ctx, "", index)
		if err != nil {
			compatError(w, r, err)
			return
		}
		if cpaAPICallInference(input.Method, endpoint) {
			admission, admitted, admissionErr := p.acquireGatewayAccountContext(ctx, row.Account)
			if admissionErr == nil && !admitted {
				admissionErr = &gatewayAccountBusyError{}
			}
			if admissionErr != nil {
				status, _, _ := gatewaySelectionFailure(admissionErr)
				compatErrorStatus(w, r, status, admissionErr.Error())
				return
			}
			defer admission.Release()
			ctx = admission.Context
		}
		if (input.EnsureFresh || input.EnsureFreshCamel) && row.Account.ExpiresAt > 0 && row.Account.ExpiresAt <= time.Now().Add(2*time.Minute).Unix() {
			row.Account, err = p.compatRefreshAccount(ctx, row.Account.ID)
			if err != nil {
				cpaReply(w, 400, map[string]string{"error": "auth token refresh failed"})
				return
			}
		}
	}
	if input.Header == nil {
		input.Header = map[string]string{}
	}
	for name, value := range input.Header {
		if strings.Contains(value, "$TOKEN$") {
			if row.Account.AccessToken == "" {
				compatError(w, r, cpaInvalid("auth_index is required for token substitution"))
				return
			}
			input.Header[name] = strings.ReplaceAll(value, "$TOKEN$", row.Account.AccessToken)
		}
	}
	host := strings.ToLower(endpoint.Hostname())
	if row.Account.AccountID != "" && (host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")) {
		found := false
		for key, value := range input.Header {
			if strings.EqualFold(key, "Chatgpt-Account-Id") && strings.TrimSpace(value) != "" {
				found = true
			}
		}
		if !found {
			input.Header["Chatgpt-Account-Id"] = row.Account.AccountID
		}
	}
	request, err := http.NewRequestWithContext(ctx, input.Method, endpoint.String(), strings.NewReader(input.Data))
	if err != nil {
		compatError(w, r, cpaInvalid("failed to build request"))
		return
	}
	for key, value := range input.Header {
		if strings.EqualFold(key, "Host") {
			request.Host = value
		} else {
			request.Header.Set(key, value)
		}
	}
	client, err := p.managedAccountClient(row.Account)
	if err != nil {
		cpaReply(w, 502, map[string]string{"error": "account transport unavailable"})
		return
	}
	response, err := client.Do(request)
	if err != nil {
		cpaReply(w, 502, map[string]string{"error": "request failed"})
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, cpaResponseLimit+1))
	if err != nil || len(body) > cpaResponseLimit {
		cpaReply(w, 502, map[string]string{"error": "failed to read bounded response"})
		return
	}
	// Preserve the remote HTTP result. Never expose transport errors containing
	// credential URLs, headers, or proxy authentication values.
	cpaReply(w, 200, map[string]any{"status_code": response.StatusCode, "header": response.Header, "body": string(body)})
}

func (p *Proxy) handleCPAModels(w http.ResponseWriter, r *http.Request) {
	row, err := p.cpaFind(r.Context(), r.URL.Query().Get("name"), r.URL.Query().Get("auth_index"))
	if err != nil {
		compatError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	endpoint, err := p.managedUpstreamURL("models")
	if err != nil {
		compatError(w, r, err)
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err == nil {
		err = p.managedUpstreamHeaders(request, row.Account)
	}
	if err != nil {
		cpaReply(w, 502, map[string]string{"error": "model request unavailable"})
		return
	}
	client, err := p.managedAccountClient(row.Account)
	if err != nil {
		cpaReply(w, 502, map[string]string{"error": "account transport unavailable"})
		return
	}
	resp, err := client.Do(request)
	if err != nil {
		cpaReply(w, 502, map[string]string{"error": "model request failed"})
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cpaResponseLimit+1))
	if err != nil || len(raw) > cpaResponseLimit || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		cpaReply(w, 502, map[string]string{"error": "model catalog unavailable"})
		return
	}
	var payload struct {
		Models []map[string]json.RawMessage `json:"models"`
		Data   []map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		cpaReply(w, 502, map[string]string{"error": "invalid model catalog"})
		return
	}
	models := payload.Models
	if models == nil {
		models = payload.Data
	}
	if models == nil {
		compatError(w, r, errors.New("model catalog unavailable"))
		return
	}
	result := []map[string]string{}
	seen := map[string]bool{}
	for _, model := range models {
		id := cpaString(model, "id")
		if id == "" {
			id = cpaString(model, "slug")
		}
		if !validManagedModel(id) || seen[id] {
			continue
		}
		seen[id] = true
		entry := map[string]string{"id": id, "object": "model"}
		if label := cpaString(model, "display_name"); label != "" {
			entry["display_name"] = label
		}
		result = append(result, entry)
	}
	cpaReply(w, 200, map[string]any{"models": result})
}
