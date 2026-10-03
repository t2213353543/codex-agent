package server

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// Reference values are executed from Sub2API's original cost engine by
// tools/verify_account_window_parity.py, rather than reimplementing its formula.
func TestManagedAccountSub2APIExecutableParity(t *testing.T) {
	path := os.Getenv("CODEX_SUB2API_COST_REFERENCE")
	if path == "" {
		t.Skip("run python tools/verify_account_window_parity.py to execute the Sub2API reference")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Model       string  `json:"model"`
		Tier        string  `json:"tier"`
		Input       int64   `json:"input"`
		Cached      int64   `json:"cached"`
		Output      int64   `json:"output"`
		Write       int64   `json:"write"`
		ImageInput  int64   `json:"image_input"`
		ImageOutput int64   `json:"image_output"`
		Expected    float64 `json:"expected"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("insufficient reference coverage: %d", len(cases))
	}
	known := true
	for _, c := range cases {
		record := RequestRecord{SentModel: c.Model, ServiceTier: c.Tier, UsageKnown: &known,
			InputTokens: c.Input, CachedTokens: c.Cached, OutputTokens: c.Output,
			CacheWriteTokens: &c.Write, ImageInputTokens: &c.ImageInput, ImageOutputTokens: &c.ImageOutput}
		actual, ok := managedAccountRequestUSD(record)
		if !ok || math.Abs(actual-c.Expected) > 1e-12*math.Max(1, c.Expected) {
			t.Errorf("%s tier=%q input=%d cached=%d write=%d: got %.15g known=%v, Sub2API %.15g", c.Model, c.Tier, c.Input, c.Cached, c.Write, actual, ok, c.Expected)
		}
	}
}
