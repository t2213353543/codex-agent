package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestAppServerTurnCostProjectionRequiresVisibleSettlement(t *testing.T) {
	mappings := map[string]appServerTurnCostMapping{"client-turn": {Owner: "owner", ThreadID: "upstream-thread", TurnID: "upstream-turn", ResponseIDs: []string{"r1", "r2"}}}
	for _, tc := range []struct{ name, micros, settled, want string }{
		{"missing", "null", `["r1","r2"]`, ""}, {"negative", "-1", `["r1","r2"]`, ""},
		{"missing-settlement", "1", "null", ""}, {"unsettled", "1", "[]", ""}, {"partial", "1", `["r1"]`, ""},
		{"wrong-response", "1", `["other","r1"]`, ""}, {"zero", "0", `["r1","r2"]`, "0.000000"},
		{"precision", "9223372036854775807", `["r1","r2"]`, "9223372036854.775807"},
		{"micros", "125001", `["r1","r2"]`, "0.125001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"threads":[{"thread_id":"upstream-thread","turns":[{"turn_id":"upstream-turn","model":"actual-model","estimated_usage_usd_micros":%s,"settled_response_ids":%s}]}]}`, tc.micros, tc.settled)
			raw, err := projectAppServerTurnCosts([]string{"client-turn", "unknown"}, mappings, []byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			var output struct {
				Turns []appServerProjectedTurnCost `json:"turns"`
			}
			if json.Unmarshal(raw, &output) != nil || len(output.Turns) != 2 {
				t.Fatal("invalid projection")
			}
			got := output.Turns[0]
			if tc.want == "" {
				if got.Status != "pending" || got.TotalUSD != nil || got.EventCount != nil {
					t.Fatalf("unsettled amount priced %+v", got)
				}
			} else if got.Status != "priced" || got.TotalUSD == nil || *got.TotalUSD != tc.want || got.EventCount == nil || *got.EventCount != 2 || got.Responses != nil || got.Model == nil || *got.Model != "actual-model" {
				t.Fatalf("incorrect exact projection %+v", got)
			}
			if output.Turns[1].Status != "pending" || output.Turns[1].TotalUSD != nil {
				t.Fatal("unknown turn invented zero cost")
			}
		})
	}
}

func TestAppServerTurnCostProjectionNeverMatchesOtherThreadOrAmbiguousRow(t *testing.T) {
	mapping := appServerTurnCostMapping{Owner: "owner", ThreadID: "correct-thread", TurnID: "same-turn", ResponseIDs: []string{"r1"}}
	row := `{"turn_id":"same-turn","estimated_usage_usd_micros":1250000,"settled_response_ids":["r1"]}`
	for _, backend := range []string{`{"threads":[{"thread_id":"wrong-thread","turns":[` + row + `]}]}`, `{"threads":[{"thread_id":"correct-thread","turns":[` + row + `,` + row + `]}]}`} {
		raw, err := projectAppServerTurnCosts([]string{"requested"}, map[string]appServerTurnCostMapping{"requested": mapping}, []byte(backend))
		if err != nil {
			t.Fatal(err)
		}
		var output struct {
			Turns []appServerProjectedTurnCost `json:"turns"`
		}
		_ = json.Unmarshal(raw, &output)
		if output.Turns[0].Status != "pending" || output.Turns[0].TotalUSD != nil {
			t.Fatal("ambiguous/foreign cost leaked")
		}
	}
	if _, err := projectAppServerTurnCosts([]string{"requested"}, nil, []byte(`{"threads":broken}`)); err == nil {
		t.Fatal("malformed backend accepted")
	}
}

