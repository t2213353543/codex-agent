package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func officialRepairPtr[T any](value T) *T { return &value }

func officialRepairFixture(t *testing.T) (string, string, officialUsageRepairEntry, []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "session-fixture", "home", "sessions", "2026", "09", "27", "rollout-fixture.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	usage := officialRepairUsage{Input: officialRepairPtr(int64(100)), Output: officialRepairPtr(int64(10)), Cached: officialRepairPtr(int64(60)), Write: officialRepairPtr(int64(0)), Reasoning: officialRepairPtr(int64(4)), Total: officialRepairPtr(int64(110))}
	start, _ := time.Parse(time.RFC3339, "2026-09-27T06:15:10Z")
	observed := start.Add(3500 * time.Millisecond)
	line, _ := json.Marshal(map[string]any{"timestamp": observed, "type": "token_usage_record", "payload": map[string]any{"response_id": "resp_native_one", "usage": usage}})
	line = append(line, '\n')
	raw := append([]byte(`{"type":"turn_context","payload":{"model":"gpt-6-astra"}}`+"\n"+`{"type":"response_item","payload":{"type":"message","role":"assistant","id":"msg-proof","content":[{"type":"output_text","text":"PRIVATE PROMPT CONTENT"}]}}`+"\n"), line...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(line)
	entry := officialUsageRepairEntry{RecordID: 1, EventID: "repair-event-1", ResponseID: "resp_app_one", RequestTime: start, LatencyMs: 3000, UsageTime: observed, Usage: usage, Model: "gpt-6-astra", SourceLineSHA256: hex.EncodeToString(sum[:]), MatchedItemIDs: []string{"msg-proof"}, Rollout: path, Line: 3, NativeResponseID: "resp_native_one"}
	source := filepath.Join(root, "plan.json")
	writeOfficialRepairPlan(t, source, []officialUsageRepairEntry{entry})
	record := []byte(`{"event_id":"repair-event-1","response_id":"resp_app_one","request_type":"official_app_server","status":"ok","http_code":200,"time":1790489710,"latency_ms":3000,"latency_known":true,"ttft_ms":2900,"model":"gpt-6-astra","future_opaque":{"n":9007199254740993}}`)
	return root, source, entry, record
}

func writeOfficialRepairPlan(t *testing.T, path string, entries []officialUsageRepairEntry) {
	t.Helper()
	raw, _ := json.Marshal(officialUsageRepairPlan{Schema: 1, Mode: "read_only_recovery_evidence", Matches: entries})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialUsageRepairParserRequiresExactRolloutProof(t *testing.T) {
	root, source, entry, record := officialRepairFixture(t)
	plan, digest, err := readOfficialUsageRepairPlan(source)
	if err != nil || len(plan.Matches) != 1 || len(digest) != 64 {
		t.Fatalf("valid plan rejected: %v", err)
	}
	if err := verifyOfficialRepairRollout(root, entry); err != nil {
		t.Fatal(err)
	}
	patch, already, costKnown, err := officialRepairRecordPatch(record, entry)
	if err != nil || already || !costKnown || !bytes.Contains(patch, []byte(`"usage_schema_source":"official_rollout"`)) || bytes.Contains(patch, []byte("latency")) || bytes.Contains(patch, []byte("ttft")) || bytes.Contains(patch, []byte("PRIVATE")) {
		t.Fatalf("repair patch altered unrelated fields or missed cost: %s %v", patch, err)
	}
	for name, mutate := range map[string]func(*officialUsageRepairEntry){
		"line hash":       func(e *officialUsageRepairEntry) { e.SourceLineSHA256 = strings.Repeat("0", 64) },
		"native response": func(e *officialUsageRepairEntry) { e.NativeResponseID = "wrong" },
		"response items":  func(e *officialUsageRepairEntry) { e.MatchedItemIDs = []string{"unrelated"} },
		"duplicate items": func(e *officialUsageRepairEntry) { e.MatchedItemIDs = []string{"msg-proof", "msg-proof"} },
		"usage":           func(e *officialUsageRepairEntry) { e.Usage.Cached = officialRepairPtr(int64(59)) },
		"time":            func(e *officialUsageRepairEntry) { e.UsageTime = e.UsageTime.Add(time.Second) },
		"model":           func(e *officialUsageRepairEntry) { e.Model = "another-model" },
		"line":            func(e *officialUsageRepairEntry) { e.Line-- },
		"path":            func(e *officialUsageRepairEntry) { e.Rollout = filepath.Join(root, "plan.json") },
	} {
		t.Run(name, func(t *testing.T) {
			changed := entry
			mutate(&changed)
			if err := verifyOfficialRepairRollout(root, changed); err == nil {
				t.Fatal("unproven rollout match accepted")
			}
		})
	}
}

func TestOfficialUsageRepairPlanRejectsUnknownCountersAndAmbiguousRows(t *testing.T) {
	_, source, entry, _ := officialRepairFixture(t)
	for name, mutate := range map[string]func(*officialUsageRepairEntry){
		"missing counter":          func(e *officialUsageRepairEntry) { e.Usage.Cached = nil },
		"negative":                 func(e *officialUsageRepairEntry) { e.Usage.Output = officialRepairPtr(int64(-1)) },
		"inconsistent total":       func(e *officialUsageRepairEntry) { e.Usage.Total = officialRepairPtr(int64(109)) },
		"reasoning greater output": func(e *officialUsageRepairEntry) { e.Usage.Reasoning = officialRepairPtr(int64(11)) },
		"cache greater input":      func(e *officialUsageRepairEntry) { e.Usage.Cached = officialRepairPtr(int64(101)) },
		"outside request":          func(e *officialUsageRepairEntry) { e.UsageTime = e.RequestTime.Add(5 * time.Second) },
		"already known plan":       func(e *officialUsageRepairEntry) { e.KnownBefore = officialRepairPtr(true) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := entry
			mutate(&changed)
			writeOfficialRepairPlan(t, source, []officialUsageRepairEntry{changed})
			if _, _, err := readOfficialUsageRepairPlan(source); err == nil {
				t.Fatal("invalid recovery plan accepted")
			}
		})
	}
	writeOfficialRepairPlan(t, source, []officialUsageRepairEntry{entry, entry})
	if _, _, err := readOfficialUsageRepairPlan(source); err == nil {
		t.Fatal("duplicate repair rows accepted")
	}
}

func TestOfficialUsageRepairRecordRefusesKnownOrMismatchedObservations(t *testing.T) {
	_, _, entry, record := officialRepairFixture(t)
	for name, patch := range map[string]map[string]any{
		"wrong event": {"event_id": "other"}, "wrong response": {"response_id": "other"}, "wrong type": {"request_type": "http_sse"},
		"error": {"status": "error"}, "non-200": {"http_code": 502}, "latency changed": {"latency_ms": 3001},
		"existing tokens": {"input_tokens": 1}, "existing cost": {"window_cost_usd": 0.01},
		"partial usage":      {"reasoning_tokens": 99},
		"inconsistent known": {"usage_known": true, "input_tokens": 101},
	} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(record, &fields)
			for k, v := range patch {
				fields[k], _ = json.Marshal(v)
			}
			raw, _ := json.Marshal(fields)
			if _, _, _, err := officialRepairRecordPatch(raw, entry); err == nil {
				t.Fatal("conflicting record was overwritten")
			}
		})
	}
}

