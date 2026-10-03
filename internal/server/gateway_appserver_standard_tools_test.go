package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppServerStandardToolsProfileKeepsCatalogAndClientTools(t *testing.T) {
	for _, webType := range []string{"web_search", "web_search_preview"} {
		t.Run(webType, func(t *testing.T) {
			model := json.RawMessage(appServerCLITestModel)
			original := append([]byte(nil), model...)
			raw := []byte(`{"model":"fixture","instructions":"CLIENT INSTRUCTIONS","input":"search now","tools":[{"type":"` + webType + `","external_web_access":true},{"type":"function","name":"inspect","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}],"tool_choice":{"type":"function","name":"inspect"},"reasoning":{"effort":"xhigh"}}`)
			in, _, err := prepareAppServerCLI(raw, http.Header{"Session-Id": []string{"caller-session"}}, model, "install")
			if err != nil {
				t.Fatal(err)
			}
			if !in.StandardTools || !bytes.Equal(original, in.ModelMetadataRaw) || !bytes.Equal(original, model) {
				t.Fatal("profile modified model metadata or was not selected")
			}
			wire, err := buildAppServerRequest(in)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			_ = json.Unmarshal(wire.Body, &body)
			appServerRequestAssertJSON(t, body["instructions"], `"CLIENT INSTRUCTIONS"`)
			appServerRequestAssertJSON(t, body["input"], `[{"type":"message","role":"user","content":[{"type":"input_text","text":"search now"}]}]`)
			appServerRequestAssertJSON(t, body["reasoning"], `{"effort":"xhigh"}`)
			appServerRequestAssertJSON(t, body["tool_choice"], `{"type":"function","name":"inspect"}`)
			var tools []struct{ Type, Name string }
			if err = json.Unmarshal(body["tools"], &tools); err != nil {
				t.Fatal(err)
			}
			if len(tools) != 3 || tools[0].Type != "web_search" || tools[1].Type != "function" || tools[1].Name != "inspect" || tools[2].Type != "custom" || tools[2].Name != "patch" {
				t.Fatalf("client tools were lost or Lite-grouped: %s", body["tools"])
			}
			if wire.Headers.Get("x-openai-internal-codex-responses-lite") != "" {
				t.Fatal("standard profile has Lite header")
			}
			// Explicit caller context is preserved; only the Lite-derived default
			// all_turns is absent in the standard profile.
			in.ClientControls["reasoning"] = json.RawMessage(`{"effort":"xhigh","context":"current_turn"}`)
			in.ClientMetadata["ws_request_header_x_openai_internal_codex_responses_lite"] = "true"
			ws, err := buildAppServerWebSocketRequest(in, appServerWebSocketRequestState{StreamRequestStartMS: 1})
			if err != nil {
				t.Fatal(err)
			}
			body = nil
			_ = json.Unmarshal(ws.Body, &body)
			appServerRequestAssertJSON(t, body["reasoning"], `{"effort":"xhigh","context":"current_turn"}`)
			if bytes.Contains(body["client_metadata"], []byte("ws_request_header_x_openai_internal_codex_responses_lite")) {
				t.Fatal("standard WS metadata retained Lite marker")
			}
		})
	}
}

func TestAppServerStandardToolsDispatchesOneHTTPNoWebSocket(t *testing.T) {
	gets, posts := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets++
			_, _ = io.WriteString(w, `{"models":[`+appServerCLITestModel+`]}`)
			return
		}
		posts++
		if r.Header.Get("Authorization") != "Bearer synthetic-token" || r.Header.Get("ChatGPT-Account-Id") != "synthetic-account" {
			t.Error("admitted credential lost")
		}
		if r.Header.Get("Content-Encoding") != "" || r.Header.Get("x-openai-internal-codex-responses-lite") != "" {
			t.Error("stale compression/Lite header")
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		if len(body["tools"]) == 0 || len(body["instructions"]) == 0 || bytes.Contains(body["input"], []byte("additional_tools")) {
			t.Error("not a standard tool request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_fixture\",\"status\":\"completed\",\"action\":{\"type\":\"search\",\"query\":\"Codex\"}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	a := &appServerCLI{installation: "install", catalog: map[[32]byte]appServerCatalogSnapshot{}, transport: &appServerTransport{dial: func(context.Context, http.Header, *ProxyEntry) (gatewayWSUpstream, *http.Response, error) {
		t.Fatal("standard built-in profile attempted WSS")
		return nil, nil, nil
	}}}
	raw := []byte(`{"model":"fixture","input":"search Codex","tools":[{"type":"web_search"}]}`)
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/backend-api/codex/responses", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer synthetic-token")
	request.Header.Set("ChatGPT-Account-Id", "synthetic-account")
	request.Header.Set("Content-Encoding", "zstd")
	response, err := a.RoundTrip(request, server.Client(), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	result, err := collectResponse(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if gets != 1 || posts != 1 || !bytes.Contains(result, []byte(`"web_search_call"`)) {
		t.Fatalf("wrong dispatch/output: gets=%d posts=%d result=%s", gets, posts, result)
	}
}

func TestAppServerStandardToolsAlreadyBuiltLitePassesThrough(t *testing.T) {
	posts := 0
	raw := []byte(`{"model":"fixture","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]}],"tools":[{"type":"web_search"}]}`)
	client := &http.Client{Transport: appServerFailoverRoundTripper(func(r *http.Request) (*http.Response, error) {
		posts++
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, raw) {
			t.Error("already-built Lite request changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("fixture")), Header: http.Header{}}, nil
	})}
	a := &appServerCLI{}
	request, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", bytes.NewReader(raw))
	response, err := a.RoundTrip(request, client, nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if posts != 1 {
		t.Fatal("transparent request was rebuilt")
	}
}
