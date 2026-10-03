package server

import (
	"errors"
	"testing"
)

func TestAppServerStartupRequiresCompleteNativeProtocol(t *testing.T) {
	base := "request_zstd_v1,websocket_message_v1"
	for _, test := range []struct {
		name, caps string
		want       error
	}{
		{"old-helper", base, errGatewayNativeUnboundedReady},
		{"missing-realtime", base + ",unbounded_payload_v1,websocket_message_v2", errNativeWSNoCompressionReady},
		{"complete", base + ",unbounded_payload_v1,websocket_message_v2,websocket_no_compression_v1", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			pool, command := nativeStartupFixture(t, "pooled", "", test.caps)
			err := pool.validateStartup(t.Context(), true, "unbounded_payload_v1", "websocket_message_v2", "websocket_no_compression_v1")
			if !errors.Is(err, test.want) {
				t.Fatalf("startup capability validation=%v want %v", err, test.want)
			}
			if *command == nil || (*command).ProcessState == nil {
				t.Fatal("capability probe process left running")
			}
		})
	}
}
