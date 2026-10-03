package server

import (
	"context"
	"encoding/json"
	"net/http"
)

// WebSocket callers use response.created's real ID to cancel active work.
// Holding that event until the first token can deadlock cancellation during
// reasoning. Once this protocol identity is exposed, the connection is pinned
// to this attempt; only pre-created authentication failures may switch owners.
func commitBPSWebSocketCreated(w http.ResponseWriter, ctx context.Context, event []byte) error {
	state := officialFailoverFrom(ctx)
	if state == nil {
		return nil
	}
	var value struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(event, &value) != nil || value.Type != "response.created" {
		return nil
	}
	if err := state.commitOwner(); err != nil {
		return err
	}
	for current := w; current != nil; {
		if buffered, ok := current.(*officialFailoverWriter); ok {
			return buffered.deliver()
		}
		unwrapper, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		current = unwrapper.Unwrap()
	}
	return nil
}
