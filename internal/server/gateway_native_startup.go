package server

import (
	"context"
	"os"
)

// Run only before publishing the pool. A disposable helper validates local
// configuration and exits without an IPC exchange or any upstream request.
func (p *gatewayNativePool) validateStartup(ctx context.Context, websocket bool, required ...string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	child, err := p.start(ctx, gatewayNativeKey{origin: "http://127.0.0.1:1"}, nil)
	if err != nil {
		return err
	}
	defer func() {
		child.shutdown(p.options.stop)
		if err != nil {
			return
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if child.command.ProcessState == nil || !child.command.ProcessState.Success() {
			err = errGatewayNativeUnavailable
		} else if _, statErr := os.Lstat(child.dir); !os.IsNotExist(statErr) {
			err = errGatewayNativeUnavailable
		}
	}()
	if !child.capabilities["request_zstd_v1"] {
		return errGatewayCompressionCapability
	}
	if websocket && !child.capabilities["websocket_message_v1"] {
		return errNativeWSCapability
	}
	for _, capability := range required {
		if !child.capabilities[capability] {
			if capability == "websocket_no_compression_v1" {
				return errNativeWSNoCompressionReady
			}
			return errGatewayNativeUnboundedReady
		}
	}
	select {
	case <-child.done:
		return errGatewayNativeUnavailable
	default:
		return nil
	}
}