func testAppServerTurnCostLedger(t *testing.T, ledger *appServerTurnCostLedger, other *appServerTurnCostLedger) {
	t.Helper()
	ctx := t.Context()
	base := appServerTurnCostMapping{Owner: "owner", ThreadID: "upstream-thread", TurnID: "upstream-turn"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			value := base
			value.ResponseIDs = []string{fmt.Sprint("r", index%5)}
			if err := ledger.Record(ctx, "key-a", "turn", value); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	value, ok, err := other.Lookup(ctx, "key-a", "turn")
	if err != nil || !ok || !reflect.DeepEqual(value.ResponseIDs, []string{"r0", "r1", "r2", "r3", "r4"}) {
		t.Fatalf("atomic merge %v %v %+v", ok, err, value)
	}
	value.ResponseIDs[0] = "mutated-client-copy"
	again, _, _ := other.Lookup(ctx, "key-a", "turn")
	if again.ResponseIDs[0] != "r0" {
		t.Fatal("caller mutated ledger memory")
	}
	for _, name := range []string{"owner", "thread", "turn"} {
		conflict := base
		switch name {
		case "owner":
			conflict.Owner = "other"
		case "thread":
			conflict.ThreadID = "other"
		case "turn":
			conflict.TurnID = "other"
		}
		if err := ledger.Record(ctx, "key-a", "turn", conflict); !errors.Is(err, errAppServerTurnCostConflict) {
			t.Fatalf("identity conflict accepted %s %v", name, err)
		}
	}
	if _, ok, err := other.Lookup(ctx, "key-b", "turn"); err != nil || ok {
		t.Fatal("cross-key provenance leaked")
	}
	if _, known, err := other.Lookup(ctx, "key-a", "turn"); err != nil || known {
		t.Fatal("conflicting turn continued exposing old billing provenance")
	}
	if err := ledger.Record(ctx, "key-a", "turn", base); !errors.Is(err, errAppServerTurnCostConflict) {
		t.Fatal("old identity revived tombstoned turn")
	}
}

func TestAppServerTurnCostMemoryLedgerIsolationAndExpiry(t *testing.T) {
	ledger := newAppServerTurnCostLedger(nil)
	testAppServerTurnCostLedger(t, ledger, ledger)
	id, _ := appServerTurnCostIdentity("key-a", "turn")
	ledger.mu.Lock()
	entry := ledger.entries[id]
	if entry.Mapping.Owner != "owner" || entry.Mapping.ThreadID != "upstream-thread" || !entry.Invalid {
		t.Fatal("conflict overwrote original owner instead of tombstoning")
	}
	entry.Expires = time.Now().Add(-time.Second)
	ledger.entries[id] = entry
	ledger.mu.Unlock()
	if _, ok, err := ledger.Lookup(t.Context(), "key-a", "turn"); err != nil || ok {
		t.Fatal("expired mapping became known priced candidate")
	}
}

func TestAppServerTurnCostRedisLedgerCrossNodeAtomicity(t *testing.T) {
	db, _ := gatewayRedisSessionFixture(t)
	id, _ := appServerTurnCostIdentity("key-a", "turn")
	prefix := db.Namespace + "gateway:turn-cost:{" + id + "}"
	t.Cleanup(func() {
		if err := db.Redis.Del(context.Background(), prefix+":identity", prefix+":responses").Err(); err != nil {
			t.Error("isolated ledger cleanup failed")
		}
	})
	testAppServerTurnCostLedger(t, newAppServerTurnCostLedger(db), newAppServerTurnCostLedger(db))
	if db.Redis.HGet(t.Context(), prefix+":identity", "owner").Val() != "owner" || db.Redis.HGet(t.Context(), prefix+":identity", "invalid").Val() != "true" {
		t.Fatal("Redis tombstone lost original owner")
	}
	if ttl := db.Redis.TTL(t.Context(), prefix+":identity").Val(); ttl < 7*24*time.Hour-time.Minute {
		t.Fatalf("ledger retention=%v", ttl)
	}
}

func TestAppServerTurnCostActualOfficialCapture(t *testing.T) {
	raw, err := os.ReadFile("testdata/appserver_system_cost_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Cases []struct {
			Case     string `json:"case"`
			Thread   string `json:"thread_id"`
			Turn     string `json:"turn_id"`
			Response string `json:"response_id"`
			Requests []struct {
				Response json.RawMessage `json:"response"`
			} `json:"cost_requests"`
			Metrics []struct {
				Amount int64 `json:"asInt"`
			} `json:"actual_cost_metric_points"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, capture := range oracle.Cases {
		if capture.Case != "chatgpt" {
			continue
		}
		seen = true
		if len(capture.Requests) != 2 || len(capture.Metrics) == 0 || capture.Metrics[0].Amount != 125001 {
			t.Fatal("missing official poll and emitted cost metric")
		}
		mapping := appServerTurnCostMapping{Owner: "synthetic-system-owner", ThreadID: capture.Thread, TurnID: capture.Turn, ResponseIDs: []string{capture.Response}}
		body, err := projectAppServerTurnCosts([]string{capture.Turn}, map[string]appServerTurnCostMapping{capture.Turn: mapping}, capture.Requests[1].Response)
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Turns []appServerProjectedTurnCost `json:"turns"`
		}
		_ = json.Unmarshal(body, &out)
		if len(out.Turns) != 1 || out.Turns[0].Status != "priced" || out.Turns[0].TotalUSD == nil || *out.Turns[0].TotalUSD != "0.125001" {
			t.Fatalf("projection differs from actual executable cost consumption %s", body)
		}
	}
	if !seen {
		t.Fatal("missing ChatGPT actual writer case")
	}
}
