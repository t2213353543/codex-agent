package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Short-lived management snapshots materialize IDs, not request contents.
// Existing target storage also owns TTL cleanup; usage_ IDs are never jobs.
type managedUsageSnapshot struct {
	ID        string `json:"as_of"`
	Owner     string `json:"-"`
	Filter    string `json:"filter"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
	Scope     string `json:"scope"`
	Total     int64  `json:"total"`
	Storage   string `json:"storage"`
	Ceiling   int64  `json:"ceiling"`
}
type storedUsageSnapshot struct {
	managedUsageSnapshot
	OwnerHash string `json:"owner_hash"`
}

func (q managedLogQuery) snapshotFilter() string {
	q.AsOf = ""
	q.SnapshotID = ""
	q.FileCeiling = 0
	q.SortBy = ""
	q.SortOrder = ""
	q.Timezone = ""
	q.Step = 0
	return q.filterHash()
}

func (p *Proxy) handleManagedUsageSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filters map[string]string `json:"filters"`
	}
	if err := readJSON(w, r, &body); err != nil {
		adminError(w, err)
		return
	}
	values := url.Values{}
	for key, value := range body.Filters {
		if key == "as_of" || key == "cursor" || key == "page" {
			adminError(w, errInvalid)
			return
		}
		values.Set(key, value)
	}
	q, err := parseManagedLogQuery(values)
	if err != nil {
		adminError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	owner := managedOperationOwner(ctx)
	if owner == "" {
		adminAPIError(w, 401, "unauthorized", "admin authentication required")
		return
	}
	snapshot := managedUsageSnapshot{ID: newID("usage_"), Filter: q.snapshotFilter(), CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Scope: managedCleanupScope, Storage: "file"}
	if p.Store.DB != nil {
		snapshot.Storage = "postgres"
		tx, err := p.Store.DB.Pool.Begin(ctx)
		if err != nil {
			p.storeError(w, err)
			return
		}
		defer tx.Rollback(context.Background())
		source, where, args := q.readSourceSQL()
		args = append(args, snapshot.ID, time.Unix(snapshot.ExpiresAt, 0))
		statement := "INSERT INTO management_cleanup_targets(preview_id,log_id,expires_at) SELECT $" + strconv.Itoa(len(args)-1) + ",id,$" + strconv.Itoa(len(args)) + " FROM " + source + " WHERE " + where
		result, err := tx.Exec(ctx, statement, args...)
		if err != nil {
			p.storeError(w, err)
			return
		}
		snapshot.Total = result.RowsAffected()
		payload, _ := json.Marshal(storedUsageSnapshot{snapshot, owner})
		entry := managementStateEntry{Version: 1, ExpiresAt: snapshot.ExpiresAt, UpdatedAt: snapshot.CreatedAt, Payload: payload}
		name, _ := managementStateKey("usage-snapshot", snapshot.ID)
		if err = p.Store.DB.writeDocument(ctx, tx, name, entry); err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			p.storeError(w, err)
			return
		}
	} else {
		ceiling, err := p.Metrics.walkManagedFile(ctx, 0, func(record RequestRecord) error {
			if q.matches(record) {
				snapshot.Total++
			}
			return nil
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			p.storeError(w, err)
			return
		}
		snapshot.Ceiling = ceiling
		if err = p.managementState().Update(ctx, "usage-snapshot", snapshot.ID, func(entry *managementStateEntry) error {
			entry.ExpiresAt = snapshot.ExpiresAt
			entry.Payload, err = json.Marshal(storedUsageSnapshot{snapshot, owner})
			return err
		}); err != nil {
			p.storeError(w, err)
			return
		}
	}
	writeJSON(w, map[string]any{"as_of": snapshot.ID, "created_at": snapshot.CreatedAt, "expires_at": snapshot.ExpiresAt, "scope": snapshot.Scope, "total": snapshot.Total, "consistency": "fixed_record_ids; concurrently_removed_details_are_unavailable"})
}

func (p *Proxy) resolveManagedUsageSnapshot(ctx context.Context, q *managedLogQuery) error {
	if q.AsOf == "" {
		return nil
	}
	entry, err := p.managementState().Get(ctx, "usage-snapshot", q.AsOf)
	if errors.Is(err, errNotFound) {
		return errVersionConflict
	}
	if err != nil {
		return err
	}
	var snapshot storedUsageSnapshot
	if err = json.Unmarshal(entry.Payload, &snapshot); err != nil {
		return err
	}
	if snapshot.OwnerHash != managedOperationOwner(ctx) || snapshot.Filter != q.snapshotFilter() {
		return errConflict
	}
	if (p.Store.DB != nil) != (snapshot.Storage == "postgres") {
		return errConflict
	}
	if snapshot.Storage == "postgres" {
		q.SnapshotID = snapshot.ID
	} else {
		q.FileCeiling = snapshot.Ceiling
		if q.FileCeiling == 0 {
			q.FileCeiling = -1
		}
	}
	return nil
}

func (m *Metrics) managedDetailInSnapshot(ctx context.Context, q managedLogQuery, record RequestRecord) (bool, error) {
	if !q.matches(record) {
		return false, nil
	}
	if q.AsOf == "" {
		return true, nil
	}
	if m.db != nil {
		var member bool
		err := m.db.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM management_cleanup_targets WHERE preview_id=$1 AND log_id=$2)", q.SnapshotID, record.ID).Scan(&member)
		return member, err
	}
	member := false
	_, err := m.walkManagedFile(ctx, q.FileCeiling, func(candidate RequestRecord) error {
		if candidate.ID == record.ID && candidate.EventID == record.EventID {
			member = true
		}
		return nil
	})
	return member, err
}
