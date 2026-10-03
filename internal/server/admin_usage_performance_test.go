package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"testing"
)

func TestManagedAccountProjectionParity(t *testing.T) {
	exerciseManagedAccountProjectionParity(t, false)
}
func TestPGManagedAccountProjectionParity(t *testing.T) {
	exerciseManagedAccountProjectionParity(t, true)
}

func exerciseManagedAccountProjectionParity(t *testing.T, pg bool) {
	m := managedUsageFixture(t, pg)
	for i := 0; i < 4; i++ {
		v := int64(i * 3)
		r := RequestRecord{EventID: fmt.Sprintf("projection-%d", i), Time: 1700000004 + int64(i), AccountID: "acc-1", Account: "renamed", Status: "error", HTTPCode: 504, Model: "extra", Groups: []string{"group-a", "group-a", "group-b"}, InputTokens: 17, OutputTokens: 2, LatencyMs: 29, RequestPreparationMs: &v, AccountSelectionMs: &v, ProxySelectionMs: &v, AuthenticationMs: &v, ResponseTransferMs: &v, TTFTMs: &v, ImageInputTokens: &v, ImageOutputTokens: &v, ReasoningTokens: &v, CacheWriteTokens: &v}
		if i == 3 {
			r.ImageInputTokens = new(int64)
			*r.ImageInputTokens = math.MaxInt64
		}
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"", "status=failed", "from=1700000001&to=1700000007", "account_id=missing"} {
		q := managedQuery(t, filter)
		q.AccountIDs = []string{"acc-1", "acc-2", "missing"}
		full, err := m.managedLogStats(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		hash := q.filterHash()
		q.accountsOnly = true
		if q.filterHash() != hash {
			t.Fatal("projection changed cursor/snapshot identity")
		}
		projected, err := m.managedLogStats(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		// Compare the public result, including optional observations, nulls,
		// renamed labels, exact IDs, image overflow, and empty matches.
		want, _ := json.Marshal(full.Accounts)
		got, _ := json.Marshal(projected.Accounts)
		if string(got) != string(want) {
			t.Fatalf("filter=%q got=%s want=%s", filter, got, want)
		}
	}
}

func TestManagedUsageAverageSamples(t *testing.T) {
	var totals managedUsageTotals
	for _, value := range []int64{0, 11, -1, 4} {
		totals.add(RequestRecord{Status: "ok", TTFTMs: &value, RequestPreparationMs: &value, AccountSelectionMs: &value, ProxySelectionMs: &value, AuthenticationMs: &value, ResponseTransferMs: &value})
	}
	totals.add(RequestRecord{Status: "ok"})
	for _, field := range []struct {
		count   int64
		average *float64
	}{
		{totals.TTFTSamples, totals.AverageTTFT},
		{totals.RequestPreparationSamples, totals.AverageRequestPreparation},
		{totals.AccountSelectionSamples, totals.AverageAccountSelection},
		{totals.ProxySelectionSamples, totals.AverageProxySelection},
		{totals.AuthenticationSamples, totals.AverageAuthentication},
		{totals.ResponseTransferSamples, totals.AverageResponseTransfer},
	} {
		if field.count != 3 || field.average == nil || *field.average != 5 {
			t.Fatalf("samples/average: %+v", field)
		}
	}
}

func TestPGManagedLogDetailCanonicalIdentity(t *testing.T) {
	m := managedUsageFixture(t, true)
	original, err := m.managedLogDetail(context.Background(), "event-a")
	if err != nil {
		t.Fatal(err)
	}
	canonical := strconv.FormatInt(original.ID, 10)
	m.Record(RequestRecord{Time: 1700000005, EventID: canonical, Model: "numeric-event", Status: "ok"})
	m.Record(RequestRecord{Time: 1700000006, EventID: "0" + canonical, Model: "padded-event", Status: "ok"})
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"event-a": "codex-a", canonical: "numeric-event", "0" + canonical: "padded-event"} {
		record, err := m.managedLogDetail(context.Background(), id)
		if err != nil || record.Model != want {
			t.Fatalf("id=%q got=%+v err=%v", id, record, err)
		}
	}
	for _, id := range []string{"+" + canonical, "99999999999999999999999999"} {
		if _, err := m.managedLogDetail(context.Background(), id); err == nil {
			t.Fatalf("noncanonical ID %q matched", id)
		}
	}
}

func TestPGManagedNumberedPageParity(t *testing.T) {
	m := managedUsageFixture(t, true)
	ctx := context.Background()
	for _, field := range []string{"time", "model", "tokens", "input_tokens", "output_tokens", "cached_tokens", "latency"} {
		for _, direction := range []string{"asc", "desc"} {
			q := managedQuery(t, "size=2&exact_total=true&sort_by="+field+"&sort_order="+direction)
			first, err := m.managedLogs(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			q.Cursor = first.NextCursor
			cursorPage, err := m.managedLogs(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			q.Cursor, q.Page = "", 2
			numbered, err := m.managedLogs(ctx, q)
			if err != nil || !reflect.DeepEqual(numbered.Items, cursorPage.Items) || numbered.HasMore != cursorPage.HasMore || numbered.NextCursor != cursorPage.NextCursor || numbered.Total != cursorPage.Total {
				t.Fatalf("%s/%s page mismatch: %+v %+v err=%v", field, direction, numbered, cursorPage, err)
			}
			q.Page = 100
			empty, err := m.managedLogs(ctx, q)
			if err != nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != "" || empty.Total != 4 {
				t.Fatalf("past end: %+v %v", empty, err)
			}
		}
	}
}
