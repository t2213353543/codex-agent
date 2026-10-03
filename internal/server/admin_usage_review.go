package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const managedUsageReviewHistoryLimit = 50

// Review state is a human management annotation. It does not change a request's
// original outcome, accounting, account availability or gateway retry behavior.
type managedUsageReviewEvent struct {
	Version  int64  `json:"version"`
	Resolved bool   `json:"resolved"`
	At       int64  `json:"at"`
	Actor    string `json:"actor"`
	Note     string `json:"note"`
}

type managedUsageReview struct {
	RecordRef        string                    `json:"record_ref"`
	Version          int64                     `json:"version"`
	Resolved         bool                      `json:"resolved"`
	ResolvedAt       int64                     `json:"resolved_at"`
	ResolvedBy       string                    `json:"resolved_by"`
	UpdatedAt        int64                     `json:"updated_at"`
	UpdatedBy        string                    `json:"updated_by"`
	Note             string                    `json:"note"`
	History          []managedUsageReviewEvent `json:"history"`
	HistoryTotal     int64                     `json:"history_total"`
	HistoryTruncated bool                      `json:"history_truncated"`
}

type managedUsageReviewIdentity struct {
	ID        int64  `json:"id"`
	EventID   string `json:"event_id"`
	Time      int64  `json:"time"`
	AccountID string `json:"account_id"`
	Model     string `json:"model"`
	Status    string `json:"status"`
}

type storedUsageReview struct {
	Review managedUsageReview         `json:"review"`
	Record managedUsageReviewIdentity `json:"record"`
}

func managedUsageReviewKey(record RequestRecord) string {
	if record.EventID != "" {
		return adminKeyHash("usage-event:" + record.EventID)
	}
	// Legacy file offsets can change after pruning. Do not bind their reviews
	// to a line number that a different request could later occupy.
	record.ID = 0
	record.Error = redactMetricError(record.Error)
	raw, _ := json.Marshal(record)
	return adminKeyHash("usage-record:" + string(raw))
}

func managedUsageReviewRecord(record RequestRecord) managedUsageReviewIdentity {
	return managedUsageReviewIdentity{record.ID, record.EventID, record.Time, record.AccountID, record.Model, record.Status}
}

func emptyManagedUsageReview(record RequestRecord) managedUsageReview {
	return managedUsageReview{RecordRef: managedUsageReviewKey(record), History: []managedUsageReviewEvent{}}
}

func decodeManagedUsageReview(entry managementStateEntry) (storedUsageReview, error) {
	var stored storedUsageReview
	if err := json.Unmarshal(entry.Payload, &stored); err != nil {
		return stored, err
	}
	stored.Review.Version = entry.Version
	if stored.Review.History == nil {
		stored.Review.History = []managedUsageReviewEvent{}
	}
	return stored, nil
}

func (p *Proxy) managedUsageReview(ctx context.Context, record RequestRecord) (managedUsageReview, error) {
	entry, err := p.managementState().Get(ctx, "usage-review", managedUsageReviewKey(record))
	if errors.Is(err, errNotFound) {
		return emptyManagedUsageReview(record), nil
	}
	if err != nil {
		return managedUsageReview{}, err
	}
	stored, err := decodeManagedUsageReview(entry)
	return stored.Review, err
}

func (p *Proxy) managedUsageReviewTarget(w http.ResponseWriter, r *http.Request) (RequestRecord, bool) {
	id := r.PathValue("id")
	if id == "" || len(id) > 200 {
		adminError(w, errInvalid)
		return RequestRecord{}, false
	}
	record, err := p.Metrics.managedLogDetail(r.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errManagedLogNotFound) {
		adminAPIError(w, http.StatusNotFound, "not_found", "usage record not found")
		return record, false
	}
	if err != nil {
		p.storeError(w, err)
		return record, false
	}
	if record.Status == "ok" {
		adminAPIError(w, http.StatusBadRequest, "not_failed", "only failed or aborted requests have error review state")
		return record, false
	}
	return record, true
}

func (p *Proxy) handleManagedUsageReview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), metricsTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	record, ok := p.managedUsageReviewTarget(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		review, err := p.managedUsageReview(ctx, record)
		if err != nil {
			p.storeError(w, err)
			return
		}
		writeJSON(w, map[string]any{"review": review, "record": managedUsageReviewRecord(record), "scope": "management_review"})
		return
	}
	var body struct {
		Version  *int64 `json:"version"`
		Resolved *bool  `json:"resolved"`
		Note     string `json:"note"`
	}
	if err := readJSON(w, r, &body); err != nil {
		adminError(w, err)
		return
	}
	if body.Version == nil || *body.Version < 0 || body.Resolved == nil || !utf8.ValidString(body.Note) || utf8.RuneCountInString(body.Note) > 1000 || strings.ContainsRune(body.Note, '\x00') {
		adminError(w, errInvalid)
		return
	}
	actor := managedOperationOwner(ctx)
	if actor == "" {
		adminAPIError(w, http.StatusUnauthorized, "unauthorized", "admin authentication required")
		return
	}
	// Unlike free-form upstream errors, a validated operator note is not silently
	// byte-truncated (1000 Chinese characters can exceed the metric error limit).
	note := bearerInMetricError.ReplaceAllString(strings.TrimSpace(body.Note), "${1}[REDACTED]")
	note = proxyURLInMetricError.ReplaceAllString(note, "${1}[REDACTED]@")
	note = secretInMetricError.ReplaceAllString(note, "${1}[REDACTED]")
	note = jwtInMetricError.ReplaceAllString(note, "[REDACTED]")
	key := managedUsageReviewKey(record)
	var review managedUsageReview
	err := p.managementState().UpdateAudited(ctx, "usage-review", key, "usage_review.update", func(entry *managementStateEntry) error {
		if entry.Version != *body.Version {
			return errVersionConflict
		}
		review = emptyManagedUsageReview(record)
		if entry.Version > 0 {
			stored, err := decodeManagedUsageReview(*entry)
			if err != nil {
				return err
			}
			review = stored.Review
		}
		now := time.Now().Unix()
		review.Version = entry.Version + 1
		review.Resolved = *body.Resolved
		review.Note = note
		review.UpdatedAt, review.UpdatedBy = now, actor
		review.ResolvedAt, review.ResolvedBy = 0, ""
		if review.Resolved {
			review.ResolvedAt, review.ResolvedBy = now, actor
		}
		review.HistoryTotal++
		review.History = append(review.History, managedUsageReviewEvent{review.Version, review.Resolved, now, actor, note})
		if len(review.History) > managedUsageReviewHistoryLimit {
			review.History = review.History[len(review.History)-managedUsageReviewHistoryLimit:]
		}
		review.HistoryTruncated = review.HistoryTotal > int64(len(review.History))
		entry.Payload, _ = json.Marshal(storedUsageReview{review, managedUsageReviewRecord(record)})
		return nil
	})
	if err != nil {
		p.storeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"review": review, "record": managedUsageReviewRecord(record), "scope": "management_review"})
}

