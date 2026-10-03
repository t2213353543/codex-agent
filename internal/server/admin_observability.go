package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const adminDiagnosticTimeout = 2 * time.Second
const adminAuditTimeout = 5 * time.Second

type adminDependencyDiagnostic struct {
	Configured bool   `json:"configured"`
	Status     string `json:"status"`
	LatencyMS  int64  `json:"latency_ms"`
	ErrorCode  string `json:"error_code,omitempty"`
}

type adminDiagnostics struct {
	Inference     map[string]any                       `json:"inference"`
	Version       string                               `json:"version"`
	CheckedAt     int64                                `json:"checked_at"`
	UptimeSeconds int64                                `json:"uptime_seconds"`
	StorageMode   string                               `json:"storage_mode"`
	Status        string                               `json:"status"`
	Dependencies  map[string]adminDependencyDiagnostic `json:"dependencies"`
	Capabilities  map[string]bool                      `json:"capabilities"`
}

// No driver error, address, configuration value, credential, or account data is
// included in diagnostics. The HTTP response has one shared deadline even if a
// driver has disabled context-based socket deadlines.
func adminCheckDependency(ctx context.Context, configured bool, ping func(context.Context) error) adminDependencyDiagnostic {
	result := adminDependencyDiagnostic{Configured: configured, Status: "not_configured"}
	if !configured {
		return result
	}
	start := time.Now()
	err := ctx.Err()
	if err != nil {
		// A canceled management request must not start new dependency work.
	} else if ping == nil {
		err = errors.New("dependency unavailable")
	} else {
		done := make(chan error, 1)
		go func() { done <- ping(ctx) }()
		select {
		case err = <-done:
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	result.LatencyMS = time.Since(start).Milliseconds()
	result.Status = "ok"
	if err != nil {
		result.Status, result.ErrorCode = "unavailable", "dependency_unavailable"
		var timeout net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
			result.Status, result.ErrorCode = "timeout", "dependency_timeout"
		} else if errors.Is(err, context.Canceled) {
			result.ErrorCode = "check_canceled"
		}
	}
	return result
}

func (p *Proxy) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), adminDiagnosticTimeout)
	defer cancel()
	result := adminDiagnostics{
		Inference: p.inferenceInfo(),
		Version:   version, CheckedAt: time.Now().Unix(), StorageMode: "file", Status: "ok",
		Dependencies: make(map[string]adminDependencyDiagnostic, 2),
		Capabilities: map[string]bool{"audit_logs": p.Store.DB != nil},
	}
	if !p.startedAt.IsZero() {
		result.UptimeSeconds = max(0, int64(time.Since(p.startedAt).Seconds()))
	}
	var pgPing, redisPing func(context.Context) error
	pgConfigured, redisConfigured := false, false
	if db := p.Store.DB; db != nil {
		result.StorageMode = "postgres"
		pgConfigured = true
		if db.Pool != nil {
			pgPing = db.Pool.Ping
		}
		if db.Redis != nil {
			redisConfigured = true
			redisPing = func(ctx context.Context) error {
				return db.Redis.WithTimeout(adminDiagnosticTimeout).Ping(ctx).Err()
			}
		}
	}
	type dependencyResult struct {
		name string
		data adminDependencyDiagnostic
	}
	checks := make(chan dependencyResult, 2)
	go func() { checks <- dependencyResult{"postgres", adminCheckDependency(ctx, pgConfigured, pgPing)} }()
	go func() { checks <- dependencyResult{"redis", adminCheckDependency(ctx, redisConfigured, redisPing)} }()
	for range 2 {
		check := <-checks
		result.Dependencies[check.name] = check.data
		if check.data.Configured && check.data.Status != "ok" || result.StorageMode == "postgres" && !check.data.Configured {
			result.Status = "degraded"
		}
	}
	writeJSON(w, result)
}

type adminAuditItem struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	EntityID  string `json:"entity_id"`
	Result    string `json:"result"`
}

type adminAuditPage struct {
	Supported   bool             `json:"supported"`
	StorageMode string           `json:"storage_mode"`
	Items       []adminAuditItem `json:"items"`
	NextCursor  string           `json:"next_cursor"`
	HasMore     bool             `json:"has_more"`
	Limit       int              `json:"limit"`
}

type adminAuditOptions struct {
	Limit    int
	Action   string
	EntityID string
	From     int64
	To       int64
	Cursor   *adminAuditCursor
}

type adminAuditCursor struct {
	Version int    `json:"v"`
	Time    string `json:"t"`
	ID      string `json:"id"`
	Filter  string `json:"filter"`
}

