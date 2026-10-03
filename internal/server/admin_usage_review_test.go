package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestManagedUsageErrorReview(t *testing.T) { exerciseManagedUsageErrorReview(t, webTestSetup) }
func TestPGManagedUsageErrorReview(t *testing.T) {
	exerciseManagedUsageErrorReview(t, adminPGTestProxy)
}

func exerciseManagedUsageErrorReview(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, handler := setup(t)
	for i, status := range []string{"error", "aborted", "error", "ok"} {
		p.Metrics.Record(RequestRecord{EventID: fmt.Sprintf("review-%d", i), Time: 1700000000 + int64(i), Model: "review-model", Status: status, AccountID: "review-account", HTTPCode: 200, InputTokens: 5, OutputTokens: 2})
	}
	if err := p.Metrics.HealthError(); err != nil {
		t.Fatal(err)
	}
	read := func(id string) managedUsageReview {
		response := managedTestRequest(t, handler, "GET", "/api/logs/"+id+"/review", nil, 200)
		var result struct {
			Review managedUsageReview `json:"review"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Review
	}
	list := func(query string) (int64, string, []managedUsageReview) {
		response := managedTestRequest(t, handler, "GET", "/api/logs/errors?"+query, nil, 200)
		var result struct {
			Items []struct {
				Record managedLogRecord   `json:"record"`
				Review managedUsageReview `json:"review"`
			} `json:"items"`
			Total  int64  `json:"total"`
			Cursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		out := make([]managedUsageReview, 0, len(result.Items))
		for _, row := range result.Items {
			if row.Record.Status == "ok" {
				t.Fatal("success appeared in error workspace")
			}
			out = append(out, row.Review)
		}
		return result.Total, result.Cursor, out
	}
	initial := read("review-0")
	if initial.Version != 0 || initial.Resolved || initial.History == nil {
		t.Fatalf("new review: %+v", initial)
	}
	if count, _, _ := list("resolved=false"); count != 3 {
		t.Fatalf("unresolved total %d", count)
	}
	if count, _, _ := list("resolved=true"); count != 0 {
		t.Fatalf("initial resolved total %d", count)
	}
	_, cursor, _ := list("resolved=false&page_size=1")
	if cursor == "" {
		t.Fatal("missing exact pagination cursor")
	}
	if total, _, rows := list("resolved=false&page_size=1&cursor=" + url.QueryEscape(cursor)); total != 3 || len(rows) != 1 {
		t.Fatal("unchanged error-review cursor could not advance")
	}
	for _, path := range []string{"/api/logs/review-0/review", "/api/logs/errors"} {
		response := webTestRequest(handler, "GET", path, "", "")
		if response.Code != 401 {
			t.Fatal("review read lacked admin authorization")
		}
	}
	body := map[string]any{"version": 0, "resolved": true, "note": "checked Bearer private-secret-token"}
	managedTestRequest(t, handler, "PUT", "/api/logs/review-0/review", body, 200)
	updated := read("review-0")
	if !updated.Resolved || updated.Version != 1 || updated.HistoryTotal != 1 || updated.ResolvedBy == "" || updated.ResolvedAt == 0 || strings.Contains(updated.Note, "private-secret-token") {
		t.Fatalf("persisted review or redaction: %+v", updated)
	}
	managedTestRequest(t, handler, "PUT", "/api/logs/review-0/review", body, 409)
	managedTestRequest(t, handler, "GET", "/api/logs/errors?resolved=false&page_size=1&cursor="+url.QueryEscape(cursor), nil, 409)
	if count, _, rows := list("resolved=true&account_id=review-account&model=review-model"); count != 1 || len(rows) != 1 || !rows[0].Resolved || rows[0].Version != 1 {
		t.Fatal("resolved filter lost exact count or annotation")
	}
	if count, _, _ := list("resolved=false"); count != 2 {
		t.Fatalf("resolved request remained pending: %d", count)
	}
	if count, _, _ := list("resolved=true&account_id=different"); count != 0 {
		t.Fatal("review filter ignored account")
	}
	// A second Proxy sees the persisted state, without any in-memory review map.
	restarted := &Proxy{Store: p.Store, Metrics: p.Metrics}
	record, err := p.Metrics.managedLogDetail(context.Background(), "review-0")
	if err != nil {
		t.Fatal(err)
	}
	durable, err := restarted.managedUsageReview(context.Background(), record)
	if err != nil || !durable.Resolved || durable.Version != 1 {
		t.Fatal("review lost on new instance", err)
	}
	if record.Status != "error" || record.HTTPCode != 200 || record.InputTokens != 5 {
		t.Fatal("human resolution changed the original request")
	}
	// Concurrent editors both read version 1. Exactly one can commit version 2.
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := webTestRequest(handler, "PUT", "/api/logs/review-0/review", `{"version":1,"resolved":false,"note":"reopened"}`, "Bearer "+webTestKey)
			statuses <- response.Code
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[200] != 1 || counts[409] != 1 {
		t.Fatal("concurrent CAS", counts)
	}
	updated = read("review-0")
	if updated.Resolved || updated.ResolvedAt != 0 || updated.ResolvedBy != "" || updated.Version != 2 || len(updated.History) != 2 {
		t.Fatalf("reopen history: %+v", updated)
	}
	if count, _, _ := list("resolved=false"); count != 3 {
		t.Fatal("reopened request missing")
	}
	for _, body := range []any{map[string]any{"resolved": true}, map[string]any{"version": 2}, map[string]any{"version": -1, "resolved": true}, map[string]any{"version": 2, "resolved": true, "note": strings.Repeat("长", 1001)}} {
		managedTestRequest(t, handler, "PUT", "/api/logs/review-0/review", body, 400)
	}
	managedTestRequest(t, handler, "PUT", "/api/logs/review-3/review", map[string]any{"version": 0, "resolved": true}, 400)
	managedTestRequest(t, handler, "GET", "/api/logs/missing/review", nil, 404)
	for _, query := range []string{"resolved=1", "resolved=true&resolved=false", "status=ok", "as_of=usage_unrelated"} {
		managedTestRequest(t, handler, "GET", "/api/logs/errors?"+query, nil, 400)
	}
	if p.Store.DB != nil {
		// A configuration audit failure must roll back the review and history.
		_, err = p.Store.DB.Pool.Exec(context.Background(), `CREATE FUNCTION reject_review_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='usage_review.update' THEN RAISE EXCEPTION 'review audit fixture'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_review BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_review_audit()`)
		if err != nil {
			t.Fatal(err)
		}
		managedTestRequest(t, handler, "PUT", "/api/logs/review-0/review", map[string]any{"version": 2, "resolved": true}, 500)
		if row := read("review-0"); row.Version != 2 || row.Resolved {
			t.Fatal("failed audit committed review")
		}
		var eventCount int
		if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM usage_events").Scan(&eventCount); err != nil || eventCount != 4 {
			t.Fatal("review altered accounting events", err, eventCount)
		}
	}
}

func TestManagedUsageReviewHistoryAndLegacyIdentity(t *testing.T) {
	p, handler := webTestSetup(t)
	r := RequestRecord{EventID: "bounded-review", Time: 1700000000, Model: "review", Status: "error"}
	p.Metrics.Record(r)
	for i := 0; i < 52; i++ {
		note := "history"
		if i == 51 {
			note = strings.Repeat("核", 1000)
		}
		managedTestRequest(t, handler, "PUT", "/api/logs/bounded-review/review", map[string]any{"version": i, "resolved": i%2 == 0, "note": note}, 200)
	}
	review, err := p.managedUsageReview(context.Background(), r)
	if err != nil || review.HistoryTotal != 52 || len(review.History) != 50 || !review.HistoryTruncated || review.History[0].Version != 3 || review.Note != strings.Repeat("核", 1000) {
		t.Fatal("bounded history or unicode note", err)
	}
	r.EventID, r.ID, r.Error = "", 1, "Bearer original-sensitive-error"
	key := managedUsageReviewKey(r)
	r.ID, r.Error = 900, redactMetricError(r.Error)
	if managedUsageReviewKey(r) != key {
		t.Fatal("legacy review moved after prune or sanitization")
	}
	r.Time++
	if managedUsageReviewKey(r) == key {
		t.Fatal("different legacy request reused a line's review")
	}
}
