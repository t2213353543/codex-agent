package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func appServerRequestFixtureInput() appServerRequestInput {
	return appServerRequestInput{
		ModelMetadataRaw: json.RawMessage(`{"slug":"fixture-model","default_reasoning_level":"high","supported_reasoning_levels":[{"effort":"high"},{"effort":"max"}],"support_verbosity":true,"default_verbosity":"low","service_tiers":[{"id":"priority"}],"model_messages":{"instructions_template":"Be precise."}}`),
		ThreadID:         "00000000-0000-4000-8000-000000000001", SessionID: "session-fixture", Tools: json.RawMessage(`[]`),
		Input:          []json.RawMessage{json.RawMessage(`{"type":"message","id":"legacy","role":"user","content":[{"type":"input_text","text":"hello"}]}`)},
		ClientMetadata: map[string]string{"session_id": "session-fixture"}, Originator: "codex_agent_gateway",
		ParallelToolCalls: true, OutputSchemaStrict: true,
	}
}

func appServerRequestAssertJSON(t *testing.T, actual []byte, expected string) {
	t.Helper()
	decode := func(raw []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got, want := decode(actual), decode([]byte(expected)); !reflect.DeepEqual(got, want) {
		t.Fatalf("writer mismatch\ngot: %s\nwant: %s", actual, expected)
	}
}

// Source-derived fixture, not a claim of captured executable parity.
func TestAppServerRequestStandardWriter(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ServiceTier = "priority"
	in.OutputSchema = json.RawMessage(`{"type":"object","properties":{"n":{"const":9007199254740993}}}`)
	wire, err := buildAppServerRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, wire.Body, `{"model":"fixture-model","instructions":"Be precise.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high","summary":"auto"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"session-fixture","client_metadata":{"session_id":"session-fixture"},"service_tier":"priority","text":{"verbosity":"low","format":{"type":"json_schema","strict":true,"schema":{"type":"object","properties":{"n":{"const":9007199254740993}}},"name":"codex_output_schema"}}}`)
	wantHeaders := http.Header{"Accept": []string{"text/event-stream"}, "Session-Id": []string{"session-fixture"}, "Thread-Id": []string{in.ThreadID}, "X-Client-Request-Id": []string{in.ThreadID}, "Originator": []string{"codex_agent_gateway"}}
	if !reflect.DeepEqual(wire.Headers, wantHeaders) {
		t.Fatalf("headers: %#v", wire.Headers)
	}
	if !strings.Contains(string(in.Input[0]), `"legacy"`) {
		t.Fatal("writer mutated caller history")
	}
}

func TestAppServerRequestLitePrefixAndControls(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ModelMetadataRaw = json.RawMessage(`{"slug":"fixture-model","use_responses_lite":true,"default_reasoning_level":"high","supports_reasoning_summary_parameter":false,"support_verbosity":false,"model_messages":{"instructions_template":"Be precise."}}`)
	in.Verbosity = "high"
	in.ServiceTier = "priority"
	in.ConcurrentReasoningSummaries = true
	in.ContentItemKinds = true
	wire, err := buildAppServerRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	// Prefix IDs independently checked with Python uuid.uuid5(NAMESPACE_OID,
	// thread), then uuid.uuid5(namespace, "[]" / "Be precise.").
	appServerRequestAssertJSON(t, wire.Body, `{"model":"fixture-model","input":[{"type":"additional_tools","id":"at_85262007-dcd1-5e19-a57b-17635b617d32","role":"developer","tools":[]},{"type":"message","id":"msg_9c9d3036-37b2-53c0-a727-9e69c82bca3d","role":"developer","content":[{"type":"input_text","text":"Be precise."}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"high","context":"all_turns"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"session-fixture","client_metadata":{"session_id":"session-fixture"}}`)
	if wire.Headers.Get("x-openai-internal-codex-responses-lite") != "true" {
		t.Fatal("Lite header absent")
	}
}

