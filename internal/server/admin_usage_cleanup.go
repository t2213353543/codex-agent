package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const managedCleanupScope = "retained_request_details"

type managedCleanupPreview struct {
	ID        string            `json:"preview_id"`
	Token     string            `json:"confirmation_token"`
	Total     int64             `json:"total"`
	Scope     string            `json:"scope"`
	ExpiresAt int64             `json:"expires_at"`
	Filters   map[string]string `json:"filters"`
	Ceiling   int64             `json:"snapshot_max_id"`
	Owner     string            `json:"-"`
}
type storedCleanupPreview struct {
	managedCleanupPreview
	OwnerHash string `json:"owner_hash"`
}
type managedCleanupJob struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Filters    map[string]string `json:"filters"`
	Total      int64             `json:"total"`
	Deleted    int64             `json:"deleted_rows"`
	Remaining  int64             `json:"remaining_rows"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  int64             `json:"created_at"`
	StartedAt  int64             `json:"started_at,omitempty"`
	FinishedAt int64             `json:"finished_at,omitempty"`
	Error      string            `json:"error,omitempty"`
	Scope      string            `json:"scope"`
	Ceiling    int64             `json:"snapshot_max_id"`
	RequestID  string            `json:"request_id"`
}

func managedOperationOwner(ctx context.Context) string {
	auth, ok := ctx.Value(adminAuthenticationContextKey{}).(adminAuthentication)
	if !ok {
		return ""
	}
	if auth.CookieToken != "" {
		return adminKeyHash("session:" + auth.CookieToken)
	}
	if auth.BearerHash != "" {
		return adminKeyHash("bearer:" + auth.BearerHash)
	}
	return ""
}

func managedCleanupQuery(filters map[string]string) (managedLogQuery, error) {
	values := url.Values{}
	for k, v := range filters {
		if k == "client_request_id" {
			values.Set(k, v)
			continue
		}
		if !stringIn(k, "from", "to", "account", "account_id", "key_name", "key_id", "group_id", "model", "endpoint", "status", "request_id", "error_category", "error_severity", "ip", "http_code", "stream", "upstream_model", "request_type", "service_tier", "reasoning_effort", "sort_by", "sort_order", "model_mismatch", "native_compaction", "websocket_warmup", "error_phase", "step", "timezone", "as_of", "sent_model", "returned_model", "inbound_path", "upstream_path", "sent_model_mismatch", "returned_model_mismatch", "error_truncated") {
			return managedLogQuery{}, fmt.Errorf("%w: unsupported cleanup filter", errInvalid)
		}
		values.Set(k, v)
	}
	q, err := parseManagedLogQuery(values)
	if err != nil {
		return q, err
	}
	if q.From <= 0 || q.To <= q.From || q.To > time.Now().Unix()+1 {
		return q, fmt.Errorf("%w: cleanup requires an explicit past time range", errInvalid)
	}
	return q, nil
}

func (p *Proxy) handleManagedCleanupPreview(w http.ResponseWriter, r *http.Request) {
	if p.Store.DB == nil {
		adminAPIError(w, 409, "postgres_required", "detail cleanup requires PostgreSQL server mode")
		return
	}
	var body struct {
		Filters map[string]string `json:"filters"`
	}
	if err := readJSON(w, r, &body); err != nil {
		adminError(w, err)
		return
	}
	q, err := managedCleanupQuery(body.Filters)
	if err != nil {
		adminError(w, err)
		return
	}
	owner := managedOperationOwner(r.Context())
	if owner == "" {
		adminAPIError(w, 401, "unauthorized", "admin authentication required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err = p.resolveManagedUsageSnapshot(ctx, &q); err != nil {
		p.storeError(w, err)
		return
	}
	source, where, args := q.readSourceSQL()
	preview := managedCleanupPreview{ID: newID("preview_"), Scope: managedCleanupScope, ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Filters: body.Filters, Owner: owner}
	// Materialize the exact visible IDs. A max(sequence ID) boundary alone
	// would accidentally include an older uncommitted insert that commits later.
	tx, err := p.Store.DB.Pool.Begin(ctx)
	if err != nil {
		p.storeError(w, safeDBError(err))
		return
	}
	defer tx.Rollback(context.Background())
	args = append(args, preview.ID, time.Unix(preview.ExpiresAt, 0))
	inserted, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at) SELECT $%d,id,$%d FROM %s WHERE %s", len(args)-1, len(args), source, where), args...)
	if err == nil {
		preview.Total = inserted.RowsAffected()
		// The insert command already supplies the exact MVCC-visible count.
		// Recover only the ceiling with a bounded primary-key probe instead of
		// scanning the just-materialized target set a second time.
		err = tx.QueryRow(ctx, "SELECT COALESCE((SELECT log_id FROM management_cleanup_targets WHERE preview_id=$1 ORDER BY log_id DESC LIMIT 1),0)", preview.ID).Scan(&preview.Ceiling)
	}
	if err != nil {
		p.storeError(w, safeDBError(err))
		return
	}
	var token [24]byte
	if _, err = rand.Read(token[:]); err != nil {
		p.storeError(w, err)
		return
	}
	preview.Token = hex.EncodeToString(token[:])
	payload, _ := json.Marshal(storedCleanupPreview{preview, owner})
	entry := managementStateEntry{Version: 1, ExpiresAt: preview.ExpiresAt, UpdatedAt: time.Now().Unix(), Payload: payload}
	name, _ := managementStateKey("cleanup-preview", preview.ID)
	if err = p.Store.DB.writeDocument(ctx, tx, name, entry); err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, preview)
}

func (p *Proxy) handleManagedCleanupCreate(w http.ResponseWriter, r *http.Request) {
	if p.Store.DB == nil {
		adminAPIError(w, 409, "postgres_required", "detail cleanup requires PostgreSQL server mode")
		return
	}
	var body struct {
		PreviewID string `json:"preview_id"`
		Token     string `json:"confirmation_token"`
		RequestID string `json:"request_id"`
	}
	if err := readJSON(w, r, &body); err != nil {
		adminError(w, err)
		return
	}
	if !validRotationRequestID(body.RequestID) || len(body.Token) != 48 {
		adminError(w, errInvalid)
		return
	}
	entry, err := p.managementState().Get(r.Context(), "cleanup-preview", body.PreviewID)
	if err != nil {
		p.storeError(w, err)
		return
	}
	var preview storedCleanupPreview
	if err = json.Unmarshal(entry.Payload, &preview); err != nil {
		p.storeError(w, err)
		return
	}
	owner := managedOperationOwner(r.Context())
	if owner == "" || owner != preview.OwnerHash || subtle.ConstantTimeCompare([]byte(preview.Token), []byte(body.Token)) != 1 {
		adminAPIError(w, 409, "preview_mismatch", "cleanup confirmation is invalid or belongs to another session")
		return
	}
	job := managedCleanupJob{ID: "cleanup_" + body.PreviewID, Status: "pending", Filters: preview.Filters, Total: preview.Total, Remaining: preview.Total, CreatedBy: owner[:16], CreatedAt: time.Now().Unix(), Scope: managedCleanupScope, Ceiling: preview.Ceiling, RequestID: body.RequestID}
	err = p.createManagedCleanupJob(r.Context(), preview, &job)
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"job": job})
}

func (p *Proxy) handleManagedCleanupList(w http.ResponseWriter, r *http.Request) {
	page, size := 1, 20
	for key, target := range map[string]*int{"page": &page, "page_size": &size} {
		if raw := r.URL.Query().Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				adminError(w, errInvalid)
				return
			}
			*target = n
		}
	}
	if page > 10000 || size > 100 {
		adminError(w, errInvalid)
		return
	}
	entries, err := p.managementState().List(r.Context(), "cleanup-jobs")
	if err != nil {
		p.storeError(w, err)
		return
	}
	jobs := []managedCleanupJob{}
	for _, entry := range entries {
		var job managedCleanupJob
		if err = json.Unmarshal(entry.Payload, &job); err != nil {
			p.storeError(w, err)
			return
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt == jobs[j].CreatedAt {
			return jobs[i].ID > jobs[j].ID
		}
		return jobs[i].CreatedAt > jobs[j].CreatedAt
	})
	total := len(jobs)
	from := (page - 1) * size
	if from > total {
		from = total
	}
	to := from + size
	if to > total {
		to = total
	}
	writeJSON(w, map[string]any{"items": jobs[from:to], "total": total, "page": page, "page_size": size})
}

func (p *Proxy) handleManagedCleanupCancel(w http.ResponseWriter, r *http.Request) {
	var result managedCleanupJob
	err := p.managementState().Update(r.Context(), "cleanup-jobs", r.PathValue("id"), func(e *managementStateEntry) error {
		if e.Version == 0 {
			return errNotFound
		}
		if err := json.Unmarshal(e.Payload, &result); err != nil {
			return err
		}
		if result.Status == "pending" || result.Status == "running" {
			result.Status = "canceled"
			result.FinishedAt = time.Now().Unix()
			e.Payload, _ = json.Marshal(result)
		}
		return nil
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"job": result})
}

func (p *Proxy) registerManagedCleanupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/logs/cleanup/preview", p.handleManagedCleanupPreview)
	mux.HandleFunc("POST /api/logs/cleanup/jobs", p.handleManagedCleanupCreate)
	mux.HandleFunc("GET /api/logs/cleanup/jobs", p.handleManagedCleanupList)
	mux.HandleFunc("POST /api/logs/cleanup/jobs/{id}/cancel", p.handleManagedCleanupCancel)
}

// Each batch's deletion and progress checkpoint commit together. Multiple
// processes serialize on the same job lock; shutdown rolls back an unfinished
// batch. A running job is therefore safely resumable without replaying counts.
func (p *Proxy) runManagedCleanupBatch(ctx context.Context, id string) error {
	db := p.Store.DB
	if db == nil {
		return errInvalid
	}
	name, err := managementStateKey("cleanup-jobs", id)
	if err != nil {
		return err
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
		return safeDBError(err)
	}
	var entry managementStateEntry
	if err = db.readDocument(ctx, tx, name, &entry); err != nil {
		return err
	}
	if entry.Version == 0 {
		return errNotFound
	}
	var job managedCleanupJob
	if err = json.Unmarshal(entry.Payload, &job); err != nil {
		return err
	}
	if !stringIn(job.Status, "pending", "running") {
		return nil
	}
	previewID := strings.TrimPrefix(job.ID, "cleanup_")
	var removed, cleared int64
	err = tx.QueryRow(ctx, `WITH selected AS (SELECT log_id FROM management_cleanup_targets WHERE preview_id=$1 ORDER BY log_id LIMIT 1000),
 deleted_logs AS (DELETE FROM request_logs l USING selected s WHERE l.id=s.log_id RETURNING l.id),
 cleared_targets AS (DELETE FROM management_cleanup_targets t USING selected s WHERE t.preview_id=$1 AND t.log_id=s.log_id RETURNING t.log_id)
 SELECT (SELECT count(*) FROM deleted_logs),(SELECT count(*) FROM cleared_targets)`, previewID).Scan(&removed, &cleared)
	if err != nil {
		return safeDBError(err)
	}
	job.Deleted += removed
	if job.StartedAt == 0 {
		job.StartedAt = time.Now().Unix()
	}
	job.Status = "running"
	// Targets, not deleted logs, drive the checkpoint: overlapping jobs or
	// retention may already have removed a detail row. The same job lock and
	// transaction serialize target removal with this persisted count. Counting
	// the entire shrinking target set after every 1000 records made large jobs
	// quadratic in their initial size.
	if cleared < 1000 {
		job.Remaining = 0
	} else if job.Remaining >= cleared {
		job.Remaining -= cleared
	} else {
		// Recover an older/inconsistent checkpoint without making its remaining
		// work negative. Normal jobs never need this full reconciliation scan.
		if err = tx.QueryRow(ctx, "SELECT count(*) FROM management_cleanup_targets WHERE preview_id=$1", previewID).Scan(&job.Remaining); err != nil {
			return safeDBError(err)
		}
	}
	if job.Remaining == 0 {
		job.Status = "succeeded"
		job.FinishedAt = time.Now().Unix()
		if err = db.audit(ctx, tx, "usage_details.cleanup", job.ID); err != nil {
			return safeDBError(err)
		}
	}
	entry.Version++
	entry.UpdatedAt = time.Now().Unix()
	entry.Payload, _ = json.Marshal(job)
	if err = db.writeDocument(ctx, tx, name, entry); err != nil {
		return err
	}
	return safeDBError(tx.Commit(ctx))
}

func (p *Proxy) processManagedCleanupJobs(ctx context.Context) error {
	if p.Store.DB == nil {
		return nil
	}
	entries, err := p.managementState().List(ctx, "cleanup-jobs")
	if err != nil {
		return err
	}
	for id, entry := range entries {
		var job managedCleanupJob
		if err = json.Unmarshal(entry.Payload, &job); err != nil {
			return err
		}
		if !stringIn(job.Status, "pending", "running") {
			continue
		}
		batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = p.runManagedCleanupBatch(batchCtx, id)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A canceled/stopped process leaves resumable state; a real batch failure
			// gets an explicit terminal error and never masquerades as completion.
			failure := redactMetricError(err.Error())
			if updateErr := p.managementState().Update(ctx, "cleanup-jobs", id, func(e *managementStateEntry) error {
				var latest managedCleanupJob
				if err := json.Unmarshal(e.Payload, &latest); err != nil {
					return err
				}
				if stringIn(latest.Status, "pending", "running") {
					latest.Status = "failed"
					latest.Error = failure
					latest.FinishedAt = time.Now().Unix()
					e.Payload, _ = json.Marshal(latest)
				}
				return nil
			}); updateErr != nil {
				return updateErr
			}
		}
	}
	return nil
}

func (p *Proxy) startManagementWorkers(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(4)
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			p.processManagedAutoResets(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			p.processManagedAccountSchedules(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := p.processManagedCleanupJobs(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("management cleanup worker: %s", redactMetricError(err.Error()))
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer workers.Done()
		for {
			more, err := p.pruneManagementEphemeralPass(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("management state cleanup: %s", redactMetricError(err.Error()))
			}
			// Backlogs and temporary failures continue promptly instead of
			// waiting five minutes for each bounded batch. A separate worker
			// keeps actual cleanup-job progress independent from housekeeping.
			timer := time.NewTimer(managedEphemeralPruneDelay(more, err))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	go func() { workers.Wait(); close(done) }()
	return done
}

func (p *Proxy) createManagedCleanupJob(ctx context.Context, preview storedCleanupPreview, job *managedCleanupJob) error {
	db := p.Store.DB
	name, _ := managementStateKey("cleanup-jobs", job.ID)
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return safeDBError(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,91823017))", name); err != nil {
		return safeDBError(err)
	}
	var entry managementStateEntry
	if err = db.readDocument(ctx, tx, name, &entry); err != nil {
		return err
	}
	if entry.Version > 0 {
		var old managedCleanupJob
		if err = json.Unmarshal(entry.Payload, &old); err != nil {
			return err
		}
		if old.RequestID != job.RequestID {
			return errVersionConflict
		}
		*job = old
		return nil
	}
	if preview.ExpiresAt <= time.Now().Unix() {
		return fmt.Errorf("%w: cleanup preview expired", errInvalid)
	}
	retained, err := tx.Exec(ctx, "UPDATE management_cleanup_targets SET expires_at=NULL WHERE preview_id=$1", preview.ID)
	if err != nil {
		return safeDBError(err)
	}
	job.Remaining = retained.RowsAffected()
	entry.Version = 1
	entry.UpdatedAt = time.Now().Unix()
	entry.Payload, _ = json.Marshal(job)
	if err = db.writeDocument(ctx, tx, name, entry); err != nil {
		return err
	}
	if err = db.audit(ctx, tx, "usage_details.cleanup.create", job.ID); err != nil {
		return safeDBError(err)
	}
	return safeDBError(tx.Commit(ctx))
}

const (
	managedEphemeralPruneBatchSize = 1000
	managedEphemeralPruneBatches   = 8
)

func managedEphemeralPruneDelay(more bool, err error) time.Duration {
	if more || err != nil {
		return 2 * time.Second
	}
	return 5 * time.Minute
}

func (p *Proxy) pruneManagementEphemeral(ctx context.Context) error {
	_, err := p.pruneManagementEphemeralPass(ctx)
	return err
}

// Each statement commits at most 1000 removed targets. Expiration uses the
// existing expiry index to pick one oldest preview, then the primary key to
// select its IDs in order. Globally ordering expiry ties by ID would sort an
// entire million-row snapshot again for every small batch.
func (p *Proxy) pruneManagedTargetBatch(ctx context.Context, cutoff time.Time, terminal []string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	batchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	statement := `WITH oldest AS MATERIALIZED (
 SELECT preview_id FROM management_cleanup_targets WHERE expires_at<=$1
 ORDER BY expires_at LIMIT 1 FOR UPDATE SKIP LOCKED
 ), selected AS MATERIALIZED (
 SELECT t.preview_id,t.log_id FROM management_cleanup_targets t
 WHERE t.preview_id=(SELECT preview_id FROM oldest) AND t.expires_at<=$1
 ORDER BY t.log_id LIMIT $2 FOR UPDATE OF t SKIP LOCKED
 ) DELETE FROM management_cleanup_targets t USING selected s
 WHERE t.preview_id=s.preview_id AND t.log_id=s.log_id AND t.expires_at<=$1`
	args := []any{cutoff, managedEphemeralPruneBatchSize}
	if terminal != nil {
		if len(terminal) == 0 {
			return 0, nil
		}
		statement = `WITH selected AS MATERIALIZED (
 SELECT preview_id,log_id FROM management_cleanup_targets
 WHERE preview_id=ANY($1::text[]) ORDER BY preview_id,log_id
 LIMIT $2 FOR UPDATE SKIP LOCKED
 ) DELETE FROM management_cleanup_targets t USING selected s
 WHERE t.preview_id=s.preview_id AND t.log_id=s.log_id`
		args[0] = terminal
	}
	result, err := p.Store.DB.Pool.Exec(batchCtx, statement, args...)
	if err != nil {
		if batchCtx.Err() != nil {
			return 0, batchCtx.Err()
		}
		return 0, safeDBError(err)
	}
	return result.RowsAffected(), nil
}

func (p *Proxy) pruneManagementEphemeralPass(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, ns := range []string{"account-oauth", "cleanup-preview", "crs-preview", "usage-snapshot", "lease-restore", "cost-restore"} {
		if err := p.managementState().PruneExpired(ctx, ns); err != nil {
			return false, err
		}
	}
	if p.Store.DB == nil {
		return false, nil
	}
	entries, err := p.managementState().List(ctx, "cleanup-jobs")
	if err != nil {
		return false, err
	}
	terminal := []string{}
	for _, entry := range entries {
		var job managedCleanupJob
		if err = json.Unmarshal(entry.Payload, &job); err != nil {
			return false, err
		}
		if stringIn(job.Status, "succeeded", "failed", "canceled") {
			terminal = append(terminal, strings.TrimPrefix(job.ID, "cleanup_"))
		}
	}
	sort.Strings(terminal)
	cutoff := time.Now()
	// Separate allowances prevent a stream of expired read snapshots from
	// starving terminal jobs, whose retained targets have expires_at=NULL.
	for _, selected := range [][]string{nil, terminal} {
		for range managedEphemeralPruneBatches {
			removed, err := p.pruneManagedTargetBatch(ctx, cutoff, selected)
			if err != nil {
				return true, err
			}
			if removed == 0 {
				break
			}
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var more bool
	err = p.Store.DB.Pool.QueryRow(probeCtx, `SELECT EXISTS(
 SELECT 1 FROM management_cleanup_targets WHERE expires_at<=$1)
 OR EXISTS(SELECT 1 FROM management_cleanup_targets WHERE preview_id=ANY($2::text[]))`, cutoff, terminal).Scan(&more)
	if err != nil {
		if probeCtx.Err() != nil {
			return true, probeCtx.Err()
		}
		return true, safeDBError(err)
	}
	return more, nil
}
