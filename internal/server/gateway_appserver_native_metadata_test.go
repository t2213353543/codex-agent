package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAppServerMirrorNativeHTTPBoundaryPreservesDelta(t *testing.T) {
	raw := []byte(`{"model":"fixture","previous_response_id":"resp_previous","input":[{"type":"function_call_output","call_id":"call_one","output":"opaque"}],"client_metadata":{"x-codex-turn-metadata":"{\"thread_source\":\"user\",\"tool_namespaces_info\":{\"functions\":[]},\"future\":9007199254740993}"}}`)
	client := &http.Client{Transport: appServerFailoverRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		var before, after map[string]json.RawMessage
		_ = json.Unmarshal(raw, &before)
		_ = json.Unmarshal(body, &after)
		for _, name := range []string{"input", "previous_response_id", "model"} {
			if !bytes.Equal(before[name], after[name]) {
				t.Fatalf("native %s changed", name)
			}
		}
		if bytes.Contains(body, []byte("thread_source")) || !bytes.Contains(body, []byte("tool_namespaces_info")) || strings.Contains(req.Header.Get("X-Codex-Turn-Metadata"), "tool_namespaces_info") {
			t.Fatal("wrong canonical/header projection")
		}
		if req.Header.Get("Authorization") != "Bearer current-account" {
			t.Fatal("account credential changed")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
	})}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example.test/responses", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer current-account")
	a := &appServerCLI{}
	res, err := a.RoundTrip(req, client, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
}

func TestAppServerMirrorNativeMetadataAbsentIsByteExact(t *testing.T) {
	raw := []byte(" { \"type\":\"response.create\", \"previous_response_id\":\"resp_existing\", \"input\": [], \"store\": true, \"stream\": false } \n")
	headers := http.Header{"x-client-custom": []string{"Exact"}, "Authorization": []string{"Bearer fixture"}}
	body, out, changed, err := appServerMirrorNativeMetadata(raw, headers)
	if err != nil || changed || !bytes.Equal(body, raw) || !reflect.DeepEqual(out, headers) {
		t.Fatalf("absent metadata changed: %v %v", changed, err)
	}
	out["x-client-custom"][0] = "changed"
	if headers["x-client-custom"][0] != "Exact" {
		t.Fatal("returned headers alias caller")
	}
}

func TestAppServerMirrorNativeMetadataHeaderOnly(t *testing.T) {
	raw := []byte(`{"previous_response_id":"resp_client","input":[],"client_metadata":{"opaque":{"large":9007199254740993}}}`)
	headers := http.Header{"x-codex-turn-metadata": []string{`{"thread_source":"user","turn_id":"turn","unknown":"中文","tool_namespaces_info":{"source":"kept-in-body-only"}}`}, "x-custom-fingerprint": []string{"unchanged"}}
	body, out, changed, err := appServerMirrorNativeMetadata(raw, headers)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !bytes.Equal(body, raw) {
		t.Fatal("header-only mirror rewrote body")
	}
	appServerRequestAssertJSON(t, []byte(out.Get("x-codex-turn-metadata")), `{"turn_id":"turn","unknown":"中文"}`)
	if out["x-custom-fingerprint"][0] != "unchanged" {
		t.Fatal("unrelated header modified")
	}
	if len(headers["x-codex-turn-metadata"]) == 0 {
		t.Fatal("caller headers mutated")
	}
}

func TestAppServerMirrorNativeMetadataBodyAuthorityAndOpaqueHistory(t *testing.T) {
	canonical := `{"thread_source":{"future":"source"},"turn_id":"body-turn","tool_namespaces_info":{"functions":{"name":"functions","functions":{}}},"unknown":{"integer":9007199254740993}}`
	input := json.RawMessage(`[{"type":"reasoning","id":"rs_server","encrypted_content":"opaque==","summary":[]},{"type":"additional_tools","id":"at_server","role":"developer","tools":[]}]`)
	raw, _ := marshalGatewayRequestJSON(map[string]any{"type": "response.create", "previous_response_id": "resp_server", "input": input, "store": true, "stream": false, "generate": false, "client_metadata": map[string]any{"x-codex-turn-metadata": canonical, "x-codex-turn-state": "opaque-turnstate", "unknown": json.RawMessage(`{"integer":9007199254740993}`)}})
	for _, headers := range []http.Header{nil, {"X-Codex-Turn-Metadata": []string{`{"thread_source":"user","turn_id":"stale-header","header_only":"must-not-enter-body"}`}}} {
		body, out, changed, err := appServerMirrorNativeMetadata(raw, headers)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("native thread_source not mirrored")
		}
		var fields, prior map[string]json.RawMessage
		_ = json.Unmarshal(body, &fields)
		_ = json.Unmarshal(raw, &prior)
		for _, key := range []string{"type", "previous_response_id", "input", "store", "stream", "generate"} {
			if !bytes.Equal(fields[key], prior[key]) {
				t.Errorf("request field changed: %s", key)
			}
		}
		var metadata map[string]json.RawMessage
		_ = json.Unmarshal(fields["client_metadata"], &metadata)
		var actual string
		_ = json.Unmarshal(metadata["x-codex-turn-metadata"], &actual)
		appServerRequestAssertJSON(t, []byte(actual), `{"turn_id":"body-turn","tool_namespaces_info":{"functions":{"name":"functions","functions":{}}},"unknown":{"integer":9007199254740993}}`)
		appServerRequestAssertJSON(t, []byte(out.Get("x-codex-turn-metadata")), `{"turn_id":"body-turn","unknown":{"integer":9007199254740993}}`)
		if string(metadata["x-codex-turn-state"]) != `"opaque-turnstate"` {
			t.Fatal("turnstate changed")
		}
		if string(metadata["unknown"]) != `{"integer":9007199254740993}` {
			t.Fatal("unknown metadata changed")
		}
	}
}

func TestAppServerMirrorNativeMetadataCapturedThreadSourceDifference(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_native_metadata_pair.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Routes map[string]json.RawMessage `json:"routes"`
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	actualCLI, wantApp := capture.Routes["native_codex_exec"], capture.Routes["native_app_server"]
	if differences := appServerRequestJSONDifferences(actualCLI, wantApp); !reflect.DeepEqual(differences, []string{"$.thread_source"}) {
		t.Fatalf("unexpected paired-capture difference: %v", differences)
	}
	body, _ := marshalGatewayRequestJSON(map[string]any{"client_metadata": map[string]string{"x-codex-turn-metadata": string(actualCLI)}, "input": []any{}})
	got, headers, changed, err := appServerMirrorNativeMetadata(body, nil)
	if err != nil || !changed {
		t.Fatalf("mirror failed: %v", err)
	}
	var output struct {
		ClientMetadata map[string]string `json:"client_metadata"`
	}
	_ = json.Unmarshal(got, &output)
	appServerRequestAssertJSON(t, []byte(output.ClientMetadata["x-codex-turn-metadata"]), string(wantApp))
	appServerRequestAssertJSON(t, []byte(headers.Get("x-codex-turn-metadata")), string(wantApp))
}
