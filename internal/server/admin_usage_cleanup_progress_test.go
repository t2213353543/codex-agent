package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
)

func TestPGManagedCleanupOverlappingTargetsAndConcurrentResume(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	ctx := t.Context()
	_, err := p.Store.DB.Pool.Exec(ctx, `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status,input_tokens)
 SELECT 'cleanup-progress-'||i,to_timestamp(1700000000),'ok',7 FROM generate_series(1,2505)i RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,model,record)
 SELECT event_id,created_at,status,'cleanup-progress',jsonb_build_object('event_id',event_id,'time',1700000000,'status',status,'model','cleanup-progress','input_tokens',7) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	create := func(id string) managedCleanupJob {
		t.Helper()
		response := managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/preview", map[string]any{"filters": map[string]string{"from": "1699999999", "to": "1700000001", "model": "cleanup-progress"}}, 200)
		var preview managedCleanupPreview
		if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil || preview.Total != 2505 || preview.Ceiling == 0 {
			t.Fatalf("invalid preview: %+v %v", preview, err)
		}
		response = managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", map[string]string{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": id}, 200)
		var result struct {
			Job managedCleanupJob `json:"job"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Job.Remaining != 2505 {
			t.Fatalf("invalid initial checkpoint: %+v %v", result, err)
		}
		return result.Job
	}
	first, second := create("first"), create("second")
	checkpoint := func(id string, deleted, remaining int64, status string) {
		t.Helper()
		entry, err := p.managementState().Get(ctx, "cleanup-jobs", id)
		if err != nil {
			t.Fatal(err)
		}
		var job managedCleanupJob
		if err := json.Unmarshal(entry.Payload, &job); err != nil || job.Deleted != deleted || job.Remaining != remaining || job.Status != status {
			t.Fatalf("checkpoint %s: %+v err=%v", id, job, err)
		}
	}
	for _, job := range []managedCleanupJob{first, second} {
		if err := p.runManagedCleanupBatch(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint(first.ID, 1000, 1505, "running")
	// All first-batch details were removed by the other job, but these 1000
	// snapshot targets are nevertheless completed and must reduce Remaining.
	checkpoint(second.ID, 0, 1505, "running")
	var workers sync.WaitGroup
	errors := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			restarted := &Proxy{Store: p.Store}
			errors <- restarted.runManagedCleanupBatch(ctx, second.ID)
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	checkpoint(second.ID, 1505, 0, "succeeded")
	for range 2 {
		if err := p.runManagedCleanupBatch(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
	}
	checkpoint(first.ID, 1000, 0, "succeeded")
	var logs, targets, events, audits int64
	if err := p.Store.DB.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM request_logs),
 (SELECT count(*) FROM management_cleanup_targets),(SELECT count(*) FROM usage_events),
 (SELECT count(*) FROM audit_logs WHERE action='usage_details.cleanup')`).Scan(&logs, &targets, &events, &audits); err != nil || logs != 0 || targets != 0 || events != 2505 || audits != 2 {
		t.Fatalf("incorrect final cleanup state: logs=%d targets=%d events=%d audits=%d err=%v", logs, targets, events, audits, err)
	}
}