func TestAppServerRequestEffortAndModelTemplate(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ModelMetadataRaw = json.RawMessage(`{"slug":"fixture-model","supported_reasoning_levels":[{"effort":"high"},{"effort":"max"},{"effort":"ultra"}],"multi_agent_reasoning_effort":"high","supports_reasoning_summary_parameter":true,"default_reasoning_summary":"detailed","model_messages":{"instructions_template":"<{{ personality }}>","instructions_variables":{"personality_default":"default","personality_friendly":"friendly"}}}`)
	in.Effort = "ultra"
	in.Personality = "friendly"
	in.ConcurrentReasoningSummaries = true
	wire, err := buildAppServerRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(wire.Body, &body)
	appServerRequestAssertJSON(t, body["reasoning"], `{"effort":"high","summary":"detailed"}`)
	appServerRequestAssertJSON(t, body["instructions"], `"<friendly>"`)
	appServerRequestAssertJSON(t, body["stream_options"], `{"reasoning_summary_delivery":"sequential_cutoff"}`)
	empty := ""
	in.Instructions = &empty
	in.Effort = "persistent"
	in.Summary = "none"
	wire, err = buildAppServerRequest(in)
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	_ = json.Unmarshal(wire.Body, &body)
	appServerRequestAssertJSON(t, body["reasoning"], `{"effort":"disabled"}`)
	if _, ok := body["instructions"]; ok {
		t.Fatal("explicit empty instruction override was not omitted")
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatal("summary disabled but stream options retained")
	}
}

func TestAppServerRequestInputOpaqueHistoryAndImages(t *testing.T) {
	raw := json.RawMessage(`{"type":"message","id":"msg_source","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,fixture","detail":"original"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"],"create_time":9007199254740993,"turn_id":"turn-source"}}`)
	for _, lite := range []bool{false, true} {
		got, err := appServerPrepareInputItem(raw, appServerRequestModel{UseResponsesLite: lite}, false)
		if err != nil {
			t.Fatal(err)
		}
		image := `{"type":"input_image","image_url":"data:image/png;base64,fixture","detail":"high"}`
		if lite {
			image = `{"type":"input_image","image_url":"data:image/png;base64,fixture"}`
		}
		appServerRequestAssertJSON(t, got, `{"type":"message","id":"msg_source","role":"user","content":[`+image+`],"internal_chat_message_metadata_passthrough":{"create_time":9007199254740993,"turn_id":"turn-source"}}`)
	}
	got, err := appServerPrepareInputItem(json.RawMessage(`{"type":"reasoning","id":"gateway-reasoning-stable","summary":[],"encrypted_content":"opaque=="}`), appServerRequestModel{}, false)
	if err != nil {
		t.Fatal(err)
	}
	appServerRequestAssertJSON(t, got, `{"type":"reasoning","summary":[],"encrypted_content":"opaque=="}`)
}

func TestAppServerRequestMetadataProjection(t *testing.T) {
	in := appServerRequestMetadata{InstallationID: "installation-fixture", SessionID: "session-fixture", ThreadID: "thread-fixture", WindowID: "window-fixture", TurnID: "turn-fixture", ParentThreadID: "parent-fixture", TurnMetadata: json.RawMessage(`{"request_kind":"turn","label":"中文😀","tool_namespaces_info":{"functions":{"name":"functions","functions":{}}},"turn_started_at_unix_ms":9007199254740993}`)}
	metadata, headers, err := projectAppServerRequestMetadata(in)
	if err != nil {
		t.Fatal(err)
	}
	if metadata["x-codex-turn-metadata"] != string(in.TurnMetadata) {
		t.Fatal("canonical body metadata changed")
	}
	if strings.Contains(headers.Get("x-codex-turn-metadata"), "tool_namespaces_info") {
		t.Fatal("tool inventory leaked into compatibility header")
	}
	for _, r := range headers.Get("x-codex-turn-metadata") {
		if r > 127 {
			t.Fatal("metadata compatibility header is not ASCII")
		}
	}
	appServerRequestAssertJSON(t, []byte(headers.Get("x-codex-turn-metadata")), `{"request_kind":"turn","label":"中文😀","turn_started_at_unix_ms":9007199254740993}`)
	if headers.Get("x-codex-parent-thread-id") != "parent-fixture" || metadata["turn_id"] != "turn-fixture" || metadata["x-codex-installation-id"] != "installation-fixture" {
		t.Fatal("identity projections differ")
	}
}

