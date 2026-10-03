package server

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestPGManagedUsageModelConditionedParity(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	const base int64 = 1710028800
	for i := 0; i < 48; i++ {
		r := managedPerformanceRecord(i)
		r.EventID, r.Time = fmt.Sprintf("model-axis-%d", i), base+int64(i/8)*86400+int64(i%8)*7201
		r.Model = []string{"model-a", "model-b"}[i%2]
		r.AccountID, r.Account = fmt.Sprintf("account-%d", (i/2)%3), fmt.Sprintf("name-%d", i%5)
		r.KeyID, r.KeyName = "shared", "Shared"
		r.Groups, r.Endpoint = []string{"shared", "shared"}, "responses"
		if i%4 == 0 {
			r.KeyID, r.KeyName = "partial", "Partial"
			r.Groups, r.Endpoint = []string{"partial", "partial", "shared"}, "compact"
		}
		r.Status = []string{"ok", "error", "aborted"}[i%3]
		m.Record(r)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	filters := []string{
		"model=model-a", "model=model-b", "model=missing", "model=model-a&status=failed",
		"model=model-b&key_id=shared&group_id=shared&endpoint=responses",
		"model=model-a&key_id=partial&group_id=partial&endpoint=compact",
		"model=model-a&key_id=shared&group_id=partial", // disjoint within the model
		"model=model-a&key_id=missing", "model=model-b&group_id=partial",
	}
	for _, zone := range []string{"UTC", "America%2FNew_York", "Asia%2FKathmandu"} {
		for _, step := range []int64{60, 300, 3600, 86400, 604800, 2592000} {
			end := base + 6*86400
			if step == 60 {
				end = base + 2*86400
			}
			for _, limits := range [][2]int64{{base, end}, {base + 1, end - 17}} {
				filters = append(filters, fmt.Sprintf("model=model-a&from=%d&to=%d&step=%d&timezone=%s", limits[0], limits[1], step, zone))
			}
		}
	}
	assertFilteredRollupParity(t, m, filters...)
	for _, statement := range []string{
		`UPDATE request_logs SET model='model-a',record=record||'{"key_id":"partial","groups":["partial","partial"]}'::jsonb WHERE event_id='model-axis-1'`,
		`UPDATE request_logs SET model='model-b',account_name='z-name',created_at=created_at+interval '0.75 second' WHERE event_id='model-axis-0'`,
		`DELETE FROM request_logs WHERE event_id IN ('model-axis-5','model-axis-7')`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertFilteredRollupParity(t, m, filters...)
	}
}

func TestPGManagedUsageModelLongKeysAndUnsafe(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	// Incompressible-looking long values also exercise the combined BTree key
	// budget: 512 model bytes plus an original 1700-byte dimension key is safe;
	// a 513-byte model uses its own exact minute guard instead of rejecting it.
	makeText := func(n int, salt string) string {
		var b strings.Builder
		for b.Len() < n {
			fmt.Fprintf(&b, "%x:%s;", b.Len()*7919, salt)
		}
		return b.String()[:n]
	}
	filters := []string{"model=short", "model=short&key_id=key&group_id=shared"}
	for i, size := range []int{512, 513, 1700, 4000} {
		model := makeText(size, fmt.Sprint(i))
		r := RequestRecord{EventID: fmt.Sprintf("long-axis-%d", i), Time: 1700000040 + int64(i)*86400, Model: model, Status: "ok", AccountID: makeText(850, "id"), Account: makeText(850, "name"), KeyID: "key", KeyName: "Key", Groups: []string{"shared"}, InputTokens: int64(i + 1)}
		m.Record(r)
		m.Record(RequestRecord{EventID: fmt.Sprintf("short-axis-%d", i), Time: r.Time + 1, Model: "short", Status: "ok", KeyID: "key", KeyName: "Key", Groups: []string{"shared"}, InputTokens: 31})
		if size <= 1024 {
			filters = append(filters, "model="+url.QueryEscape(model), "model="+url.QueryEscape(model)+"&group_id=shared")
		}
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	assertFilteredRollupParity(t, m, filters...)
	for _, statement := range []string{
		`UPDATE request_logs SET model='short' WHERE event_id='long-axis-1'`,
		`DELETE FROM request_logs WHERE event_id='long-axis-2'`,
		`UPDATE request_logs SET model='short' WHERE event_id='long-axis-3'`,
		`TRUNCATE request_logs`,
	} {
		if _, err := db.Pool.Exec(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
		assertFilteredRollupParity(t, m, filters...)
	}
}
