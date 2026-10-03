package server

import "context"

type gatewayPayloadPolicyKey struct{}

// Explicit local dispatch policy, never a client-controlled header. Native
// helpers must negotiate support before accepting unrestricted payloads.
func withGatewayUnboundedPayload(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, gatewayPayloadPolicyKey{}, enabled)
}
func gatewayUnboundedPayload(ctx context.Context) bool {
	enabled, _ := ctx.Value(gatewayPayloadPolicyKey{}).(bool)
	return enabled
}

func gatewayPayloadUnboundedOption(values []bool) bool { return len(values) > 0 && values[0] }
