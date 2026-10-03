package server

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestPGManagedUsageOptionIdentityParity(t *testing.T) {
	pg := newTestPGMetrics(t, newMetricsTestDB(t))
	file, err := NewMetrics(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(file.Close)
	rows := []RequestRecord{
		{Account: "legacy-name", KeyName: "legacy-key", Model: "old-model"},
		{AccountID: "account-a", Account: "Zulu", KeyID: "key-a", KeyName: "Zulu", Model: "common-model", Groups: []string{"", "group-a", "group-a"}},
		{AccountID: "account-a", Account: "\u00e1lpha", KeyID: "key-a", KeyName: "\u00e1lpha", Model: "common-model", Groups: []string{"group-a"}},
		{AccountID: "account-b", Account: "Zulu", KeyID: "key-b", KeyName: "Zulu", Model: "other-model"},
		{AccountID: "name:real-account", Account: "Prefix is a valid stable ID", KeyID: "name:real-key", KeyName: "Prefix key", Model: "other-model"},
	}
	for i, row := range rows {
		row.Time, row.EventID, row.Status = int64(1700000000+i), fmt.Sprintf("option-parity-%d", i), "ok"
		file.Record(row)
		pg.Record(row)
	}
	if file.HealthError() != nil || pg.HealthError() != nil {
		t.Fatal(file.HealthError(), pg.HealthError())
	}
	for _, field := range []string{"account", "key", "model", "group"} {
		for _, search := range []string{"", "zulu", "legacy", "renamed", "group"} {
			t.Run(field+"/"+search, func(t *testing.T) {
				current := map[string]string{"account-b": "renamed", "key-b": "renamed", "group-a": "renamed"}
				q := managedQuery(t, "")
				want, err := file.managedUsageOptions(context.Background(), q, field, search, 50, current)
				if err != nil {
					t.Fatal(err)
				}
				got, err := pg.managedUsageOptions(context.Background(), q, field, search, 50, current)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("options got=%+v want=%+v error=%v", got, want, err)
				}
				for _, option := range got {
					if field == "account" && option.Value == "legacy:legacy-name" {
						t.Fatal("legacy aggregation key was exposed as a real account ID")
					}
				}
			})
		}
	}
}
