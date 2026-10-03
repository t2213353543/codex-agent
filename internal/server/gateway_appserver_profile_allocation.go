package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type appServerRequestAllocationKey struct{}
type appServerRequestAllocation struct {
	SessionID, TurnID string
	StartedMS         int64
}

func withAppServerRequestAllocation(ctx context.Context) context.Context {
	if _, ok := ctx.Value(appServerRequestAllocationKey{}).(*appServerRequestAllocation); ok {
		return ctx
	}
	return context.WithValue(ctx, appServerRequestAllocationKey{}, &appServerRequestAllocation{SessionID: bpsUUID(), TurnID: bpsUUID(), StartedMS: time.Now().UnixMilli()})
}

// A retry retains the same allocated request identities. Explicit client
// metadata always wins. Route affinity supplies a stable identity for plain
// clients without a session header, without retaining their conversation.
func appServerRequestDefaults(req *http.Request, installation string) appServerProfileDefaults {
	allocation, _ := req.Context().Value(appServerRequestAllocationKey{}).(*appServerRequestAllocation)
	if allocation == nil {
		allocation = &appServerRequestAllocation{SessionID: bpsUUID(), TurnID: bpsUUID(), StartedMS: time.Now().UnixMilli()}
	}
	session := allocation.SessionID
	if route, _ := req.Context().Value(gatewayRequestRouteKey{}).(*gatewayRequestRoute); route != nil && route.session != "" {
		oid := [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
		session = appServerUUIDString(appServerUUIDv5(oid, []byte("app-server-cli-route\x00"+route.keyID+"\x00"+route.session)))
	}
	return appServerProfileDefaults{InstallationID: installation, SessionID: session, ThreadID: session, TurnID: allocation.TurnID, TurnStartedAtUnixMS: &allocation.StartedMS}
}

func appServerRequestIdentityHeaders(headers http.Header, body map[string]json.RawMessage, defaults appServerProfileDefaults) http.Header {
	result := headers.Clone()
	// Client metadata can be authoritative even if their HTTP library omits
	// identity headers. Do not let a locally generated header override it.
	var metadata map[string]string
	_ = json.Unmarshal(body["client_metadata"], &metadata)
	var canonical map[string]json.RawMessage
	_ = json.Unmarshal([]byte(metadata["x-codex-turn-metadata"]), &canonical)
	for _, field := range []struct{ header, flat, canonical, fallback string }{{"Session-Id", "session_id", "session_id", defaults.SessionID}, {"Thread-Id", "thread_id", "thread_id", defaults.ThreadID}} {
		value := metadata[field.flat]
		if value == "" {
			_ = json.Unmarshal(canonical[field.canonical], &value)
		}
		if value != "" {
			result.Set(field.header, value)
		}
	}
	if result.Get("Session-Id") == "" && result.Get("Session_id") == "" {
		var key string
		_ = json.Unmarshal(body["prompt_cache_key"], &key)
		if key == "" {
			key = defaults.SessionID
		}
		result.Set("Session-Id", key)
	}
	if result.Get("Thread-Id") == "" {
		session := result.Get("Session-Id")
		if session == "" {
			session = result.Get("Session_id")
		}
		result.Set("Thread-Id", session)
	}
	return result
}
