package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAppServerAuxiliaryOfficialWriterVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_auxiliary_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Vectors []struct {
			Name     string          `json:"name"`
			Path     string          `json:"provider_relative_path"`
			Method   string          `json:"method"`
			Body     json.RawMessage `json:"body"`
			Response json.RawMessage `json:"expected_response"`
		}
	}
	if err = json.Unmarshal(raw, &fixture); err != nil || len(fixture.Vectors) != 5 {
		t.Fatalf("invalid captured vectors: %v", err)
	}
	for _, v := range fixture.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			p := appServerAuxiliaryTestProxy(t)
			p.Client = &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Method != v.Method || req.URL.Path != "/backend-api/codex/"+strings.TrimLeft(v.Path, "/") {
					t.Fatalf("wrong auxiliary route: %s %s", req.Method, req.URL.Path)
				}
				actual, _ := io.ReadAll(req.Body)
				if !bytes.Equal(actual, v.Body) {
					t.Fatal("official serialized request changed")
				}
				if req.Header.Get("Authorization") == "Bearer test-client-key" || req.Header.Get("X-Codex-Image-Turn-Id") != "fixture-image-turn" {
					t.Fatal("auxiliary identity header lost")
				}
				if req.Header.Get("Content-Encoding") != "" || gatewayNativeRequestCompression(req.Context()) {
					t.Fatal("unary auxiliary request gained zstd")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Codex-Imagegen-Request-Id": []string{"image-result-id"}}, Body: io.NopCloser(bytes.NewReader(v.Response)), Request: req}, nil
			})}
			req := appServerAuxiliaryTestRequest("/v1/"+strings.TrimLeft(v.Path, "/"), string(v.Body), "test-client-key")
			req.Header.Set("X-Codex-Image-Turn-Id", "fixture-image-turn")
			out := httptest.NewRecorder()
			p.ServeHTTP(out, req)
			if out.Code != 200 || !bytes.Equal(out.Body.Bytes(), v.Response) || out.Header().Get("X-Codex-Imagegen-Request-Id") != "image-result-id" {
				t.Fatalf("auxiliary response changed: %d", out.Code)
			}
		})
	}
}