func (o adminAuditOptions) filterHash() string {
	raw, _ := json.Marshal([]any{o.Action, o.EntityID, o.From, o.To})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseAdminAuditOptions(q url.Values) (adminAuditOptions, error) {
	o := adminAuditOptions{Limit: 50, Action: q.Get("action"), EntityID: q.Get("entity_id")}
	for key, values := range q {
		switch key {
		case "limit", "action", "entity_id", "from", "to", "cursor":
		default:
			return o, errors.New("unsupported audit query parameter")
		}
		if len(values) != 1 {
			return o, errors.New("audit query parameters must not be repeated")
		}
	}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil || limit < 1 || limit > 200 {
			return o, errors.New("limit must be between 1 and 200")
		}
		o.Limit = limit
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{{"action", o.Action, 100}, {"entity_id", o.EntityID, 256}} {
		if len(field.value) > field.max || strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return o, fmt.Errorf("invalid audit %s", field.name)
		}
	}
	for _, field := range []struct {
		name  string
		value *int64
	}{{"from", &o.From}, {"to", &o.To}} {
		if v := q.Get(field.name); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 1 || n > 253402300799 {
				return o, errors.New("audit time must be positive Unix seconds")
			}
			*field.value = n
		}
	}
	if o.From > 0 && o.To > 0 && o.To <= o.From {
		return o, errors.New("to must be after from")
	}
	if raw := q.Get("cursor"); raw != "" {
		if len(raw) > 768 {
			return o, errors.New("invalid audit cursor")
		}
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return o, errors.New("invalid audit cursor")
		}
		var cursor adminAuditCursor
		decoder := json.NewDecoder(bytes.NewReader(decoded))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&cursor) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			return o, errors.New("invalid audit cursor")
		}
		stamp, err := time.Parse(time.RFC3339Nano, cursor.Time)
		id, idErr := strconv.ParseInt(cursor.ID, 10, 64)
		if err != nil || stamp.Year() < 1970 || stamp.Year() > 9999 || stamp.Nanosecond()%1000 != 0 || idErr != nil || id < 1 || cursor.Version != 1 || cursor.Filter != o.filterHash() {
			return o, errors.New("invalid audit cursor or changed filters")
		}
		o.Cursor = &cursor
	}
	return o, nil
}

func (p *Proxy) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	o, err := parseAdminAuditOptions(r.URL.Query())
	if err != nil {
		adminError(w, err)
		return
	}
	page := adminAuditPage{StorageMode: "file", Items: []adminAuditItem{}, Limit: o.Limit}
	db := p.Store.DB
	if db == nil {
		writeJSON(w, page)
		return
	}
	if db.Pool == nil {
		adminAPIError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit records are temporarily unavailable")
		return
	}
	page.Supported, page.StorageMode = true, "postgres"
	ctx, cancel := context.WithTimeout(r.Context(), adminAuditTimeout)
	defer cancel()
	where := []string{"true"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if o.Action != "" {
		add("action=$%d", o.Action)
	}
	if o.EntityID != "" {
		add("entity_id=$%d", o.EntityID)
	}
	if o.From > 0 {
		add("created_at >= $%d", time.Unix(o.From, 0))
	}
	if o.To > 0 {
		add("created_at < $%d", time.Unix(o.To, 0))
	}
	if o.Cursor != nil {
		stamp, _ := time.Parse(time.RFC3339Nano, o.Cursor.Time)
		id, _ := strconv.ParseInt(o.Cursor.ID, 10, 64)
		args = append(args, stamp, id)
		where = append(where, fmt.Sprintf("(created_at,id) < ($%d,$%d)", len(args)-1, len(args)))
	}
	args = append(args, o.Limit+1)
	query := `SELECT id,created_at,actor,action,entity_id,result FROM audit_logs WHERE ` + strings.Join(where, " AND ") + fmt.Sprintf(" ORDER BY created_at DESC,id DESC LIMIT $%d", len(args))
	rows, err := db.Pool.Query(ctx, query, args...)
	if err != nil {
		adminAPIError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit records are temporarily unavailable")
		return
	}
	defer rows.Close()
	var lastTime time.Time
	for rows.Next() {
		var item adminAuditItem
		var id int64
		var stamp time.Time
		if err = rows.Scan(&id, &stamp, &item.Actor, &item.Action, &item.EntityID, &item.Result); err != nil {
			break
		}
		if len(page.Items) == o.Limit {
			page.HasMore = true
			break
		}
		item.ID, item.CreatedAt = strconv.FormatInt(id, 10), stamp.Unix()
		page.Items = append(page.Items, item)
		lastTime = stamp
	}
	if err != nil || rows.Err() != nil {
		adminAPIError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit records are temporarily unavailable")
		return
	}
	if page.HasMore {
		last := page.Items[len(page.Items)-1]
		cursor, _ := json.Marshal(adminAuditCursor{Version: 1, Time: lastTime.UTC().Format(time.RFC3339Nano), ID: last.ID, Filter: o.filterHash()})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(cursor)
	}
	writeJSON(w, page)
}