func insertOfficialRepairFixture(t *testing.T, db *Database, entry officialUsageRepairEntry, record []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `INSERT INTO usage_events(event_id,created_at,status,latency_ms,latency_known) VALUES($1,$2,'ok',$3,true)`, entry.EventID, entry.RequestTime, entry.LatencyMs); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `INSERT INTO request_logs(id,event_id,created_at,status,model,record) VALUES($1,$2,$3,'ok','gpt-6-astra',$4)`, entry.RecordID, entry.EventID, entry.RequestTime, record); err != nil {
		t.Fatal(err)
	}
}

func TestOfficialUsageRepairPGDryRunCommitIdempotenceAndPreservation(t *testing.T) {
	db := newMetricsTestDB(t)
	root, source, entry, record := officialRepairFixture(t)
	insertOfficialRepairFixture(t, db, entry, record)
	ctx := context.Background()
	get := func() (string, int64, int64, int64, int64) {
		var raw string
		var input, output, cached, latency int64
		if err := db.Pool.QueryRow(ctx, `SELECT record::text,input_tokens,output_tokens,cached_tokens,usage_events.latency_ms FROM request_logs JOIN usage_events USING(event_id) WHERE id=1`).Scan(&raw, &input, &output, &cached, &latency); err != nil {
			t.Fatal(err)
		}
		return raw, input, output, cached, latency
	}
	before, _, _, _, _ := get()
	dry, err := repairOfficialUsageWithRoot(ctx, db, source, root, true)
	if err != nil || dry.Repairable != 1 || dry.Updated != 0 || dry.CostKnown != 1 {
		t.Fatalf("dry run failed: %+v %v", dry, err)
	}
	after, input, _, _, _ := get()
	if before != after || input != 0 {
		t.Fatal("dry run changed data")
	}
	result, err := repairOfficialUsageWithRoot(ctx, db, source, root, false)
	if err != nil || result.Updated != 1 || !result.RebuildRequired {
		t.Fatalf("commit failed: %+v %v", result, err)
	}
	after, input, output, cached, latency := get()
	if input != 100 || output != 10 || cached != 60 || latency != 3000 || !strings.Contains(after, "9007199254740993") || !strings.Contains(after, `"ttft_ms": 2900`) || !strings.Contains(after, `"usage_known": true`) {
		t.Fatalf("data not preserved: %s %d/%d/%d/%d", after, input, output, cached, latency)
	}
	again, err := repairOfficialUsageWithRoot(ctx, db, source, root, false)
	if err != nil || again.Updated != 0 || again.AlreadyRepaired != 1 {
		t.Fatalf("second run not idempotent: %+v %v", again, err)
	}
	final, _, _, _, _ := get()
	if final != after {
		t.Fatal("idempotent rerun modified frozen values")
	}
	var aggregates int64
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_totals").Scan(&aggregates); err != nil || aggregates != 0 {
		t.Fatal("repair unexpectedly rebuilt aggregates")
	}
}

