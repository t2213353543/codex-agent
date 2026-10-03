package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const maxCompressedBody = maxBody + (1 << 20)

// Keep only two small-request encoders between calls. Large bodies may grow
// the encoder's history buffers, so their encoders are never retained here.
const gatewayReusableCompressionBody = 1 << 20

var gatewayRequestEncoders = make(chan *zstd.Encoder, 2)

var errGatewayPreparedBodyLimit = errors.New("prepared request exceeds 32 MiB")
var errGatewayCompressedBodyLimit = errors.New("compressed request exceeds wire limit")
var errGatewayCompressionCapability = errors.New("native helper does not support request compression; upgrade the configured helper")

type gatewayNativeCompressionKey struct{}

func withGatewayNativeRequestCompression(ctx context.Context) context.Context {
	return context.WithValue(ctx, gatewayNativeCompressionKey{}, true)
}

func withoutGatewayNativeRequestCompression(ctx context.Context) context.Context {
	return context.WithValue(ctx, gatewayNativeCompressionKey{}, false)
}

func gatewayNativeRequestCompression(ctx context.Context) bool {
	value, _ := ctx.Value(gatewayNativeCompressionKey{}).(bool)
	return value
}

// Preserve the client's observed compression choice. Do not infer OAuth mode
// from user-agent or add compression to API-key, compact, models or WS calls.
// Codex 0.155 uses the same ResponsesClient for both Guardian inference routes.
func gatewayWantsRequestCompression(r *http.Request, path string) bool {
	if r.Method != http.MethodPost || gatewayRequestContentEncoding(r.Header) != "zstd" {
		return false
	}
	return path == "/responses" || path == "/guardian" || path == "/guardian-classifier"
}

// Header maps built by middleware need not have canonical key casing. Count
// every spelling so stacked encodings cannot be hidden behind a second key.
func gatewayRequestContentEncoding(header http.Header) string {
	var values []string
	for key, fields := range header {
		if strings.EqualFold(key, "Content-Encoding") {
			values = append(values, fields...)
		}
	}
	return strings.ToLower(strings.TrimSpace(strings.Join(values, ",")))
}

type gatewayRequestContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r gatewayRequestContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

func prepareGatewayRequestBody(p *Proxy, r *http.Request, path string, prepared []byte) ([]byte, bool, error) {
	if err := r.Context().Err(); err != nil {
		return nil, false, err
	}
	if len(prepared) > maxBody && !gatewayUnboundedPayload(r.Context()) {
		return nil, false, errGatewayPreparedBodyLimit
	}
	if !gatewayWantsRequestCompression(r, path) {
		return prepared, false, nil
	}
	if p.nativeHTTPFor(r.Context()) != nil {
		return prepared, true, nil
	}
	var encoder *zstd.Encoder
	reusable := len(prepared) <= gatewayReusableCompressionBody
	if reusable {
		select {
		case encoder = <-gatewayRequestEncoders:
		default:
		}
	}
	if encoder == nil {
		var err error
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(3)))
		if err != nil {
			return nil, false, errors.New("cannot prepare compressed request")
		}
	}
	defer func() {
		if reusable {
			select {
			case gatewayRequestEncoders <- encoder:
				return
			default:
			}
		}
		_ = encoder.Close()
	}()
	wire := encoder.EncodeAll(prepared, nil)
	if err := r.Context().Err(); err != nil {
		return nil, false, err
	}
	if len(wire) > maxCompressedBody && !gatewayUnboundedPayload(r.Context()) {
		return nil, false, errGatewayCompressedBodyLimit
	}
	return wire, false, nil
}
