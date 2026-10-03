package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"time"
)

func (p *Proxy) gatewayAppServerTurnCosts(w http.ResponseWriter, r *http.Request) {
	native := r.URL.Path == "/backend-api/wham/usage/thread-estimates/query"
	if !native && r.URL.Path != "/v1/analytics/codex/turn-costs" || r.URL.RawQuery != "" {
		apiError(w, 404, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		apiError(w, 405, "method not allowed")
		return
	}
	started := time.Now()
	token := bearerToken(r)
	keyID, keyName, _, err := p.resolveGatewayKeyContext(r.Context(), token)
	if err != nil {
		apiError(w, 503, "gateway key authorization unavailable")
		return
	}
	if keyID == "" {
		apiError(w, 401, "invalid API key")
		return
	}
	delegatedOwner := appServerDelegatedOwner(r.Context())
	if native && delegatedOwner == "" {
		apiError(w, 403, "delegated account identity required for native turn estimates")
		return
	}
	if p.AppServerCLI == nil || p.AppServerCLI.turnCosts == nil {
		apiError(w, 503, "turn cost routing unavailable")
		return
	}
	settings, err := p.settingsValue()
	if err != nil {
		apiError(w, 503, "gateway settings unavailable")
		return
	}
	rec := RequestRecord{Time: started.Unix(), KeyID: keyID, KeyName: keyName, Endpoint: "analytics/codex/turn-costs", IP: clientIP(r)}
	if native {
		rec.Endpoint = "wham/usage/thread-estimates/query"
	}
	ctx, attempt := withGatewayAttemptObservation(r.Context())
	r = r.WithContext(ctx)
	w = &gatewayResponseWriter{ResponseWriter: w, record: &rec}
	defer func() { applyGatewayAttemptSnapshot(&rec, attempt); p.finishRecord(&rec, started, keyID) }()
	fail := func(status int, message string) {
		rec.Status, rec.Error, rec.HTTPCode = "error", message, status
		apiError(w, status, message)
	}
	raw, err := readBody(w, r)
	if err != nil {
		fail(400, err.Error())
		return
	}
	var input struct {
		TurnIDs []string `json:"turn_ids"`
		Threads []struct {
			ThreadID string   `json:"thread_id"`
			TurnIDs  []string `json:"turn_ids"`
		} `json:"threads"`
		IncludeSettled bool `json:"include_settled_response_ids"`
	}
	if json.Unmarshal(raw, &input) != nil || !native && input.TurnIDs == nil || native && input.Threads == nil {
		fail(400, "invalid turn cost query")
		return
	}
	clientThreads := map[string]map[string]bool{}
	if native {
		input.TurnIDs = nil
		for _, thread := range input.Threads {
			if thread.ThreadID == "" || thread.TurnIDs == nil {
				fail(400, "native cost query requires thread_id and turn_ids")
				return
			}
			for _, turn := range thread.TurnIDs {
				input.TurnIDs = append(input.TurnIDs, turn)
				if clientThreads[turn] == nil {
					clientThreads[turn] = map[string]bool{}
				}
				clientThreads[turn][thread.ThreadID] = true
			}
		}
	}
	for _, id := range input.TurnIDs {
		if id == "" {
			fail(400, "turn_ids must not contain empty IDs")
			return
		}
	}
	groups := map[string]map[string]appServerTurnCostMapping{}
	matchedClientThreads := map[string]string{}
	for _, id := range input.TurnIDs {
		mapping, known, err := p.AppServerCLI.turnCosts.Lookup(ctx, keyID, id)
		if err != nil {
			fail(503, "turn cost routing lookup unavailable")
			return
		}
		if !known || delegatedOwner != "" && mapping.Owner != delegatedOwner || native && (mapping.ClientThreadID == "" || !clientThreads[id][mapping.ClientThreadID]) {
			continue
		}
		matchedClientThreads[id] = mapping.ClientThreadID
		if groups[mapping.Owner] == nil {
			groups[mapping.Owner] = map[string]appServerTurnCostMapping{}
		}
		groups[mapping.Owner][id] = mapping
	}
	projected := map[string]appServerProjectedTurnCost{}
	nativeProjected := map[string]appServerNativeTurnEstimate{}
	var queriedAccounts []Account
	owners := make([]string, 0, len(groups))
	for owner := range groups {
		owners = append(owners, owner)
	}
	sort.Strings(owners)
	for _, owner := range owners {
		mappings := groups[owner]
		route := withGatewayRequestRoute(ctx, keyID, "turn-cost-owner:"+owner)
		defer stopGatewayRequestRoute(route)
		defer observeGatewayAdmissionLoss(&rec, route)
		account, release, err := p.pickAppServerRealtimeOwner(route, token, keyID, owner)
		if err != nil {
			rec.Status, rec.Error = "error", err.Error()
			rec.HTTPCode, _, _ = gatewaySelectionFailure(err)
			writeGatewaySelectionError(w, err)
			return
		}
		queriedAccounts = append(queriedAccounts, account)
		var payload []byte
		var upstreamStatus int
		func() {
			defer release()
			rec.Account, rec.AccountID, rec.AccountCredentialVersion = account.Name, account.ID, account.CredentialVersion
			rec.Groups = append([]string(nil), account.Groups...)
			threads := map[string]map[string]bool{}
			for _, mapping := range mappings {
				if threads[mapping.ThreadID] == nil {
					threads[mapping.ThreadID] = map[string]bool{}
				}
				threads[mapping.ThreadID][mapping.TurnID] = true
			}
			type threadQuery struct {
				Thread string   `json:"thread_id"`
				Turns  []string `json:"turn_ids"`
			}
			query := struct {
				Threads []threadQuery `json:"threads"`
				Include bool          `json:"include_settled_response_ids"`
			}{Include: true}
			threadIDs := make([]string, 0, len(threads))
			for id := range threads {
				threadIDs = append(threadIDs, id)
			}
			sort.Strings(threadIDs)
			for _, thread := range threadIDs {
				turns := make([]string, 0, len(threads[thread]))
				for turn := range threads[thread] {
					turns = append(turns, turn)
				}
				sort.Strings(turns)
				query.Threads = append(query.Threads, threadQuery{thread, turns})
			}
			body, _ := json.Marshal(query)
			route = p.withGatewayNativeDispatchAuthorization(route, token, keyID)
			proxy, proxyErr := p.gatewayRequestProxy(route, account)
			if proxyErr != nil {
				err = proxyErr
				return
			}
			if proxy != nil {
				rec.ProxyName = proxy.Name
			}
			up, _ := http.NewRequestWithContext(withoutGatewayNativeRequestCompression(route), http.MethodPost, "https://chatgpt.com/backend-api/wham/usage/thread-estimates/query", bytes.NewReader(body))
			if copyErr := copyCodexRequestHeaders(up.Header, r.Header, settings); copyErr != nil {
				err = copyErr
				return
			}
			p.AppServerCLI.applyIdentity(up.Header)
			up.Header.Set("Authorization", "Bearer "+account.AccessToken)
			if account.AccountID != "" {
				up.Header.Set("ChatGPT-Account-Id", account.AccountID)
			}
			up.Header.Set("Content-Type", "application/json")
			up.Header.Set("Accept", "*/*")
			client := p.gatewayRequestClientWithHeaderTimeout(route, proxy, gatewayClientHTTP, time.Duration(settings.UpstreamResponseHeaderTimeoutSeconds)*time.Second)
			response, requestErr := p.doObservedAccountRequest(&rec, client, up, account, doHTTPAccountRequest, func(updated Account) error {
				if gatewayAccountRef(updated) != owner {
					return &gatewayOwnerChangedError{}
				}
				return p.authorizeGatewayRetry(up.Context(), token, keyID, updated)
			})
			if requestErr != nil {
				err = requestErr
				return
			}
			defer response.Body.Close()
			upstreamStatus = response.StatusCode
			observeGatewayHeaders(&rec, response.Header)
			payload, err = io.ReadAll(response.Body)
		}()
		if err != nil {
			observeGatewayTransportError(&rec, err, "turn_cost_query")
			fail(gatewayTransportFailureStatus(err), "turn cost upstream query failed")
			return
		}
		if upstreamStatus < 200 || upstreamStatus >= 300 {
			// Do not expose an upstream account/turn identifier inside diagnostics.
			fail(upstreamStatus, "turn cost upstream query rejected")
			return
		}
		if native {
			group, projectErr := projectAppServerNativeTurnEstimates(mappings, payload)
			if projectErr != nil {
				fail(502, "invalid native turn estimate response")
				return
			}
			for turn, value := range group {
				nativeProjected[turn] = value
			}
			continue
		}
		requested := make([]string, 0, len(mappings))
		for id := range mappings {
			requested = append(requested, id)
		}
		sort.Strings(requested)
		body, err := projectAppServerTurnCosts(requested, mappings, payload)
		if err != nil {
			fail(502, "invalid turn cost upstream response")
			return
		}
		var group struct {
			Turns []appServerProjectedTurnCost `json:"turns"`
		}
		if json.Unmarshal(body, &group) != nil {
			fail(502, "invalid projected turn costs")
			return
		}
		for _, cost := range group.Turns {
			projected[cost.TurnID] = cost
		}
	}
	// Recheck key after asynchronous account/backend work, before exposing cost.
	current, _, _, err := p.resolveGatewayKeyContext(ctx, token)
	if err != nil {
		fail(503, "gateway key authorization unavailable")
		return
	}
	if current != keyID {
		fail(403, "gateway key is no longer authorized")
		return
	}
	for _, account := range queriedAccounts {
		if err = p.authorizeGatewayRetry(ctx, token, keyID, account); err != nil {
			fail(403, "turn cost account is no longer authorized")
			return
		}
	}
	if len(queriedAccounts) > 1 {
		rec.Account, rec.AccountID = "", ""
		rec.AccountCredentialVersion = 0
		rec.Groups = nil
	}
	result := struct {
		Turns []appServerProjectedTurnCost `json:"turns"`
	}{Turns: make([]appServerProjectedTurnCost, 0, len(input.TurnIDs))}
	for _, id := range input.TurnIDs {
		value, ok := projected[id]
		if !ok {
			value = appServerProjectedTurnCost{TurnID: id, Status: "pending"}
		}
		result.Turns = append(result.Turns, value)
	}
	rec.Status, rec.HTTPCode = "ok", 200
	w.Header().Set("Content-Type", "application/json")
	var response any = result
	if native {
		type threadResult struct {
			ThreadID string                        `json:"thread_id"`
			Turns    []appServerNativeTurnEstimate `json:"turns"`
		}
		out := struct {
			Threads []threadResult `json:"threads"`
		}{Threads: make([]threadResult, 0, len(input.Threads))}
		for _, thread := range input.Threads {
			row := threadResult{ThreadID: thread.ThreadID, Turns: make([]appServerNativeTurnEstimate, 0, len(thread.TurnIDs))}
			for _, id := range thread.TurnIDs {
				value, known := nativeProjected[id]
				if !known || matchedClientThreads[id] != thread.ThreadID {
					value = appServerNativeTurnEstimate{TurnID: id}
				}
				row.Turns = append(row.Turns, value)
			}
			out.Threads = append(out.Threads, row)
		}
		response = out
	}
	if err := json.NewEncoder(w).Encode(response); err != nil && !errors.Is(err, io.EOF) {
		rec.Status, rec.Error = "aborted", "client turn cost delivery failed"
	}
}
