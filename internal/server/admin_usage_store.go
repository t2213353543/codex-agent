package server

import (
	"bufio"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

type managedLogRecord struct {
	RequestRecord
	Stream              bool   `json:"stream"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	LatencyMS           *int64 `json:"latency_ms"`
	ErrorCategory       string `json:"error_category,omitempty"`
	ErrorSeverity       string `json:"error_severity,omitempty"`
	ErrorSeveritySource string `json:"error_severity_source,omitempty"`
	UsageKnown          *bool  `json:"usage_known"`
	ModelConsistency    string `json:"model_consistency"`
}

func managedUsageKnown(r RequestRecord) *bool {
	if r.UsageKnown != nil {
		return r.UsageKnown
	}
	if r.InputTokens != 0 || r.OutputTokens != 0 || r.CachedTokens != 0 {
		known := true
		return &known
	}
	return nil
}

func managedRecord(r RequestRecord) managedLogRecord {
	severity, severitySource := managedErrorSeverity(r), ""
	if severity != "" {
		severitySource = managedErrorSeveritySource
	}
	r.Error = redactMetricError(r.Error)
	r.UsageAmount = ""
	r.WindowCostUSD = nil
	r.WindowCostPriceVersion = ""
	r.RolloutBudgetUnits = nil
	return managedLogRecord{RequestRecord: r, Stream: r.Stream, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CachedTokens: r.CachedTokens, LatencyMS: r.measuredLatency(), ErrorCategory: managedFailureCategory(r), ErrorSeverity: severity, ErrorSeveritySource: severitySource, UsageKnown: managedUsageKnown(r), ModelConsistency: gatewayModelConsistency(r)}
}

type managedLogPage struct {
	Items      []managedLogRecord `json:"items"`
	Total      int64              `json:"total"`
	TotalKind  string             `json:"total_kind"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor"`
	Page       int                `json:"page"`
	PageSize   int                `json:"page_size"`
	Scope      string             `json:"scope"`
}

func (m *Metrics) managedLogs(ctx context.Context, q managedLogQuery) (managedLogPage, error) {
	page := managedLogPage{Items: []managedLogRecord{}, Total: -1, TotalKind: "not_computed", Page: q.Page, PageSize: q.Size, Scope: "retained_request_details"}
	if q.Size < 1 || q.Size > 500 || q.Page < 1 || q.Page > 100 {
		return page, errInvalid
	}
	var cursor managedLogCursor
	var err error
	if q.Cursor != "" {
		cursor, err = q.decodeCursor()
		if err != nil {
			return page, err
		}
		if err := m.resolveManagedModelBoundary(ctx, &cursor); err != nil {
			return page, err
		}
	}
	if m.db != nil {
		return m.managedLogsPG(ctx, q, cursor, page)
	}
	if cursor.Version != 0 && cursor.Storage != "file" {
		return page, errInvalid
	}
	for n := 0; n < q.Page; n++ {
		h := &managedRecordHeap{query: q}
		heap.Init(h)
		var total int64
		boundary := cursor.Ceiling
		if q.AsOf != "" {
			boundary = q.FileCeiling
		}
		ceiling, err := m.walkManagedFile(ctx, boundary, func(r RequestRecord) error {
			if !q.matches(r) {
				return nil
			}
			total++
			if cursor.ID > 0 && !q.afterCursor(r, cursor) {
				return nil
			}
			if h.Len() < q.Size+1 {
				heap.Push(h, r)
			} else if q.before(r, h.items[0]) {
				h.items[0] = r
				heap.Fix(h, 0)
			}
			return nil
		})
		if err != nil {
			return page, err
		}
		sort.Slice(h.items, func(i, j int) bool { return q.before(h.items[i], h.items[j]) })
		page.Total = total
		page.TotalKind = "exact"
		page.HasMore = len(h.items) > q.Size
		if page.HasMore {
			h.items = h.items[:q.Size]
		}
		page.Items = []managedLogRecord{}
		for _, r := range h.items {
			page.Items = append(page.Items, managedRecord(r))
		}
		page.NextCursor = ""
		if page.HasMore {
			page.NextCursor = q.encodeCursor(h.items[len(h.items)-1], ceiling, "file")
		}
		if n+1 < q.Page {
			if !page.HasMore {
				page.Items = []managedLogRecord{}
				return page, nil
			}
			q.Cursor = page.NextCursor
			cursor, _ = q.decodeCursor()
			if err := m.resolveManagedModelBoundary(ctx, &cursor); err != nil {
				return page, err
			}
		}
	}
	return page, nil
}

// Keep at most one page plus one lookahead while scanning a file. File mode
// deliberately reads the complete snapshot, not the old 100k-line suffix.
type managedRecordHeap struct {
	query managedLogQuery
	items []RequestRecord
}

func (h managedRecordHeap) Len() int           { return len(h.items) }
func (h managedRecordHeap) Less(i, j int) bool { return h.query.before(h.items[j], h.items[i]) }
func (h managedRecordHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *managedRecordHeap) Push(x any)        { h.items = append(h.items, x.(RequestRecord)) }
func (h *managedRecordHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

func (m *Metrics) walkManagedFile(ctx context.Context, ceiling int64, visit func(RequestRecord) error) (int64, error) {
	if ceiling < 0 {
		return 0, ctx.Err()
	}
	m.mu.Lock()
	file, err := os.Open(m.logPath())
	m.mu.Unlock()
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("usage log is not a regular file")
	}
	if ceiling == 0 {
		ceiling = info.Size()
	}
	if ceiling > info.Size() {
		return 0, fmt.Errorf("%w: log snapshot no longer available", errInvalid)
	}
	scanner := bufio.NewScanner(io.NewSectionReader(file, 0, ceiling))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var line int64
	for scanner.Scan() {
		if err = queryCanceled(ctx); err != nil {
			return ceiling, err
		}
		line++
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var rec RequestRecord
		if err = json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return ceiling, fmt.Errorf("invalid usage record at line %d", line)
		}
		// Append-only line number gives old records a stable identity across pages.
		if rec.ID == 0 {
			rec.ID = line
		}
		if err = visit(rec); err != nil {
			return ceiling, err
		}
	}
	return ceiling, scanner.Err()
}

