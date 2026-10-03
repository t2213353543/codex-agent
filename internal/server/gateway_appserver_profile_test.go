package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAppServerProfileOpaqueEnvironmentCannotRejectConversation(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.Input = []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>a & b</cwd></environment_context>"}]}`)}
	result, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in})
	if err != nil || result.Environment != nil || !bytes.Equal(result.Request.Input[0], in.Input[0]) {
		t.Fatalf("opaque client context modified/rejected: %v", err)
	}
	if !strings.Contains(strings.Join(result.Missing, ","), "client_environment.parseable_xml") {
		t.Fatal("unproven environment not recorded")
	}
}

func TestAppServerProfileCapturedMetadataReconstruction(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_writer_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Fixtures []struct {
			Request  appServerRequestInput
			Expected struct {
				ClientMetadata map[string]string `json:"client_metadata"`
			}
		}
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	f := capture.Fixtures[1]
	want := map[string]string{}
	for key, value := range f.Request.ClientMetadata {
		want[key] = value
	}
	var payload map[string]json.RawMessage
	if err = json.Unmarshal([]byte(f.Request.ClientMetadata["x-codex-turn-metadata"]), &payload); err != nil {
		t.Fatal(err)
	}
	// Remove fields whose real sources are flat client metadata + window ID.
	// Preserve original request operation and actual environment/review data.
	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id", "window_number", "root_turn_id"} {
		delete(payload, key)
	}
	partial, _ := marshalGatewayRequestJSON(payload)
	f.Request.ClientMetadata["x-codex-turn-metadata"] = string(partial)
	f.Request.ExtraHeaders = nil
	result, err := resolveAppServerRequestProfile(appServerProfileInput{Request: f.Request})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if key == "x-codex-turn-metadata" {
			if diff := appServerRequestJSONDifferences([]byte(result.Request.ClientMetadata[key]), []byte(value)); len(diff) > 0 {
				t.Fatalf("captured turn metadata mismatch: %v", diff)
			}
		} else if result.Request.ClientMetadata[key] != value {
			t.Errorf("captured flat field mismatch at %s", key)
		}
	}
	if len(result.Missing) != 0 {
		t.Fatalf("complete captured metadata has missing fields: %v", result.Missing)
	}
	if result.Sources["window_number"] != "window_id" {
		t.Fatal("window counter was not derived from captured window id")
	}
}

func TestAppServerProfilePreservesUnknownAndScopedIdentity(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.Input = []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque==","summary":[]}`), json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"new question"}]} `)}
	start := 1
	now := int64(1700000000123)
	window := uint64(3)
	in.ClientMetadata = map[string]string{"custom_client_field": "exact", "thread_source": "unknown-flat-source", "x-codex-turn-metadata": `{"thread_source":{"future":"opaque-source"},"custom_payload":{"exact":9007199254740993},"tool_namespaces_info":{"functions":{"name":"functions","functions":{}}}}`}
	in.ContentItemKinds = true
	before := append([]byte(nil), in.Input[0]...)
	result, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in, Defaults: appServerProfileDefaults{InstallationID: "installation", TurnID: "new-turn", WindowNumber: &window, ContextWindowID: "window-uuid", TurnStartedAtUnixMS: &now, CreateTime: json.Number("1700000000.1234567")}, NewInputStart: &start, NewItemIDs: map[int]string{1: "msg_supplied-new-id"}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, result.Request.Input[0]) {
		t.Fatal("opaque reasoning history changed")
	}
	var turn map[string]json.RawMessage
	_ = json.Unmarshal([]byte(result.Request.ClientMetadata["x-codex-turn-metadata"]), &turn)
	appServerRequestAssertJSON(t, turn["thread_source"], `{"future":"opaque-source"}`)
	appServerRequestAssertJSON(t, turn["custom_payload"], `{"exact":9007199254740993}`)
	if result.Request.ClientMetadata["thread_source"] != "unknown-flat-source" || result.Request.ClientMetadata["custom_client_field"] != "exact" {
		t.Fatal("unknown flat metadata removed")
	}
	if bytes.Contains([]byte(result.Request.ExtraHeaders.Get("x-codex-turn-metadata")), []byte("tool_namespaces_info")) {
		t.Fatal("tool inventory duplicated into header")
	}
	appServerRequestAssertJSON(t, result.Request.Input[1], `{"type":"message","id":"msg_supplied-new-id","role":"user","content":[{"type":"input_text","text":"new question"}],"internal_chat_message_metadata_passthrough":{"turn_id":"new-turn","create_time":1700000000.1234567,"content_item_kinds":["user.text"]}}`)
	if result.Request.ClientMetadata["x-codex-window-id"] != in.ThreadID+":3" {
		t.Fatal("explicit window not projected")
	}
	if !strings.Contains(strings.Join(result.Missing, ","), "sandbox") {
		t.Fatal("unknown execution state was silently invented")
	}
	if bytes.Contains([]byte(result.Request.ClientMetadata["x-codex-turn-metadata"]), []byte(`"agent_name":"/root"`)) {
		t.Fatal("invented root agent capability")
	}
	if !bytes.Equal(in.Input[0], before) || bytes.Contains(in.Input[1], []byte("msg_supplied")) {
		t.Fatal("resolver mutated caller input")
	}
}

