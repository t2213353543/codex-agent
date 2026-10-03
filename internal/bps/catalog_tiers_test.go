package bps

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCatalogExplicitEmptyServiceTiersPreserveAdvertisedModels(t *testing.T) {
	input := []byte(`{"allowed":true,"model_catalog":{"default_model":"gpt-6-astra","models":[{"id":"gpt-5.6-terra","efforts":[{"value":"none"},{"value":"low"},{"value":"medium"},{"value":"xhigh"}],"default_effort":"medium"},{"id":"gpt-6-astra","efforts":[{"value":"low"},{"value":"xhigh"}],"default_effort":"xhigh"}]}}`)
	body, err := ModelsFromAccess(input)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Models) != 2 || string(envelope.Models[0]["slug"]) != `"gpt-5.6-terra"` || string(envelope.Models[1]["priority"]) != "0" {
		t.Fatalf("upstream catalog identity/default changed: %s", body)
	}
	for _, model := range envelope.Models {
		for _, field := range []string{"service_tiers", "additional_speed_tiers"} {
			if !bytes.Equal(model[field], []byte("[]")) {
				t.Fatalf("%s must explicitly deny invented speed capabilities: %s", field, body)
			}
		}
		if _, exists := model["default_service_tier"]; exists {
			t.Fatal("a default service tier was invented")
		}
	}
	if string(envelope.Models[0]["default_reasoning_level"]) != `"medium"` || string(envelope.Models[1]["default_reasoning_level"]) != `"xhigh"` {
		t.Fatal("advertised reasoning defaults changed")
	}
	var efforts []struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(envelope.Models[0]["supported_reasoning_levels"], &efforts) != nil || len(efforts) != 4 || efforts[0].Effort != "none" || efforts[3].Effort != "xhigh" {
		t.Fatal("advertised effort choices changed")
	}
}