func (m *Metrics) managedLogsPG(ctx context.Context, q managedLogQuery, cursor managedLogCursor, page managedLogPage) (managedLogPage, error) {
	if cursor.Version != 0 && cursor.Storage != "postgres" {
		return page, errInvalid
	}
	ceiling := cursor.Ceiling
	if ceiling == 0 {
		if err := m.db.Pool.QueryRow(ctx, "SELECT COALESCE(max(id),0) FROM request_logs").Scan(&ceiling); err != nil {
			return page, err
		}
	}
	source, base, args := q.readSourceSQL()
	args = append(args, ceiling)
	base += fmt.Sprintf(" AND id<=$%d", len(args))
	if q.ExactTotal {
		total, err := m.managedExactTotal(ctx, q, ceiling, source, base, args)
		if err != nil {
			return page, err
		}
		page.Total = total
		page.TotalKind = "exact"
	}
	where := base
	params := append([]any{}, args...)
	if cursor.ID > 0 {
		op := "<"
		if q.SortOrder == "asc" {
			op = ">"
		}
		if q.SortBy == "time" {
			params = append(params, time.Unix(cursor.Time, 0).UTC(), cursor.ID)
			where += fmt.Sprintf(" AND (created_at,id)%s($%d,$%d)", op, len(params)-1, len(params))
		} else {
			var value any = cursor.Value
			if q.SortBy == "model" {
				value = cursor.Text
			}
			stamp := time.Unix(cursor.Time, 0).UTC()
			if cursor.ExactTime != nil {
				stamp = *cursor.ExactTime
			}
			params = append(params, value, stamp, cursor.ID)
			where += fmt.Sprintf(" AND (%s,created_at,id)%s($%d,$%d,$%d)", q.sortSQL(), op, len(params)-2, len(params)-1, len(params))
		}
	}
	// Numbered pages are bounded at 100. Let PostgreSQL skip preceding rows
	// without transferring or decoding every earlier page; cursor requests
	// still use the indexed seek predicate above.
	statement, params := managedLogPageSQL(q, source, where, params)
	rows, err := queryManagedLogPage(ctx, m.db.Pool, q, statement, params...)
	if err != nil {
		return page, err
	}
	items := make([]RequestRecord, 0, q.Size+1)
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			break
		}
		var r RequestRecord
		if err = json.Unmarshal(raw, &r); err != nil {
			break
		}
		r.ID = id
		items = append(items, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	page.HasMore = len(items) > q.Size
	if page.HasMore {
		items = items[:q.Size]
	}
	page.Items = make([]managedLogRecord, 0, len(items))
	for _, r := range items {
		page.Items = append(page.Items, managedRecord(r))
	}
	page.NextCursor = ""
	if page.HasMore {
		page.NextCursor = q.encodeCursor(items[len(items)-1], ceiling, "postgres")
	}
	return page, nil
}

// Full-query iteration uses a single database snapshot. It streams raw records
// to an aggregator or a temporary export file rather than buffering all rows.
func (m *Metrics) walkManagedLogs(ctx context.Context, q managedLogQuery, visit func(RequestRecord) error) error {
	if m.db == nil {
		_, err := m.walkManagedFile(ctx, q.FileCeiling, func(r RequestRecord) error {
			if q.matches(r) {
				return visit(r)
			}
			return nil
		})
		return err
	}
	source, where, args := q.readSourceSQL()
	rows, err := m.db.Pool.Query(ctx, "SELECT id,record FROM "+source+" WHERE "+where+" ORDER BY "+q.orderSQL(), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		var r RequestRecord
		if err = json.Unmarshal(raw, &r); err != nil {
			return err
		}
		r.ID = id
		if err = visit(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (m *Metrics) managedLogDetail(ctx context.Context, id string) (RequestRecord, error) {
	var rec RequestRecord
	if m.db != nil {
		var raw []byte
		// Keep the columns uncast so both the primary key and event index can
		// serve this point lookup. Only canonical IDs matched the old text cast.
		where, args := "event_id=$1", []any{id}
		if recordID, err := strconv.ParseInt(id, 10, 64); err == nil && strconv.FormatInt(recordID, 10) == id {
			where = "id=$2 OR event_id=$1"
			args = append(args, recordID)
		}
		err := m.db.Pool.QueryRow(ctx, "SELECT id,record FROM request_logs WHERE "+where+" ORDER BY id DESC LIMIT 1", args...).Scan(&rec.ID, &raw)
		if err != nil {
			return rec, err
		}
		recordID := rec.ID
		err = json.Unmarshal(raw, &rec)
		rec.ID = recordID
		return rec, err
	}
	found := false
	_, err := m.walkManagedFile(ctx, 0, func(r RequestRecord) error {
		if strconv.FormatInt(r.ID, 10) == id || r.EventID == id {
			rec = r
			found = true
		}
		return nil
	})
	if err != nil {
		return rec, err
	}
	if !found {
		return rec, errManagedLogNotFound
	}
	return rec, nil
}
