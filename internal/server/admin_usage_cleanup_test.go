package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestPGManagedCleanupLifecycle(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	ctx := context.Background()
	db := p.Store.DB
	insert := `WITH events AS (INSERT INTO usage_events(event_id,created_at,status,input_tokens) SELECT $1||i::text,to_timestamp(1700000000),'ok',7 FROM generate_series(1,$2::int)i RETURNING event_id,created_at,status,input_tokens)
 INSERT INTO request_logs(event_id,created_at,status,model,record) SELECT event_id,created_at,status,'cleanup-fixture',jsonb_build_object('event_id',event_id,'time',1700000000,'status',status,'model','cleanup-fixture','input_tokens',7) FROM events`
	// Allocate the smaller sequence ID but keep it invisible to the preview.
	early, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer early.Rollback(ctx)
	// Reserve the smaller identity without holding the shared minute-summary
	// row while synchronously waiting for the visible writer to commit. The
	// late record still has the same timestamp and a lower ID at preview time.
	var lateID int64
	if err = early.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('request_logs','id'))").Scan(&lateID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, insert, "visible-", 1205); err != nil {
		t.Fatal(err)
	}
	if _, err = early.Exec(ctx, `WITH event AS (
 INSERT INTO usage_events(event_id,created_at,status,input_tokens) VALUES('late-commit-1',to_timestamp(1700000000),'ok',7) RETURNING *
 ) INSERT INTO request_logs(id,event_id,created_at,status,model,record)
 SELECT $1,event_id,created_at,status,'cleanup-fixture',jsonb_build_object('event_id',event_id,'time',1700000000,'status',status,'model','cleanup-fixture','input_tokens',7) FROM event`, lateID); err != nil {
		t.Fatal(err)
	}
	filters := map[string]string{"from": "1699999999", "to": "1700000001", "model": "cleanup-fixture"}
	previewRequest := func() managedCleanupPreview {
		response := managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/preview", map[string]any{"filters": filters}, 200)
		var result managedCleanupPreview
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	preview := previewRequest()
	if preview.Total != 1205 {
		t.Fatalf("preview count: %d", preview.Total)
	}
	if err = early.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Pool.Exec(ctx, insert, "new-after-preview-", 1); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": "synthetic-cleanup-idempotency"}
	var created struct {
		Job managedCleanupJob `json:"job"`
	}
	response := managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", body, 200)
	if err = json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", body, 200)
	body["request_id"] = "different-idempotency"
	managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", body, 409)
	if err = p.runManagedCleanupBatch(ctx, created.Job.ID); err != nil {
		t.Fatal(err)
	}
	entry, err := p.managementState().Get(ctx, "cleanup-jobs", created.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var job managedCleanupJob
	if err = json.Unmarshal(entry.Payload, &job); err != nil {
		t.Fatal(err)
	}
	if job.Status != "running" || job.Deleted != 1000 || job.Remaining != 205 {
		t.Fatalf("checkpoint: %+v", job)
	}
	// A new process resumes the committed checkpoint; it must exclude both the
	// later commit with an older ID and the record added after the preview.
	restarted := &Proxy{Store: p.Store}
	if err = restarted.runManagedCleanupBatch(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	var logs, events int64
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_events").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if logs != 2 || events != 1207 {
		t.Fatalf("snapshot or accounting violated: logs=%d events=%d", logs, events)
	}
	preview = previewRequest()
	body = map[string]string{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": "canceled-job"}
	response = managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", body, 200)
	json.Unmarshal(response.Body.Bytes(), &created)
	managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs/"+created.Job.ID+"/cancel", map[string]any{}, 200)
	managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs/"+created.Job.ID+"/cancel", map[string]any{}, 200)
	if err = p.runManagedCleanupBatch(ctx, created.Job.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&logs); err != nil || logs != 2 {
		t.Fatalf("cancel deleted records: %d %v", logs, err)
	}
	// Audit failures roll back the deletion and checkpoint together.
	preview = previewRequest()
	body = map[string]string{"preview_id": preview.ID, "confirmation_token": preview.Token, "request_id": "rollback-job"}
	response = managedTestRequest(t, handler, http.MethodPost, "/api/logs/cleanup/jobs", body, 200)
	json.Unmarshal(response.Body.Bytes(), &created)
	_, err = db.Pool.Exec(ctx, `CREATE FUNCTION reject_cleanup_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='usage_details.cleanup' THEN RAISE EXCEPTION 'synthetic audit failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_cleanup BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION reject_cleanup_audit()`)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.runManagedCleanupBatch(ctx, created.Job.ID); err == nil {
		t.Fatal("expected injected transaction failure")
	}
	if err = db.Pool.QueryRow(ctx, "SELECT count(*) FROM request_logs").Scan(&logs); err != nil || logs != 2 {
		t.Fatalf("rollback lost records: %d %v", logs, err)
	}
	if _, err = db.Pool.Exec(ctx, "DROP TRIGGER reject_cleanup ON audit_logs"); err != nil {
		t.Fatal(err)
	}
	if err = p.runManagedCleanupBatch(ctx, created.Job.ID); err != nil {
		t.Fatal(err)
	}
	managedTestRequest(t, handler, http.MethodGet, "/api/logs/cleanup/jobs?page=1&page_size=1", nil, 200)
	if err = p.pruneManagementEphemeral(ctx); err != nil {
		t.Fatal(err)
	}
}
