package server

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func appServerCapturedContextProfile(t *testing.T) appServerContextProfile {
	t.Helper()
	raw, err := os.ReadFile("testdata/appserver_context_model_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	return appServerContextProfile{ModelMetadataRaw: raw, SandboxMode: "read-only", ApprovalPolicy: "never", NetworkAccess: "restricted", CollaborationMode: "default", IncludeCollaborationMode: true, CustomModelCatalog: true, MultiAgentVersion: "v2", SelectedEffort: "xhigh", MaxConcurrentAgents: 4, WaitAgentEnabled: true, ExposeModelOverrides: true, CWD: "/fixture/workspace", Shell: "bash", CurrentDate: "2026-09-27", Timezone: "Asia/Shanghai", WorkspaceRoots: []string{"/fixture/workspace"}}
}

func TestAppServerContextMatchesExecutableCapture(t *testing.T) {
	profile := appServerCapturedContextProfile(t)
	fragments, err := buildAppServerInitialContext(profile)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/appserver_writer_pilot.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Fixtures []struct {
			Expected struct{ Input []json.RawMessage }
		}
	}
	if err = json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	expected := capture.Fixtures[1].Expected.Input[:4]
	identities := make([]appServerContextIdentity, len(expected))
	for i, item := range expected {
		var value struct {
			ID       string
			Metadata struct {
				TurnID     string      `json:"turn_id"`
				CreateTime json.Number `json:"create_time"`
			} `json:"internal_chat_message_metadata_passthrough"`
		}
		if err = json.Unmarshal(item, &value); err != nil {
			t.Fatal(err)
		}
		identities[i] = appServerContextIdentity{ID: value.ID, TurnID: value.Metadata.TurnID, CreateTime: value.Metadata.CreateTime}
	}
	actual, err := materializeAppServerContext(fragments, nil, identities)
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(expected) {
		t.Fatalf("context count: got %d expected %d", len(actual), len(expected))
	}
	for i := range actual {
		if diff := appServerRequestJSONDifferences(actual[i], expected[i]); len(diff) > 0 {
			t.Errorf("context message %d mismatches at %v", i, diff)
		}
	}
	// Client-owned continuation already contains exactly these context items;
	// the adapter must not re-inject them or rewrite their IDs and metadata.
	retained := append([]json.RawMessage(nil), expected...)
	again, err := materializeAppServerContext(fragments, retained, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("retained context duplicated: additions %d err %v", len(again), err)
	}
	if !reflect.DeepEqual(retained, expected) {
		t.Fatal("adapter mutated client history")
	}
	// A writer with content-item classifications disabled still retains exact
	// visible context. Missing classifications must not duplicate that context.
	withoutKinds := make([]json.RawMessage, 0, len(retained))
	for _, item := range retained {
		normalized, err := appServerPrepareInputItem(item, appServerRequestModel{}, false)
		if err != nil {
			t.Fatal(err)
		}
		withoutKinds = append(withoutKinds, normalized)
	}
	again, err = materializeAppServerContext(fragments, withoutKinds, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("unclassified retained context duplicated: additions %d err %v", len(again), err)
	}
}

func TestAppServerContextCatalogEmptyOverrides(t *testing.T) {
	p := appServerCapturedContextProfile(t)
	p.ModelMetadataRaw = json.RawMessage(`{"slug":"fixture","multi_agent_version":"v2","model_messages":{"permissions":{"read_only":"catalog {{ network_access }}"},"approvals":{"never":""},"collaboration_modes":{"default":""},"multi_agent":{"role":{"root":""},"mode":{"hint_text":""}}}}`)
	p.NetworkAccess = "enabled"
	fragments, err := buildAppServerInitialContext(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != 2 {
		t.Fatalf("empty catalog role/mode must suppress fallback: %d", len(fragments))
	}
	expected := []appServerContextPart{{Kind: "permissions.instructions", Text: "<permissions instructions>\ncatalog enabled\n</permissions instructions>"}, {Kind: "collaboration_mode.instructions", Text: "<collaboration_mode></collaboration_mode>"}}
	if !reflect.DeepEqual(fragments[0].Parts, expected) {
		t.Fatalf("catalog override mismatch: %#v", fragments[0].Parts)
	}
}
