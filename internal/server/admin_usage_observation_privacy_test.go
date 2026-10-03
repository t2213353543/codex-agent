package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManagedObservationExportPrivacy(t *testing.T) {
	secret := "Bearer synthetic-observation-secret"
	r := RequestRecord{SentModel: secret, ReturnedModel: secret, InboundPath: secret, UpstreamPath: secret, UsageSchemaSource: secret, Attempts: []GatewayAttemptSummary{{Attempt: 1, Phase: secret, Endpoint: secret, ErrorSource: secret, ErrorCode: secret}}}
	for _, redacted := range []bool{true, false} {
		out := managedExportRecord(r, redacted)
		raw, err := json.Marshal(out)
		if err != nil || strings.Contains(string(raw), "synthetic-observation-secret") {
			t.Fatal("observation export omitted secret filtering", err)
		}
		if out.Attempts[0].Attempt != 1 {
			t.Fatal("structural observation changed")
		}
	}
	if r.Attempts[0].ErrorCode != secret || r.SentModel != secret {
		t.Fatal("export mutated stored observations")
	}
}
