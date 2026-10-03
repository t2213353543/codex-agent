package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestBPSOAuthObservedHTTPRequiresHeadersAndClones(t *testing.T) {
	for _, reported := range []int{0, 99, 200, 400, 429, 600} {
		t.Run(itoa(int64(reported)), func(t *testing.T) {
			_, observer := withGatewayAttemptObservation(t.Context())
			h := observer.Begin("request", "/responses", time.Now())
			if reported != 0 {
				h.Headers(reported, time.Now())
			}
			h.Finish("transport", "transport", time.Now())
			logical := 503
			state := officialFailoverState{failure: errors.New("synthetic local error")}
			rec := RequestRecord{HTTPCode: logical, Status: "error"}
			state.captureActualAttempts(observer, &rec)
			state.recordAttempts(&rec, nil)
			if len(rec.Attempts) != 1 || rec.Attempts[0].StatusCode == nil || *rec.Attempts[0].StatusCode != logical {
				t.Fatalf("logical status lost: %+v", rec.Attempts)
			}
			a := rec.Attempts[0]
			valid := reported >= 100 && reported <= 599
			if valid && (a.ObservedHTTPStatusCode == nil || *a.ObservedHTTPStatusCode != reported) || !valid && a.ObservedHTTPStatusCode != nil {
				t.Fatalf("invented/lost observed HTTP: status=%d record=%+v", reported, a)
			}
			encoded, _ := json.Marshal(a)
			if !valid && bytes.Contains(encoded, []byte("observed_http_status_code")) {
				t.Fatalf("unknown observation did not stay omitted: %s", encoded)
			}
			if valid {
				*a.StatusCode = 599
				if *a.ObservedHTTPStatusCode != reported {
					t.Fatal("logical and observed pointers alias")
				}
				*a.ObservedHTTPStatusCode = 599
				again := observer.Snapshot().Attempts[0]
				if *again.StatusCode != reported || *again.ObservedHTTPStatusCode != reported {
					t.Fatal("snapshot mutation changed collector")
				}
			}
		})
	}
}

func TestBPSOAuthJSONDetailAndExportRoundTrip(t *testing.T) {
	logical, observed := 429, 200
	base := time.Now().Unix()
	rec := RequestRecord{EventID: "v3-logical-observed", Time: base, Model: "synthetic", Endpoint: "responses", Status: "error", HTTPCode: 200, ErrorSource: "upstream", ObservationSchemaVersion: 2, FinalUpstreamErrorCode: "rateLimitExceeded", FinalUpstreamErrorSource: "upstream", Attempts: []GatewayAttemptSummary{{Attempt: 1, Phase: "request", Endpoint: "/responses", StatusCode: &logical, ObservedHTTPStatusCode: &observed, ErrorSource: "upstream", ErrorCode: "rateLimitExceeded"}}}
	m, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.Record(rec)
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	stored, err := m.managedLogDetail(t.Context(), rec.EventID)
	if err != nil {
		t.Fatal(err)
	}
	assertAttempts := func(raw []byte) {
		t.Helper()
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		items, ok := value["attempts"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("attempts missing: %s", raw)
		}
		item := items[0].(map[string]any)
		if item["status_code"] != float64(429) || item["observed_http_status_code"] != float64(200) || item["error_code"] != "rateLimitExceeded" {
			t.Fatalf("roundtrip lost status contract: %s", raw)
		}
	}
	for _, projection := range []any{stored, managedRecord(stored), managedExportRecord(stored, false), managedExportRecord(stored, true)} {
		encoded, _ := json.Marshal(projection)
		assertAttempts(encoded)
		var envelope map[string]any
		_ = json.Unmarshal(encoded, &envelope)
		if envelope["final_upstream_error_code"] != "rateLimitExceeded" || envelope["final_upstream_error_source"] != "upstream" || envelope["observation_schema_version"] != float64(2) {
			t.Fatalf("selected outcome lost in projection: %s", encoded)
		}
	}
	q := managedLogQuery{From: base - 1, To: base + 1}
	for _, format := range []string{"jsonl", "csv"} {
		var output bytes.Buffer
		count, err := m.exportManagedLogsWithPrivacy(t.Context(), q, format, &output, false)
		if err != nil || count != 1 {
			t.Fatalf("export=%s count=%d err=%v", format, count, err)
		}
		if format == "jsonl" {
			assertAttempts(bytes.TrimSpace(output.Bytes()))
			continue
		}
		rows, err := csv.NewReader(&output).ReadAll()
		if err != nil || len(rows) != 2 {
			t.Fatal(err)
		}
		for index, name := range rows[0] {
			if name == "attempts" {
				assertAttempts([]byte(`{"attempts":` + rows[1][index] + `}`))
			}
			if name == "final_upstream_error_code" && rows[1][index] != "rateLimitExceeded" {
				t.Fatal("CSV selected generation code missing")
			}
			if name == "final_upstream_error_source" && rows[1][index] != "upstream" {
				t.Fatal("CSV selected generation source missing")
			}
		}
	}
	legacy, err := json.Marshal(GatewayAttemptSummary{Attempt: 1, Phase: "request", StatusCode: &logical})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacy, []byte("observed_http_status_code")) {
		t.Fatal("legacy missing transport status invented")
	}
}
