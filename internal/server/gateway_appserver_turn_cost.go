package server

import (
	"encoding/json"
	"fmt"
)

// This is the minimum provenance required to translate a client's turn query.
// Owner is the gateway account reference, never inferred from backend JSON.
// ThreadID and TurnID are the IDs actually sent to the model upstream.
type appServerTurnCostMapping struct {
	Owner          string   `json:"owner"`
	ClientThreadID string   `json:"client_thread_id,omitempty"`
	ThreadID       string   `json:"thread_id"`
	TurnID         string   `json:"turn_id"`
	ResponseIDs    []string `json:"response_ids"`
}

type appServerProjectedTurnCost struct {
	TurnID          string  `json:"turn_id"`
	Status          string  `json:"status"`
	TotalUSD        *string `json:"total_usd"`
	EventCount      *uint64 `json:"event_count"`
	Responses       any     `json:"responses"`
	Model           *string `json:"model"`
	Speed           *string `json:"speed"`
	ReasoningEffort *string `json:"reasoning_effort"`
}

type appServerNativeTurnEstimate struct {
	TurnID  string   `json:"turn_id"`
	Model   *string  `json:"model"`
	Micros  *int64   `json:"estimated_usage_usd_micros"`
	Settled []string `json:"settled_response_ids"`
}

// Native backend consumers retain their own settlement/amount policy. Preserve
// actual nullable values and response IDs; only translate the admitted turn ID.
func projectAppServerNativeTurnEstimates(mappings map[string]appServerTurnCostMapping, raw json.RawMessage) (map[string]appServerNativeTurnEstimate, error) {
	var result struct {
		Threads []struct {
			ID    string                        `json:"thread_id"`
			Turns []appServerNativeTurnEstimate `json:"turns"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	type identity struct{ thread, turn string }
	observed := map[identity]appServerNativeTurnEstimate{}
	ambiguous := map[identity]bool{}
	for _, thread := range result.Threads {
		for _, turn := range thread.Turns {
			id := identity{thread.ID, turn.TurnID}
			if _, ok := observed[id]; ok {
				ambiguous[id] = true
			}
			observed[id] = turn
		}
	}
	output := map[string]appServerNativeTurnEstimate{}
	for clientTurn, mapping := range mappings {
		id := identity{mapping.ThreadID, mapping.TurnID}
		value, ok := observed[id]
		if !ok || ambiguous[id] || mapping.Owner == "" || mapping.ClientThreadID == "" {
			continue
		}
		value.TurnID = clientTurn
		output[clientTurn] = value
	}
	return output, nil
}

// Mirrors official turn_cost_worker_chatgpt.rs: a visible nonnegative amount
// is usable only after all expected responses have settled. Unknown mapping,
// absent amount and incomplete settlement remain pending, never zero-priced.
// The caller must load mappings under the admitted API key and query each
// owner separately; this pure function performs no network or account lookup.
func projectAppServerTurnCosts(requested []string, mappings map[string]appServerTurnCostMapping, backend json.RawMessage) ([]byte, error) {
	type turn struct {
		ID      string   `json:"turn_id"`
		Model   *string  `json:"model"`
		Micros  *int64   `json:"estimated_usage_usd_micros"`
		Settled []string `json:"settled_response_ids"`
	}
	var result struct {
		Threads []struct {
			ID    string `json:"thread_id"`
			Turns []turn `json:"turns"`
		} `json:"threads"`
	}
	if err := json.Unmarshal(backend, &result); err != nil {
		return nil, fmt.Errorf("invalid ChatGPT turn cost response: %w", err)
	}
	type identity struct{ thread, turn string }
	observed := map[identity]turn{}
	ambiguous := map[identity]bool{}
	for _, thread := range result.Threads {
		for _, row := range thread.Turns {
			key := identity{thread.ID, row.ID}
			if _, exists := observed[key]; exists {
				ambiguous[key] = true
			}
			observed[key] = row
		}
	}
	output := struct {
		Turns []appServerProjectedTurnCost `json:"turns"`
	}{Turns: make([]appServerProjectedTurnCost, 0, len(requested))}
	for _, clientTurn := range requested {
		cost := appServerProjectedTurnCost{TurnID: clientTurn, Status: "pending"}
		mapping, known := mappings[clientTurn]
		id := identity{mapping.ThreadID, mapping.TurnID}
		row, found := observed[id]
		if known && mapping.Owner != "" && mapping.ThreadID != "" && mapping.TurnID != "" && len(mapping.ResponseIDs) > 0 && found && !ambiguous[id] && row.Micros != nil && *row.Micros >= 0 && row.Settled != nil {
			settled := make(map[string]bool, len(row.Settled))
			for _, response := range row.Settled {
				settled[response] = true
			}
			covered := true
			for _, expected := range mapping.ResponseIDs {
				if expected == "" || !settled[expected] {
					covered = false
					break
				}
			}
			if covered {
				amount := fmt.Sprintf("%d.%06d", *row.Micros/1_000_000, *row.Micros%1_000_000)
				count := uint64(len(row.Settled))
				cost.Status, cost.TotalUSD, cost.EventCount, cost.Model = "priced", &amount, &count, row.Model
			}
		}
		output.Turns = append(output.Turns, cost)
	}
	return json.Marshal(output)
}