func TestOfficialUsageRepairPGRollsBackWholePlanOnConflict(t *testing.T) {
	db := newMetricsTestDB(t)
	root, source, first, record := officialRepairFixture(t)
	insertOfficialRepairFixture(t, db, first, record)
	second := first
	second.RecordID, second.EventID, second.ResponseID = 2, "repair-event-2", "resp_app_two"
	second.NativeResponseID = "resp_native_two"
	second.Rollout = filepath.Join(filepath.Dir(first.Rollout), "rollout-second.jsonl")
	data, _ := os.ReadFile(first.Rollout)
	data = bytes.ReplaceAll(data, []byte(first.NativeResponseID), []byte(second.NativeResponseID))
	line := bytes.SplitAfter(data, []byte("\n"))[second.Line-1]
	sum := sha256.Sum256(line)
	second.SourceLineSHA256 = hex.EncodeToString(sum[:])
	if err := os.WriteFile(second.Rollout, data, 0600); err != nil {
		t.Fatal(err)
	}
	// SQL/event identity matches but the second log carries a conflicting prior
	// observation. The earlier first-row UPDATE must be rolled back with it.
	other := bytes.ReplaceAll(bytes.ReplaceAll(record, []byte(first.EventID), []byte(second.EventID)), []byte(first.ResponseID), []byte(second.ResponseID))
	other = bytes.Replace(other, []byte(`"status":"ok"`), []byte(`"status":"ok","input_tokens":5`), 1)
	insertOfficialRepairFixture(t, db, second, other)
	writeOfficialRepairPlan(t, source, []officialUsageRepairEntry{first, second})
	if _, err := repairOfficialUsageWithRoot(context.Background(), db, source, root, false); err == nil {
		t.Fatal("conflicting second row was accepted")
	}
	var input int64
	var known bool
	if err := db.Pool.QueryRow(context.Background(), `SELECT input_tokens,COALESCE((record->>'usage_known')::boolean,false) FROM request_logs JOIN usage_events USING(event_id) WHERE id=1`).Scan(&input, &known); err != nil || input != 0 || known {
		t.Fatal("earlier row escaped transaction rollback", err)
	}
}
