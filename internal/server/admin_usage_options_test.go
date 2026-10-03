package server

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func exerciseManagedUsageOptionsCalendar(t *testing.T, pg bool) {
	m := managedUsageFixture(t, pg)
	ctx := context.Background()
	q := managedQuery(t, "group_id=group-a")
	options, err := m.managedUsageOptions(ctx, q, "account", "renamed", 50, map[string]string{"acc-1": "Renamed account"})
	if err != nil || len(options) != 1 || options[0].Value != "acc-1" || options[0].Deleted {
		t.Fatalf("current name: %+v %v", options, err)
	}
	options, err = m.managedUsageOptions(ctx, managedQuery(t, ""), "key", "", 50, map[string]string{"key-1": "Primary"})
	if err != nil || len(options) != 2 || !options[1].Deleted {
		t.Fatalf("deleted identity: %+v %v", options, err)
	}
	options, err = m.managedUsageOptions(ctx, managedQuery(t, ""), "model", "codex", 1, nil)
	if err != nil || len(options) != 2 {
		t.Fatalf("lookahead: %+v %v", options, err)
	}
	options, err = m.managedUsageOptions(ctx, q, "group", "", 50, nil)
	if err != nil || len(options) != 1 || options[0].Value != "group-a" {
		t.Fatalf("group options: %+v %v", options, err)
	}
	// New York's spring transition has a 23-hour calendar day.
	location, _ := time.LoadLocation("America/New_York")
	for i, day := range []int{7, 8, 9} {
		stamp := time.Date(2026, 3, day, 12, 0, 0, 0, location).Unix()
		upstreamModel := "observed-dst"
		if i == 0 {
			upstreamModel = ""
		}
		m.Record(RequestRecord{EventID: "dst-" + string(rune('a'+i)), Time: stamp, Model: "dst", UpstreamModel: upstreamModel, Status: "ok", Groups: []string{"g1", "g1", "g2"}, InputTokens: 1})
	}
	stats, err := m.managedLogStats(ctx, managedQuery(t, "model=dst&from=1772700000&to=1773200000&step=86400&timezone=America%2FNew_York"))
	if err != nil {
		t.Fatal(err)
	}
	byTime := map[int64]managedUsagePoint{}
	for _, point := range stats.Series {
		byTime[point.Time] = point
	}
	march8 := time.Date(2026, 3, 8, 0, 0, 0, 0, location).Unix()
	march9 := time.Date(2026, 3, 9, 0, 0, 0, 0, location).Unix()
	if len(stats.Series) < 3 || march9-march8 != 23*3600 || byTime[march8].Requests != 1 || byTime[march9].Requests != 1 || len(stats.Groups) != 2 || stats.Groups[0].Requests != 3 || stats.Requests != 3 {
		t.Fatalf("DST or overlapping group semantics: %+v", stats)
	}
	if len(stats.Models) != 1 || stats.Models[0].Name != "dst" || stats.Models[0].Requests != 3 || len(stats.UpstreamModels) != 1 || stats.UpstreamModels[0].Name != "observed-dst" || stats.UpstreamModels[0].Requests != 2 || stats.UpstreamModels[0].Tokens != 2 {
		t.Fatalf("requested and observed model dimensions must not fill unknown observations: %+v", stats)
	}
	stats, err = m.managedLogStats(ctx, managedQuery(t, "model=dst&step=2592000&timezone=America%2FNew_York"))
	if err != nil || len(stats.Series) != 1 || stats.Series[0].Time != time.Date(2026, 3, 1, 0, 0, 0, 0, location).Unix() {
		t.Fatalf("calendar month: %+v %v", stats, err)
	}
	var export bytes.Buffer
	_, err = m.exportManagedLogsWithPrivacy(ctx, managedQuery(t, "group_id=group-a"), "jsonl", &export, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(export.String(), "Alpha") || strings.Contains(export.String(), "Primary") || strings.Contains(export.String(), "Bearer") || strings.Contains(export.String(), "key-1") {
		t.Fatal("redacted export leaked identity")
	}
	var row managedLogRecord
	if err = json.Unmarshal(bytes.Split(export.Bytes(), []byte{'\n'})[0], &row); err != nil || row.ErrorCategory == "" || !strings.HasPrefix(row.KeyID, "key_") {
		t.Fatalf("redaction removed structured context: %+v %v", row, err)
	}
}

func TestManagedUsageOptionsCalendarFile(t *testing.T) { exerciseManagedUsageOptionsCalendar(t, false) }
func TestPGManagedUsageOptionsCalendar(t *testing.T)   { exerciseManagedUsageOptionsCalendar(t, true) }

func TestPGManagedUsageOptionsCurrentLabelsPreserveSearchAndDeleted(t *testing.T) {
	db := newMetricsTestDB(t)
	m := newTestPGMetrics(t, db)
	ctx := context.Background()
	if _, err := db.Pool.Exec(ctx, `
INSERT INTO accounts(id,display_name,created_at,updated_at) VALUES
 ('acc-live','Renamed account',1,1);
INSERT INTO groups(id,name) VALUES ('group-live','Renamed group')`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []RequestRecord{
		{EventID: "current-option-account", Time: 1700000000, AccountID: "acc-live", Account: "Old account", Groups: []string{"group-live"}, Model: "option-model", Status: "ok"},
		{EventID: "deleted-option-account", Time: 1700000001, AccountID: "acc-deleted", Account: "Deleted account", Groups: []string{"group-deleted"}, Model: "option-model", Status: "ok"},
	} {
		m.Record(row)
	}
	if err := m.HealthError(); err != nil {
		t.Fatal(err)
	}
	current := map[string]string{"acc-live": "Renamed account", "group-live": "Renamed group"}
	for _, tc := range []struct {
		field, search string
	}{
		{"account", ""}, {"account", "renamed"}, {"account", "deleted"},
		{"group", ""}, {"group", "renamed"}, {"group", "deleted"},
	} {
		t.Run(tc.field+"/"+tc.search, func(t *testing.T) {
			q := managedQuery(t, "")
			got, err := m.managedUsageOptionsPGCurrent(ctx, q, tc.field, tc.search, 50)
			if err != nil {
				t.Fatal(err)
			}
			want, err := m.managedUsageOptions(ctx, q, tc.field, tc.search, 50, current)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("bounded options got=%+v want=%+v", got, want)
			}
		})
	}
}
