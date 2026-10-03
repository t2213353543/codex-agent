package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

// Official runtime requests keep their original JSON and compression semantics
// without the legacy compatibility gateway's application payload ceiling.
func readOfficialBody(r *http.Request) ([]byte, error) {
	var reader io.Reader = gatewayRequestContextReader{ctx: r.Context(), reader: r.Body}
	switch gatewayRequestContentEncoding(r.Header) {
	case "", "identity":
	case "zstd":
		decoder, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<63), zstd.WithDecoderMaxWindow((1<<41)+7*(1<<38)))
		if err != nil {
			return nil, errors.New("invalid zstd request body")
		}
		defer decoder.Close()
		reader = decoder
	default:
		return nil, errUnsupportedContentEncoding
	}
	body, err := io.ReadAll(gatewayRequestContextReader{ctx: r.Context(), reader: reader})
	if canceled := r.Context().Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, errors.New("cannot read request")
	}
	return body, nil
}

func officialRequestUnbounded(r *http.Request) bool {
	return r != nil && (requestInferenceMode(r.Context()) == inferenceAppServer || requestInferenceMode(r.Context()) == inferenceCodexCLI)
}