func TestAppServerRequestWebSocketContinuationProjection(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ModelMetadataRaw = json.RawMessage(`{"slug":"fixture-model","use_responses_lite":true}`)
	in.TurnState = "opaque-server-turn-state"
	prior := "resp_server"
	delta := []json.RawMessage{json.RawMessage(`{"type":"function_call_output","call_id":"call_server","output":"tool result"}`)}
	wire, err := buildAppServerWebSocketRequest(in, appServerWebSocketRequestState{PreviousResponseID: &prior, InputDelta: &delta, Warmup: true, StreamRequestStartMS: 1700000000000, RoutingHint: "server-hint", Traceparent: "00-placeholder"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(wire.Body, &body)
	appServerRequestAssertJSON(t, body["input"], `[{"type":"function_call_output","call_id":"call_server","output":"tool result"}]`)
	appServerRequestAssertJSON(t, body["client_metadata"], `{"session_id":"session-fixture","ws_request_header_x_openai_internal_codex_responses_lite":"true","x-codex-turn-state":"opaque-server-turn-state","ws_request_header_traceparent":"00-placeholder","x-codex-ws-stream-request-start-ms":"1700000000000"}`)
	if string(body["type"]) != `"response.create"` || string(body["previous_response_id"]) != `"resp_server"` || string(body["generate"]) != "false" {
		t.Fatalf("invalid WS controls: %s", wire.Body)
	}
	if wire.Headers.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" || wire.Headers.Get("x-codex-routing-hint") != "server-hint" {
		t.Fatal("WS handshake controls differ")
	}
	if wire.Headers.Get("x-codex-turn-state") != "" || wire.Headers.Get("x-openai-internal-codex-responses-lite") != "" || wire.Headers.Get("Accept") != "" {
		t.Fatal("HTTP-only controls retained in handshake")
	}
	if len(in.ClientMetadata) != 1 {
		t.Fatal("WS projection mutated caller metadata")
	}
	if _, err = buildAppServerWebSocketRequest(in, appServerWebSocketRequestState{PreviousResponseID: &prior}); err == nil {
		t.Fatal("accepted previous_response_id without validated delta")
	}
}

// These expected payloads came from the unmodified 0.155.0 executable's real
// egress. The source capture stays private; export allowlists safe headers and
// consistently renames identities. Context is captured input, not a builder
// expectation, so this verifies only the final writer/envelope boundary.
func TestAppServerRequestCapturedPilot(t *testing.T) {
	path := os.Getenv("APP_SERVER_WRITER_FIXTURE")
	if path == "" {
		path = "testdata/appserver_writer_pilot.json"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Fixtures []struct {
			Name            string                         `json:"name"`
			Request         appServerRequestInput          `json:"request"`
			SerializedTools string                         `json:"serialized_tools"`
			State           appServerWebSocketRequestState `json:"state"`
			Expected        json.RawMessage                `json:"expected"`
			ExpectedHeaders map[string]string              `json:"expected_headers"`
		} `json:"fixtures"`
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Fixtures) != 3 {
		t.Fatalf("expected prewarm+first+resume capture, got %d", len(capture.Fixtures))
	}
	for _, fixture := range capture.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			// Preserve exact official serde_json object insertion order for
			// UUID hashing; the pretty fixture's RawMessage adds whitespace.
			fixture.Request.Tools = json.RawMessage(fixture.SerializedTools)
			wire, err := buildAppServerWebSocketRequest(fixture.Request, fixture.State)
			if err != nil {
				t.Fatal(err)
			}
			if differences := appServerRequestJSONDifferences(wire.Body, fixture.Expected); len(differences) > 0 {
				t.Fatalf("captured body mismatches at %v", differences)
			}
			if fixture.ExpectedHeaders != nil {
				headers := map[string]string{}
				for name := range fixture.ExpectedHeaders {
					headers[name] = wire.Headers.Get(name)
				}
				for name, want := range fixture.ExpectedHeaders {
					if headers[name] != want {
						t.Errorf("captured handshake mismatch at %s", name)
					}
				}
			}
		})
	}
}

// Report only field paths when a private server-side fixture differs. Never
// print actual payloads or header values from that optional test input.
func appServerRequestJSONDifferences(actual, expected []byte) []string {
	decode := func(raw []byte) (any, error) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var value any
		err := d.Decode(&value)
		return value, err
	}
	got, err := decode(actual)
	if err != nil {
		return []string{"$invalid_actual_json"}
	}
	want, err := decode(expected)
	if err != nil {
		return []string{"$invalid_expected_json"}
	}
	var differences []string
	var compare func(string, any, any)
	compare = func(path string, a, b any) {
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			for k := range keys {
				av, ae := am[k]
				bv, be := bm[k]
				if ae != be {
					differences = append(differences, path+"."+k)
				} else {
					compare(path+"."+k, av, bv)
				}
			}
			return
		}
		aa, aok := a.([]any)
		ba, bok := b.([]any)
		if aok && bok {
			if len(aa) != len(ba) {
				differences = append(differences, path+".length")
				return
			}
			for i := range aa {
				compare(fmt.Sprintf("%s[%d]", path, i), aa[i], ba[i])
			}
			return
		}
		if !reflect.DeepEqual(a, b) {
			differences = append(differences, path)
		}
	}
	compare("$", got, want)
	sort.Strings(differences)
	return differences
}
