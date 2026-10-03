package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
)

// Lives for one admitted attempt. No prompt/history bytes are retained.
type appServerCostObservation struct {
	ClientTurn   string
	ClientThread string
	Owner        string
	ThreadID     string
	TurnID       string
	ResponseIDs  []string
	mu           sync.Mutex
	invalid      bool
}

func appServerCostIDs(raw []byte, headers http.Header) (thread, turn string, valid bool) {
	var envelope struct {
		Metadata map[string]json.RawMessage `json:"client_metadata"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &envelope) != nil {
		return "", "", false
	}
	canonical, bodyCanonical := envelope.Metadata["x-codex-turn-metadata"]
	var text string
	if bodyCanonical {
		if json.Unmarshal(canonical, &text) != nil {
			return "", "", false
		}
	} else {
		filtered := make(http.Header)
		copyGatewayHeaders(filtered, headers, func(name string) bool { return name == "x-codex-turn-metadata" }, func(string) bool { return true })
		text = filtered.Get("X-Codex-Turn-Metadata")
	}
	if bodyCanonical || text != "" {
		var fields struct {
			Thread string `json:"thread_id"`
			Turn   string `json:"turn_id"`
		}
		if json.Unmarshal([]byte(text), &fields) != nil {
			return "", "", false
		}
		return fields.Thread, fields.Turn, true
	}
	if value, ok := envelope.Metadata["thread_id"]; ok && json.Unmarshal(value, &thread) != nil {
		return "", "", false
	}
	if value, ok := envelope.Metadata["turn_id"]; ok && json.Unmarshal(value, &turn) != nil {
		return "", "", false
	}
	return thread, turn, true
}

func newAppServerCostObservation(raw []byte, headers http.Header) *appServerCostObservation {
	thread, turn, valid := appServerCostIDs(raw, headers)
	if !valid || turn == "" {
		return nil
	}
	return &appServerCostObservation{ClientTurn: turn, ClientThread: thread}
}

func (o *appServerCostObservation) BindSent(raw []byte, headers http.Header) {
	if o == nil {
		return
	}
	var envelope struct {
		Generate *bool `json:"generate"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Generate != nil && !*envelope.Generate {
		return
	}
	thread, turn, valid := appServerCostIDs(raw, headers)
	o.mu.Lock()
	defer o.mu.Unlock()
	if !valid || thread == "" || turn == "" {
		o.invalid = true
		return
	}
	if o.ThreadID != "" && (o.ThreadID != thread || o.TurnID != turn) {
		o.invalid = true
		return
	}
	o.ThreadID, o.TurnID = thread, turn
}

// Call after outer stream response identity validation, before final record
// commitment. A response.created ID is not a completed billable response.
func (o *appServerCostObservation) ObserveResponse(raw []byte) {
	if o == nil {
		return
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil || event.Type != "response.completed" || event.Response.ID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, id := range o.ResponseIDs {
		if id == event.Response.ID {
			return
		}
	}
	o.ResponseIDs = append(o.ResponseIDs, event.Response.ID)
}

func (o *appServerCostObservation) Mapping() (appServerTurnCostMapping, bool) {
	if o == nil {
		return appServerTurnCostMapping{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.invalid || o.ClientTurn == "" || o.Owner == "" || o.ThreadID == "" || o.TurnID == "" || len(o.ResponseIDs) == 0 {
		return appServerTurnCostMapping{}, false
	}
	ids := append([]string(nil), o.ResponseIDs...)
	sort.Strings(ids)
	return appServerTurnCostMapping{Owner: o.Owner, ClientThreadID: o.ClientThread, ThreadID: o.ThreadID, TurnID: o.TurnID, ResponseIDs: ids}, true
}

type appServerCostObservationKey struct{}

func withAppServerCostObservation(ctx context.Context, o *appServerCostObservation) context.Context {
	if o == nil {
		return ctx
	}
	return context.WithValue(ctx, appServerCostObservationKey{}, o)
}
func appServerCostObservationFrom(ctx context.Context) *appServerCostObservation {
	o, _ := ctx.Value(appServerCostObservationKey{}).(*appServerCostObservation)
	return o
}