func (p *Proxy) handleManagedUsageErrors(w http.ResponseWriter, r *http.Request) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values["cursor"]) > 1 {
		adminError(w, errInvalid)
		return
	}
	// The cursor also binds the current resolved set, which is loaded below.
	// Delay its signature check until these internal filters are available.
	cursor := values.Get("cursor")
	values.Del("cursor")
	q, err := parseManagedLogQuery(values)
	if err != nil {
		adminError(w, err)
		return
	}
	q.Cursor = cursor
	if q.AsOf != "" || q.Status != "" && q.Status != "failed" || q.Cursor != "" && q.Page > 1 {
		adminError(w, errInvalid)
		return
	}
	q.Status, q.ExactTotal = "failed", true
	if value := r.URL.Query().Get("resolved"); value != "" {
		if value != "true" && value != "false" {
			adminError(w, errInvalid)
			return
		}
		resolved := value == "true"
		q.ReviewOnly = &resolved
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var entries map[string]managementStateEntry
	if q.ReviewOnly != nil {
		entries, err = p.managementState().List(ctx, "usage-review")
		if err != nil {
			p.storeError(w, err)
			return
		}
	}
	reviews := make(map[string]managedUsageReview, len(entries))
	for key, entry := range entries {
		stored, err := decodeManagedUsageReview(entry)
		if err != nil || stored.Review.RecordRef != key {
			if err == nil {
				err = errInvalid
			}
			p.storeError(w, err)
			return
		}
		// Histories are obtained only from the independent detail endpoint.
		stored.Review.History = []managedUsageReviewEvent{}
		reviews[key] = stored.Review
		if stored.Review.Resolved && q.ReviewOnly != nil {
			q.ReviewHashes = append(q.ReviewHashes, key)
			if stored.Record.EventID != "" {
				q.ReviewEventIDs = append(q.ReviewEventIDs, stored.Record.EventID)
			} else {
				q.ReviewRecordIDs = append(q.ReviewRecordIDs, stored.Record.ID)
			}
		}
	}
	// Cursor signatures must be independent of map iteration order. Changes to
	// the resolved set invalidate its old pagination instead of silently skipping.
	sort.Strings(q.ReviewHashes)
	sort.Strings(q.ReviewEventIDs)
	sort.Slice(q.ReviewRecordIDs, func(i, j int) bool { return q.ReviewRecordIDs[i] < q.ReviewRecordIDs[j] })
	if q.Cursor != "" {
		if parsed, err := q.decodeCursor(); err != nil || parsed.Version != 1 && parsed.Version != 2 {
			adminAPIError(w, http.StatusConflict, "review_query_changed", "error review state or filters changed; refresh from the first page")
			return
		}
	}
	page, err := p.Metrics.managedLogs(ctx, q)
	if err != nil {
		p.storeError(w, err)
		return
	}
	if q.ReviewOnly == nil {
		// The unfiltered error page does not need the global resolved set.
		// Decode annotations only for visible records, independent of how many
		// historical reviews administrators have accumulated.
		keys := make([]string, len(page.Items))
		for i, record := range page.Items {
			keys[i] = managedUsageReviewKey(record.RequestRecord)
		}
		entries, err := p.managementState().GetMany(ctx, "usage-review", keys)
		if err != nil {
			p.storeError(w, err)
			return
		}
		for key, entry := range entries {
			stored, err := decodeManagedUsageReview(entry)
			if err != nil || stored.Review.RecordRef != key {
				if err == nil {
					err = errInvalid
				}
				p.storeError(w, err)
				return
			}
			stored.Review.History = []managedUsageReviewEvent{}
			reviews[key] = stored.Review
		}
	}
	type item struct {
		Record managedLogRecord   `json:"record"`
		Review managedUsageReview `json:"review"`
	}
	items := make([]item, 0, len(page.Items))
	for _, record := range page.Items {
		review, ok := reviews[managedUsageReviewKey(record.RequestRecord)]
		if !ok {
			review = emptyManagedUsageReview(record.RequestRecord)
		}
		items = append(items, item{record, review})
	}
	w.Header().Set("X-Review-Count", strconv.FormatInt(page.Total, 10))
	writeJSON(w, map[string]any{"items": items, "total": page.Total, "total_kind": page.TotalKind, "has_more": page.HasMore, "next_cursor": page.NextCursor, "page": page.Page, "page_size": page.PageSize, "checked_at": time.Now().Unix(), "scope": "retained_request_errors_with_current_management_review"})
}
