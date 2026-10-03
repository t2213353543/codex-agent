package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestOfficialLargePlainAndCompressedRequestsPreserveContent(t *testing.T) {
	body := []byte(`{"model":"fixture","input":"` + strings.Repeat("a", maxBody+1) + `"}`)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll(body, nil)
	for _, encoding := range []string{"identity", "zstd"} {
		wire := body
		if encoding == "zstd" {
			wire = compressed
		}
		req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(wire))
		req.Header.Set("Content-Encoding", encoding)
		req = req.WithContext(withInferenceMode(req.Context(), inferenceAppServer))
		got, err := readOfficialBody(req)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("large %s input changed or rejected: %v", encoding, err)
		}
		got, _, err = applyGatewayPolicy(req, got, "fixture", Settings{})
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("large official policy changed/rejected input: %v", err)
		}
	}
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	if _, _, err = applyGatewayPolicy(request, body, "fixture", Settings{}); err == nil {
		t.Fatal("legacy policy ceiling unexpectedly changed")
	}
}

func TestOfficialRequestReadCancellationAndEncodingValidation(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	request.Header.Set("Content-Encoding", "gzip, zstd")
	if _, err := readOfficialBody(request); err != errUnsupportedContentEncoding {
		t.Fatal("stacked encoding accepted")
	}
	request.Header.Del("Content-Encoding")
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	if _, err := readOfficialBody(request.WithContext(ctx)); err != context.Canceled {
		t.Fatal("canceled large read was not canceled")
	}
}

func TestOfficialWebSocketWriterHasNoLegacyEventSizeCeiling(t *testing.T) {
	payload := []byte(`data: {"type":"response.output_text.delta","delta":"` + strings.Repeat("x", maxBody+1) + `"}` + "\n\n")
	count := 0
	writer := &bpsWSResponseWriter{header: make(http.Header), unbounded: true, send: func(raw []byte) error { count = len(raw); return nil }}
	if n, err := io.Copy(writer, bytes.NewReader(payload)); err != nil || n != int64(len(payload)) || count <= maxBody {
		t.Fatalf("large websocket event rejected or truncated: %d %d %v", n, count, err)
	}
}
