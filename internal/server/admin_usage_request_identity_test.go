package server

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

func TestPGManagedRequestIDFilterCanonicalIdentity(t *testing.T) {
	m := managedUsageFixture(t, true)
	ctx := context.Background()
	original, err := m.managedLogDetail(ctx, "event-a")
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(original.ID, 10)
	for i, record := range []RequestRecord{
		{EventID: id}, {EventID: "0" + id}, {RequestID: id}, {ResponseID: id},
		{RequestID: "+" + id}, {ResponseID: "0" + id}, {RequestID: "999999999999999999999999999999"},
	} {
		if record.EventID == "" {
			record.EventID = fmt.Sprintf("identity-filter-%d", i)
		}
		record.Time, record.Status = 1700000010+int64(i), "ok"
		m.Record(record)
	}
	if err = m.HealthError(); err != nil {
		t.Fatal(err)
	}
	all, err := m.managedLogs(ctx, managedQuery(t, "size=100"))
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{id, "0" + id, "+" + id, "-" + id, "event-a", "999999999999999999999999999999", "missing"} {
		q := managedQuery(t, "request_id="+url.QueryEscape(identity)+"&size=100")
		want, got := []int64{}, []int64{}
		for _, record := range all.Items {
			if q.matches(record.RequestRecord) {
				want = append(want, record.ID)
			}
		}
		page, err := m.managedLogs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Items {
			got = append(got, record.ID)
		}
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("request_id=%q matched %v, want %v", identity, got, want)
		}
	}
}
