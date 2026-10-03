package bps

import (
	"encoding/json"
	"testing"
)

func TestCatalogUsesAdvertisedModelsAndEfforts(t *testing.T) {
	s := []byte(`{"allowed":true,"model_catalog":{"default_model":"model-b","models":[{"id":"model-a","label":"A","description":"Desc","efforts":[{"value":"none","label":"Default"},{"value":"new-custom-effort","description":"Advertised future effort"}],"default_effort":"none"},{"id":"model-b","label":"B","efforts":[{"value":"xhigh","label":"Heavy"}],"default_effort":"xhigh"}]}}`)
	b, e := ModelsFromAccess(s)
	if e != nil {
		t.Fatal(e)
	}
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if e = json.Unmarshal(b, &envelope); e != nil {
		t.Fatal(e)
	}
	if len(envelope.Models) != 2 || string(envelope.Models[1]["priority"]) != "0" {
		t.Fatal(string(b))
	}
	var levels []map[string]string
	json.Unmarshal(envelope.Models[0]["supported_reasoning_levels"], &levels)
	if len(levels) != 2 || levels[1]["effort"] != "new-custom-effort" {
		t.Fatal("efforts were guessed")
	}
	for _, m := range envelope.Models {
		for _, key := range []string{"context_window", "max_context_window", "max_output_tokens"} {
			if _, ok := m[key]; ok {
				t.Fatalf("unsupported capability invented: %s", key)
			}
		}
		if stringField(m, "base_instructions") != DefaultClientInstructions {
			t.Fatal("missing Codex-required client instruction template")
		}
	}
}

func TestCatalogRejectsDeniedAndInvalidDefaults(t *testing.T) {
	for _, s := range []string{`{"allowed":false}`, `{"allowed":true,"model_catalog":{"models":[]}}`, `{"allowed":true,"model_catalog":{"models":[{"id":"a","default_effort":"ultra","efforts":[{"value":"high"}]}]}}`} {
		if _, e := ModelsFromAccess([]byte(s)); e == nil {
			t.Fatal("invalid catalog accepted", s)
		}
	}
}