func TestAppServerProfileHistoryBoundaryAndMirrorAreExplicit(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.Input = []json.RawMessage{json.RawMessage(`{"type":"compaction","encrypted_content":"opaque"}`), json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}`)}
	in.ClientMetadata = map[string]string{"x-codex-turn-metadata": `{"thread_source":"user"}`}
	unknown, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in, Defaults: appServerProfileDefaults{TurnID: "new-request-turn"}})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Request.ClientMetadata["x-codex-window-id"] != "" {
		t.Fatal("truncated history invented a window counter")
	}
	if !reflect.DeepEqual(unknown.Request.Input, in.Input) {
		t.Fatal("unspecified boundary rewrote history IDs")
	}
	if !strings.Contains(strings.Join(unknown.Missing, ","), "input.item_id") {
		t.Fatal("missing history IDs not recorded")
	}
	exact, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in, CompleteHistory: true, MirrorAppServer: true})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Request.ClientMetadata["x-codex-window-id"] != in.ThreadID+":1" {
		t.Fatal("complete history compaction counter not used")
	}
	if strings.Contains(exact.Request.ClientMetadata["x-codex-turn-metadata"], "thread_source") {
		t.Fatal("explicit mirror profile did not omit thread_source")
	}
	if !strings.Contains(unknown.Request.ClientMetadata["x-codex-turn-metadata"], `"thread_source":"user"`) {
		t.Fatal("ordinary profile silently dropped thread_source")
	}
}

func TestAppServerProfileEnvironmentRequiresActualClientPolicy(t *testing.T) {
	in := appServerRequestFixtureInput()
	env := `<environment_context>\n  <cwd>/client/project &amp; assets</cwd>\n  <shell>zsh</shell>\n  <current_date>2026-09-27</current_date>\n  <timezone>Asia/Shanghai</timezone>\n  <filesystem><workspace_roots><root>/client/project &amp; assets</root></workspace_roots><permission_profile type="managed"><file_system type="restricted"><entry access="read"><special>:root</special></entry></file_system></permission_profile></filesystem>\n</environment_context>`
	item, _ := marshalGatewayRequestJSON(map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"type": "input_text", "text": env}}})
	in.Input = []json.RawMessage{item}
	in.ModelMetadataRaw = json.RawMessage(`{"slug":"fixture","node_repl_disabled":true,"node_repl_auto_review_required":false}`)
	result, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in})
	if err != nil {
		t.Fatal(err)
	}
	if result.Environment == nil || result.Environment.CWD != "/client/project & assets" || result.Environment.SandboxMode != "read-only" {
		t.Fatalf("client environment not resolved: %+v", result.Environment)
	}
	if _, err = result.Environment.contextProfile(appServerContextProfile{}); err == nil {
		t.Fatal("unknown approval/network policy invented")
	}
	profile, err := result.Environment.contextProfile(appServerContextProfile{ApprovalPolicy: "never", NetworkAccess: "restricted"})
	if err != nil {
		t.Fatal(err)
	}
	if profile.CWD != result.Environment.CWD || profile.MultiAgentVersion != "" {
		t.Fatal("context profile invented server environment or agents")
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal([]byte(result.Request.ClientMetadata["x-codex-turn-metadata"]), &meta)
	if string(meta["sandbox_mode"]) != `"read-only"` || string(meta["node_repl_disabled"]) != "true" || string(meta["node_repl_auto_review_required"]) != "false" {
		t.Fatal("derivable model/environment metadata missing")
	}
	if _, exists := meta["sandbox"]; exists {
		t.Fatal("sandbox engine inferred from read-only policy")
	}
}

func TestAppServerProfileRejectsConflictingClientIdentity(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ClientMetadata = map[string]string{"thread_id": "flat", "x-codex-turn-metadata": `{"thread_id":"canonical"}`}
	in.ExtraHeaders = http.Header{}
	if _, err := resolveAppServerRequestProfile(appServerProfileInput{Request: in}); err == nil {
		t.Fatal("inconsistent client identity silently overwritten")
	}
}

func TestAppServerPrewarmProfileMatchesCapturedPhase(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_writer_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Fixtures []struct{ Request appServerRequestInput }
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	generation := capture.Fixtures[1].Request
	want := capture.Fixtures[0].Request
	before, _ := marshalGatewayRequestJSON(generation)
	warm, err := appServerPrewarmProfile(generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := warm.ClientMetadata["turn_id"]; !present || warm.ClientMetadata["turn_id"] != "" {
		t.Fatal("prewarm turn_id must be explicitly empty")
	}
	for key, value := range want.ClientMetadata {
		if key == "x-codex-turn-metadata" {
			if differences := appServerRequestJSONDifferences([]byte(warm.ClientMetadata[key]), []byte(value)); len(differences) > 0 {
				t.Fatalf("actual prewarm metadata mismatch: %v", differences)
			}
		} else if warm.ClientMetadata[key] != value {
			t.Errorf("actual prewarm flat metadata differs at %s", key)
		}
	}
	if len(warm.ClientMetadata) != len(want.ClientMetadata) {
		t.Fatalf("unexpected prewarm flat fields: %v", warm.ClientMetadata)
	}
	after, _ := marshalGatewayRequestJSON(generation)
	if !bytes.Equal(before, after) {
		t.Fatal("prewarm projection modified generation metadata")
	}
	if !bytes.Equal(warm.ModelMetadataRaw, generation.ModelMetadataRaw) || !bytes.Equal(warm.Tools, generation.Tools) || !reflect.DeepEqual(warm.Input, generation.Input) {
		t.Fatal("metadata projection changed model/tools/input")
	}
	var body, header map[string]json.RawMessage
	_ = json.Unmarshal([]byte(warm.ClientMetadata["x-codex-turn-metadata"]), &body)
	_ = json.Unmarshal([]byte(warm.ExtraHeaders.Get("x-codex-turn-metadata")), &header)
	if !reflect.DeepEqual(body, header) {
		t.Fatal("prewarm header projection differs without tool inventory")
	}
}

func TestAppServerPrewarmProfileRetainsToolSnapshotAndUnknownFields(t *testing.T) {
	in := appServerRequestFixtureInput()
	in.ClientMetadata = map[string]string{"turn_id": "caller-turn", "root_turn_id": "caller-root", "caller_extension": "unchanged", "x-codex-turn-metadata": `{"request_kind":"turn","turn_id":"caller-turn","root_turn_id":"caller-root","turn_started_at_unix_ms":1700000000000,"window_id":"thread:4","tool_namespaces_info":{"functions":{"name":"functions","functions":{}}},"thread_source":{"future":"value"},"unknown":{"large":9007199254740993}}`}
	in.ExtraHeaders = http.Header{"X-Codex-Window-Id": []string{"thread:4"}}
	warm, err := appServerPrewarmProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(warm.ClientMetadata["x-codex-turn-metadata"]), &body)
	if len(body["tool_namespaces_info"]) == 0 || len(body["thread_source"]) == 0 || len(body["unknown"]) == 0 {
		t.Fatal("prewarm discarded tool snapshot or client extension")
	}
	if _, exists := body["sandbox"]; exists {
		t.Fatal("prewarm invented execution environment")
	}
	if bytes.Contains([]byte(warm.ExtraHeaders.Get("x-codex-turn-metadata")), []byte("tool_namespaces_info")) {
		t.Fatal("prewarm copied unbounded tool inventory into header")
	}
	warm.ClientMetadata["caller_extension"] = "changed"
	warm.ExtraHeaders.Set("x-codex-window-id", "changed")
	if in.ClientMetadata["caller_extension"] != "unchanged" || in.ExtraHeaders.Get("x-codex-window-id") != "thread:4" {
		t.Fatal("prewarm aliases generation metadata/header maps")
	}
	in.ClientMetadata["x-codex-turn-metadata"] = "invalid-json"
	if _, err = appServerPrewarmProfile(in); err == nil {
		t.Fatal("invalid canonical prewarm metadata accepted")
	}
}
