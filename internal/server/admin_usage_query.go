package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Management queries are separate from gateway admission, forwarding and
// durable accounting. A detail filter never changes the account's quota.
type managedLogQuery struct {
	SentModel, ReturnedModel, InboundPath, UpstreamPath      string
	SentModelMismatch, ReturnedModelMismatch, ErrorTruncated *bool
	statsQueryer                                             managedPGStatsQueryer
	// Internal projection used by the account batch endpoint; it is not a filter.
	accountsOnly bool
	// Only the separate error-review endpoint supplies these server-side filters.
	ReviewOnly                                                           *bool
	ReviewEventIDs                                                       []string
	ReviewRecordIDs                                                      []int64
	ReviewHashes                                                         []string
	AsOf, SnapshotID                                                     string
	FileCeiling                                                          int64
	AccountIDs                                                           []string
	Step                                                                 int64
	Timezone                                                             string
	UpstreamModel, RequestType, ServiceTier, ReasoningEffort, ErrorPhase string
	ModelMismatch, NativeCompaction                                      *bool
	WebSocketWarmup                                                      *bool
	From, To                                                             int64
	Account, AccountID, KeyName, KeyID, GroupID                          string
	Model, Endpoint, Status, RequestID, ErrorCategory, IP                string
	HTTPCode                                                             int
	ErrorSeverity                                                        string
	ClientRequestID                                                      string
	Stream                                                               *bool
	SortBy, SortOrder, Cursor                                            string
	Page, Size                                                           int
	ExactTotal                                                           bool
}

func parseManagedLogQuery(values url.Values) (managedLogQuery, error) {
	for _, entries := range values {
		if len(entries) > 1 {
			return managedLogQuery{}, fmt.Errorf("%w: repeated query parameter", errInvalid)
		}
	}
	if values.Get("size") != "" && values.Get("page_size") != "" && values.Get("size") != values.Get("page_size") {
		return managedLogQuery{}, errInvalid
	}
	q := managedLogQuery{Timezone: "Asia/Shanghai", SortBy: "time", SortOrder: "desc", Page: 1, Size: 50}
	q.AsOf = values.Get("as_of")
	q.ClientRequestID = values.Get("client_request_id")
	if q.ClientRequestID != "" && !gatewayClientRequestIDPattern.MatchString(q.ClientRequestID) {
		return q, fmt.Errorf("%w: invalid client request ID", errInvalid)
	}
	if len(q.AsOf) > 128 || q.AsOf != "" && !strings.HasPrefix(q.AsOf, "usage_") {
		return q, errInvalid
	}
	invalid := func() (managedLogQuery, error) { return q, fmt.Errorf("%w: invalid usage query", errInvalid) }
	for key, target := range map[string]*string{"sent_model": &q.SentModel, "returned_model": &q.ReturnedModel, "inbound_path": &q.InboundPath, "upstream_path": &q.UpstreamPath} {
		*target = values.Get(key)
		if len(*target) > 1024 {
			return invalid()
		}
	}
	for key, target := range map[string]*string{"account": &q.Account, "account_id": &q.AccountID, "key_name": &q.KeyName, "key_id": &q.KeyID, "group_id": &q.GroupID, "model": &q.Model, "endpoint": &q.Endpoint, "status": &q.Status, "request_id": &q.RequestID, "error_category": &q.ErrorCategory, "ip": &q.IP, "upstream_model": &q.UpstreamModel, "request_type": &q.RequestType, "service_tier": &q.ServiceTier, "reasoning_effort": &q.ReasoningEffort, "error_phase": &q.ErrorPhase} {
		*target = values.Get(key)
		if len(*target) > 1024 {
			return invalid()
		}
	}
	for key, target := range map[string]*int64{"from": &q.From, "to": &q.To, "step": &q.Step} {
		if raw := values.Get(key); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 1 || n > 253402300799 {
				return invalid()
			}
			*target = n
		}
	}
	if raw := values.Get("timezone"); raw != "" {
		q.Timezone = raw
	}
	if len(q.Timezone) > 100 {
		return invalid()
	}
	if _, err := time.LoadLocation(q.Timezone); err != nil {
		return invalid()
	}
	if q.Step != 0 && !validManagedUsageStep(q.Step) {
		return invalid()
	}
	if q.To != 0 && q.To <= q.From {
		return invalid()
	}
	for key, target := range map[string]*int{"page": &q.Page, "size": &q.Size, "page_size": &q.Size, "http_code": &q.HTTPCode} {
		if raw := values.Get(key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return invalid()
			}
			*target = n
		}
	}
	if q.Page > 100 || q.Size > 500 || q.HTTPCode != 0 && (q.HTTPCode < 100 || q.HTTPCode > 599) {
		return invalid()
	}
	if raw := values.Get("stream"); raw != "" {
		if raw != "true" && raw != "false" {
			return invalid()
		}
		v := raw == "true"
		q.Stream = &v
	}
	for key, target := range map[string]**bool{"model_mismatch": &q.ModelMismatch, "native_compaction": &q.NativeCompaction, "websocket_warmup": &q.WebSocketWarmup, "sent_model_mismatch": &q.SentModelMismatch, "returned_model_mismatch": &q.ReturnedModelMismatch, "error_truncated": &q.ErrorTruncated} {
		if raw := values.Get(key); raw != "" {
			if raw != "true" && raw != "false" {
				return invalid()
			}
			v := raw == "true"
			*target = &v
		}
	}
	if raw := values.Get("exact_total"); raw != "" {
		if raw != "true" && raw != "false" {
			return invalid()
		}
		q.ExactTotal = raw == "true"
	}
	if raw := values.Get("sort_by"); raw != "" {
		q.SortBy = raw
	}
	if raw := values.Get("sort_order"); raw != "" {
		q.SortOrder = raw
	}
	if !stringIn(q.SortBy, "time", "model", "tokens", "input_tokens", "output_tokens", "cached_tokens", "latency") || !stringIn(q.SortOrder, "asc", "desc") {
		return invalid()
	}
	if !stringIn(q.Status, "", "ok", "error", "aborted", "failed") {
		return invalid()
	}
	if q.ErrorCategory != "" && !stringIn(q.ErrorCategory, managedFailureCategories...) {
		return invalid()
	}
	q.ErrorSeverity = values.Get("error_severity")
	if !stringIn(q.ErrorSeverity, "", "P1", "P2", "P3", "unknown") {
		return invalid()
	}
	q.Cursor = values.Get("cursor")
	if q.Cursor != "" && q.Page > 1 {
		return invalid()
	}
	if q.Cursor != "" {
		if _, err := q.decodeCursor(); err != nil {
			return q, err
		}
	}
	return q, nil
}

func stringIn(value string, choices ...string) bool {
	for _, s := range choices {
		if s == value {
			return true
		}
	}
	return false
}

var managedFailureCategories = []string{"unsupported_parameter", "system_role", "no_enabled_account", "invalid_oauth", "unsupported_model", "encrypted_history", "request_too_large", "upstream_refusal", "rate_limit", "overloaded", "upstream_error", "aborted", "timeout", "authentication", "server_error", "unknown"}

func managedFailureCategory(r RequestRecord) string {
	if r.Status == "ok" {
		return ""
	}
	// The selected generation's typed outcome takes precedence over diagnostic
	// prose and later OAuth attempts. Legacy records keep their original rules.
	if r.Status == "error" && (r.ErrorSource == "" || r.ErrorSource == "upstream") && r.FinalUpstreamErrorSource == "upstream" {
		switch r.FinalUpstreamErrorCode {
		case "rateLimitExceeded", "usageLimitExceeded", "rate_limit_exceeded", "rate_limit_error", "insufficient_quota", "billing_hard_limit_reached":
			return "rate_limit"
		case "unauthorized", "authentication_error", "invalid_api_key", "authentication_unavailable", "upstream_account_rejected":
			return "authentication"
		case "serverOverloaded", "server_overloaded", "model_overloaded", "overloaded_error":
			return "overloaded"
		}
	}
	m := strings.ToLower(r.Error)
	switch {
	case strings.Contains(m, "unsupported parameter"):
		return "unsupported_parameter"
	case strings.Contains(m, "system messages are not allowed"):
		return "system_role"
	case strings.Contains(m, "no enabled account"):
		return "no_enabled_account"
	case strings.Contains(m, "invalidated oauth") || strings.Contains(m, "refresh_token_reused"):
		return "invalid_oauth"
	case strings.Contains(m, "model") && strings.Contains(m, "not supported") || strings.Contains(m, "model_not_found"):
		return "unsupported_model"
	case strings.Contains(m, "encrypted content") || strings.Contains(m, "invalid_encrypted_content"):
		return "encrypted_history"
	case r.HTTPCode == 413 || strings.Contains(m, "exceeds 32 mib"):
		return "request_too_large"
	case strings.Contains(m, "flagged for possible") || strings.Contains(m, "content_filter") || strings.Contains(m, "policy violation"):
		return "upstream_refusal"
	case r.HTTPCode == 429 || strings.Contains(m, "rate limit") || strings.Contains(m, "rate_limit"):
		return "rate_limit"
	case strings.Contains(m, "overloaded"):
		return "overloaded"
	case strings.Contains(m, "an error occurred while processing"):
		return "upstream_error"
	case r.Status == "aborted" || r.HTTPCode == 499 || strings.Contains(m, "context canceled"):
		return "aborted"
	case r.HTTPCode == 504 || strings.Contains(m, "timeout") || strings.Contains(m, "timed out") || strings.Contains(m, "deadline exceeded"):
		return "timeout"
	case r.HTTPCode == 401 || r.HTTPCode == 403:
		return "authentication"
	case r.HTTPCode >= 500:
		return "server_error"
	default:
		return "unknown"
	}
}

func (q managedLogQuery) matches(r RequestRecord) bool {
	if q.ClientRequestID != "" && q.ClientRequestID != r.ClientRequestID {
		return false
	}
	if q.ErrorSeverity != "" && managedErrorSeverity(r) != q.ErrorSeverity {
		return false
	}
	if q.WebSocketWarmup != nil && (r.WebSocketWarmup == nil || *q.WebSocketWarmup != *r.WebSocketWarmup) {
		return false
	}
	if q.SentModel != "" && q.SentModel != r.SentModel || q.ReturnedModel != "" && q.ReturnedModel != r.ReturnedModel || q.InboundPath != "" && q.InboundPath != r.InboundPath || q.UpstreamPath != "" && q.UpstreamPath != r.UpstreamPath {
		return false
	}
	if q.SentModelMismatch != nil && (r.Model == "" || r.SentModel == "" || *q.SentModelMismatch != (r.Model != r.SentModel)) {
		return false
	}
	if q.ReturnedModelMismatch != nil && (r.SentModel == "" || r.ReturnedModel == "" || *q.ReturnedModelMismatch != (r.SentModel != r.ReturnedModel)) {
		return false
	}
	if q.ErrorTruncated != nil && (r.ErrorTruncated == nil || *q.ErrorTruncated != *r.ErrorTruncated) {
		return false
	}
	if q.ReviewOnly != nil {
		key := managedUsageReviewKey(r)
		index := sort.SearchStrings(q.ReviewHashes, key)
		resolved := index < len(q.ReviewHashes) && q.ReviewHashes[index] == key
		if *q.ReviewOnly != resolved {
			return false
		}
	}
	if len(q.AccountIDs) > 0 && !stringIn(r.AccountID, q.AccountIDs...) {
		return false
	}
	if q.From != 0 && r.Time < q.From || q.To != 0 && r.Time >= q.To {
		return false
	}
	if q.AccountID != "" {
		if r.AccountID != q.AccountID {
			return false
		}
	} else if q.Account != "" && r.Account != q.Account && r.AccountID != q.Account {
		return false
	}
	if q.KeyName != "" && q.KeyName != r.KeyName || q.KeyID != "" && q.KeyID != r.KeyID || q.Model != "" && q.Model != r.Model || q.Endpoint != "" && q.Endpoint != r.Endpoint || q.IP != "" && q.IP != r.IP {
		return false
	}
	if q.UpstreamModel != "" && q.UpstreamModel != r.UpstreamModel || q.RequestType != "" && q.RequestType != r.RequestType || q.ServiceTier != "" && q.ServiceTier != r.ServiceTier || q.ReasoningEffort != "" && q.ReasoningEffort != r.ReasoningEffort {
		return false
	}
	if q.ModelMismatch != nil && (r.UpstreamModel == "" || *q.ModelMismatch != (r.UpstreamModel != r.Model)) {
		return false
	}
	if q.NativeCompaction != nil && (r.NativeCompaction == nil || *q.NativeCompaction != *r.NativeCompaction) {
		return false
	}
	if q.ErrorPhase != "" && r.ErrorPhase != q.ErrorPhase {
		return false
	}
	if q.GroupID != "" && !stringIn(q.GroupID, r.Groups...) {
		return false
	}
	if q.Status == "failed" {
		if r.Status == "ok" {
			return false
		}
	} else if q.Status != "" && q.Status != r.Status {
		return false
	}
	if q.HTTPCode != 0 && q.HTTPCode != r.HTTPCode || q.Stream != nil && *q.Stream != r.Stream {
		return false
	}
	if q.RequestID != "" && q.RequestID != r.EventID && q.RequestID != r.RequestID && q.RequestID != r.ResponseID && q.RequestID != strconv.FormatInt(r.ID, 10) {
		return false
	}
	return q.ErrorCategory == "" || q.ErrorCategory == managedFailureCategory(r)
}

func (q managedLogQuery) filterHash() string {
	q.SnapshotID = ""
	q.FileCeiling = 0
	q.Cursor = ""
	q.Page = 0
	q.Size = 0
	q.ExactTotal = false
	raw, _ := json.Marshal(q)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type managedLogCursor struct {
	Version   int        `json:"v"`
	Filter    string     `json:"f"`
	Value     int64      `json:"x"`
	Time      int64      `json:"t"`
	ID        int64      `json:"i"`
	Ceiling   int64      `json:"c"`
	Storage   string     `json:"s"`
	Text      string     `json:"text,omitempty"`
	ModelHash string     `json:"model_hash,omitempty"`
	ExactTime *time.Time `json:"-"`
}

func (q managedLogQuery) decodeCursor() (managedLogCursor, error) {
	var c managedLogCursor
	bad := fmt.Errorf("%w: invalid cursor or changed filters", errInvalid)
	limit := 1024
	if q.SortBy == "model" {
		limit = 64 << 10
	}
	if len(q.Cursor) > limit {
		return c, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
	if err != nil {
		return c, bad
	}
	if err = json.Unmarshal(raw, &c); err != nil || c.Time <= 0 || c.ID <= 0 {
		return c, bad
	}
	if c.Version == 0 && q.SortBy == "time" && q.SortOrder == "desc" {
		return c, nil
	}
	if c.Version != 1 && c.Version != 2 || c.Filter != q.filterHash() || c.Ceiling < 0 || !stringIn(c.Storage, "postgres", "file") {
		return c, bad
	}
	if c.Version == 2 {
		digest, err := hex.DecodeString(c.ModelHash)
		if q.SortBy != "model" || c.Text != "" || err != nil || len(digest) != sha256.Size {
			return c, bad
		}
	}
	return c, nil
}
func (q managedLogQuery) value(r RequestRecord) int64 {
	switch q.SortBy {
	case "model":
		return 0
	case "tokens":
		return r.InputTokens + r.OutputTokens
	case "input_tokens":
		return r.InputTokens
	case "output_tokens":
		return r.OutputTokens
	case "cached_tokens":
		return r.CachedTokens
	case "latency":
		return r.LatencyMs
	default:
		return r.Time
	}
}
func (q managedLogQuery) before(a, b RequestRecord) bool {
	if q.SortBy == "model" && a.Model != b.Model {
		if q.SortOrder == "asc" {
			return a.Model < b.Model
		}
		return a.Model > b.Model
	}
	ax, bx := q.value(a), q.value(b)
	if ax == bx {
		ax, bx = a.Time, b.Time
	}
	if ax == bx {
		ax, bx = a.ID, b.ID
	}
	if q.SortOrder == "asc" {
		return ax < bx
	}
	return ax > bx
}
func (q managedLogQuery) afterCursor(r RequestRecord, c managedLogCursor) bool {
	if q.SortBy == "model" && r.Model != c.Text {
		if q.SortOrder == "asc" {
			return r.Model > c.Text
		}
		return r.Model < c.Text
	}
	x := q.value(r)
	v := c.Value
	if c.Version == 0 {
		v = c.Time
	}
	if x == v {
		x, v = r.Time, c.Time
	}
	if x == v {
		x, v = r.ID, c.ID
	}
	if q.SortOrder == "asc" {
		return x > v
	}
	return x < v
}
func (q managedLogQuery) encodeCursor(r RequestRecord, ceiling int64, storage string) string {
	// Only lexical model sorting needs the model text for its seek boundary.
	// Unrelated cursors must not grow with a retained observation's model name.
	text := ""
	if q.SortBy == "model" {
		text = r.Model
	}
	c := managedLogCursor{Version: 1, Filter: q.filterHash(), Value: q.value(r), Time: r.Time, ID: r.ID, Ceiling: ceiling, Storage: storage, Text: text}
	raw, _ := json.Marshal(c)
	if base64.RawURLEncoding.EncodedLen(len(raw)) > 1024 && q.SortBy == "model" {
		// Oversized model observations cannot fit a practical URL cursor. Bind
		// a compact boundary reference to the retained record's exact text.
		digest := sha256.Sum256([]byte(text))
		c.Version, c.Text, c.ModelHash = 2, "", hex.EncodeToString(digest[:])
		raw, _ = json.Marshal(c)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (q managedLogQuery) sortSQL() string {
	switch q.SortBy {
	case "model":
		return `model COLLATE "C"`
	case "tokens":
		return "(COALESCE((record->>'input_tokens')::bigint,0)+COALESCE((record->>'output_tokens')::bigint,0))"
	case "input_tokens", "output_tokens", "cached_tokens":
		return "COALESCE((record->>'" + q.SortBy + "')::bigint,0)"
	case "latency":
		return "COALESCE((record->>'latency_ms')::bigint,0)"
	default:
		return "created_at"
	}
}

func (q managedLogQuery) orderSQL() string {
	if q.SortBy == "time" {
		// Sorting a timestamp and its monotone epoch projection is redundant.
		// Keep the native column so the existing (created_at,id) index can seek.
		return "created_at " + q.SortOrder + ",id " + q.SortOrder
	}
	return q.sortSQL() + " " + q.SortOrder + ",created_at " + q.SortOrder + ",id " + q.SortOrder
}

// The CASE order matches managedFailureCategory. Fixed SQL expressions only;
// every caller-controlled filter value is a positional parameter.
const managedFailureSQL = `CASE WHEN status='ok' THEN ''
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('rateLimitExceeded','usageLimitExceeded','rate_limit_exceeded','rate_limit_error','insufficient_quota','billing_hard_limit_reached') THEN 'rate_limit'
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('unauthorized','authentication_error','invalid_api_key','authentication_unavailable','upstream_account_rejected') THEN 'authentication'
 WHEN status='error' AND COALESCE(record->>'error_source','') IN ('','upstream') AND record->>'final_upstream_error_source'='upstream' AND record->>'final_upstream_error_code' IN ('serverOverloaded','server_overloaded','model_overloaded','overloaded_error') THEN 'overloaded'
 WHEN position('unsupported parameter' in lower(COALESCE(record->>'error','')))>0 THEN 'unsupported_parameter'
 WHEN position('system messages are not allowed' in lower(COALESCE(record->>'error','')))>0 THEN 'system_role'
 WHEN position('no enabled account' in lower(COALESCE(record->>'error','')))>0 THEN 'no_enabled_account'
 WHEN position('invalidated oauth' in lower(COALESCE(record->>'error','')))>0 OR position('refresh_token_reused' in lower(COALESCE(record->>'error','')))>0 THEN 'invalid_oauth'
 WHEN (position('model' in lower(COALESCE(record->>'error','')))>0 AND position('not supported' in lower(COALESCE(record->>'error','')))>0) OR position('model_not_found' in lower(COALESCE(record->>'error','')))>0 THEN 'unsupported_model'
 WHEN position('encrypted content' in lower(COALESCE(record->>'error','')))>0 OR position('invalid_encrypted_content' in lower(COALESCE(record->>'error','')))>0 THEN 'encrypted_history'
 WHEN record->>'http_code'='413' OR position('exceeds 32 mib' in lower(COALESCE(record->>'error','')))>0 THEN 'request_too_large'
 WHEN position('flagged for possible' in lower(COALESCE(record->>'error','')))>0 OR position('content_filter' in lower(COALESCE(record->>'error','')))>0 OR position('policy violation' in lower(COALESCE(record->>'error','')))>0 THEN 'upstream_refusal'
 WHEN record->>'http_code'='429' OR position('rate limit' in lower(COALESCE(record->>'error','')))>0 OR position('rate_limit' in lower(COALESCE(record->>'error','')))>0 THEN 'rate_limit'
 WHEN position('overloaded' in lower(COALESCE(record->>'error','')))>0 THEN 'overloaded'
 WHEN position('an error occurred while processing' in lower(COALESCE(record->>'error','')))>0 THEN 'upstream_error'
 WHEN status='aborted' OR record->>'http_code'='499' OR position('context canceled' in lower(COALESCE(record->>'error','')))>0 THEN 'aborted'
 WHEN record->>'http_code'='504' OR position('timeout' in lower(COALESCE(record->>'error','')))>0 OR position('timed out' in lower(COALESCE(record->>'error','')))>0 OR position('deadline exceeded' in lower(COALESCE(record->>'error','')))>0 THEN 'timeout'
 WHEN record->>'http_code' IN ('401','403') THEN 'authentication'
 WHEN COALESCE((record->>'http_code')::integer,0)>=500 THEN 'server_error' ELSE 'unknown' END`

func (q managedLogQuery) whereSQL() (string, []any) {
	return q.whereSQLAfter(nil)
}
func (q managedLogQuery) whereSQLAfter(initial []any) (string, []any) {
	parts := []string{"TRUE"}
	args := append([]any{}, initial...)
	add := func(expr string, value any) {
		args = append(args, value)
		parts = append(parts, strings.ReplaceAll(expr, "$%d", "$"+strconv.Itoa(len(args))))
	}
	if filter := q.indexedFilterTags(); filter != nil {
		add(managedLogFilterProjection+" @> $%d::jsonb", filter)
	}
	if q.ClientRequestID != "" {
		add("record->>'client_request_id'=$%d", q.ClientRequestID)
	}
	for _, field := range []struct{ name, value string }{{"sent_model", q.SentModel}, {"returned_model", q.ReturnedModel}, {"inbound_path", q.InboundPath}, {"upstream_path", q.UpstreamPath}} {
		if field.value != "" {
			add("record->>'"+field.name+"'=$%d", field.value)
		}
	}
	if q.SentModelMismatch != nil {
		add("(COALESCE(model,'')<>'' AND COALESCE(record->>'sent_model','')<>'' AND (record->>'sent_model'<>model)=$%d)", *q.SentModelMismatch)
	}
	if q.ReturnedModelMismatch != nil {
		add("(COALESCE(record->>'sent_model','')<>'' AND COALESCE(record->>'returned_model','')<>'' AND (record->>'returned_model'<>record->>'sent_model')=$%d)", *q.ReturnedModelMismatch)
	}
	if q.ErrorTruncated != nil {
		add("(record->>'error_truncated')::boolean=$%d", *q.ErrorTruncated)
	}
	if q.ReviewOnly != nil {
		clauses := []string{}
		if len(q.ReviewEventIDs) > 0 {
			args = append(args, q.ReviewEventIDs)
			clauses = append(clauses, fmt.Sprintf("event_id=ANY($%d::text[])", len(args)))
		}
		if len(q.ReviewRecordIDs) > 0 {
			args = append(args, q.ReviewRecordIDs)
			clauses = append(clauses, fmt.Sprintf("(COALESCE(event_id,'')='' AND id=ANY($%d::bigint[]))", len(args)))
		}
		match := "FALSE"
		if len(clauses) > 0 {
			match = "(" + strings.Join(clauses, " OR ") + ")"
		}
		if !*q.ReviewOnly {
			match = "NOT (" + match + ")"
		}
		parts = append(parts, match)
	}
	if len(q.AccountIDs) > 0 {
		add("account_ref=ANY($%d::text[])", q.AccountIDs)
	}
	if q.SnapshotID != "" {
		add("id IN (SELECT log_id FROM management_cleanup_targets WHERE preview_id=$%d)", q.SnapshotID)
	}
	if q.From != 0 {
		add("created_at >= $%d", time.Unix(q.From, 0).UTC())
	}
	if q.To != 0 {
		add("created_at < $%d", time.Unix(q.To, 0).UTC())
	}
	if q.AccountID != "" {
		add("account_ref = $%d", q.AccountID)
	} else if q.Account != "" {
		args = append(args, q.Account)
		parts = append(parts, fmt.Sprintf("(account_ref=$%d OR account_name=$%d)", len(args), len(args)))
	}
	for _, field := range []struct{ expr, value string }{{"key_name", q.KeyName}, {"model", q.Model}, {"record->>'key_id'", q.KeyID}, {"record->>'endpoint'", q.Endpoint}, {"record->>'ip'", q.IP}, {"record->>'upstream_model'", q.UpstreamModel}, {"record->>'request_type'", q.RequestType}, {"record->>'service_tier'", q.ServiceTier}, {"record->>'reasoning_effort'", q.ReasoningEffort}, {"record->>'error_phase'", q.ErrorPhase}} {
		if field.value != "" {
			add(field.expr+" = $%d", field.value)
		}
	}
	if q.GroupID != "" {
		add("COALESCE(record->'groups','[]'::jsonb) ? $%d", q.GroupID)
	}
	if q.Status == "failed" {
		parts = append(parts, "status<>'ok'")
	} else if q.Status != "" {
		add("status=$%d", q.Status)
	}
	if q.HTTPCode != 0 {
		add("COALESCE((record->>'http_code')::integer,0)=$%d", q.HTTPCode)
	}
	if q.Stream != nil {
		add("COALESCE((record->>'stream')::boolean,false)=$%d", *q.Stream)
	}
	if q.RequestID != "" {
		var match string
		match, args = q.requestIdentitySQL(args)
		parts = append(parts, "id IN ("+match+")")
	}
	if q.ModelMismatch != nil {
		add("(COALESCE(record->>'upstream_model','')<>'' AND (record->>'upstream_model'<>model)=$%d)", *q.ModelMismatch)
	}
	if q.NativeCompaction != nil {
		add("(record->>'native_compaction')::boolean=$%d", *q.NativeCompaction)
	}
	if q.WebSocketWarmup != nil {
		add("(record->>'websocket_warmup')::boolean=$%d", *q.WebSocketWarmup)
	}
	if q.ErrorCategory != "" {
		add(managedErrorCategoryProjection+"=$%d", q.ErrorCategory)
	}
	if q.ErrorSeverity != "" {
		add(managedErrorSeverityProjection+"=$%d", q.ErrorSeverity)
	}
	return strings.Join(parts, " AND "), args
}

func (q managedLogQuery) requestIdentitySQL(initial []any) (string, []any) {
	args := append(append([]any{}, initial...), q.RequestID)
	parameter := len(args)
	match := fmt.Sprintf("SELECT id FROM request_logs WHERE event_id=$%d UNION SELECT id FROM request_logs WHERE record->>'request_id'=$%d UNION SELECT id FROM request_logs WHERE record->>'response_id'=$%d", parameter, parameter, parameter)
	// Noncanonical numbers only match textual identities, preserving the old
	// id::text contract without casting the indexed primary key.
	if id, err := strconv.ParseInt(q.RequestID, 10, 64); err == nil && strconv.FormatInt(id, 10) == q.RequestID {
		args = append(args, id)
		match += fmt.Sprintf(" UNION SELECT $%d::bigint", len(args))
	}
	return match, args
}

// A plain IN/UNION predicate can become a generic hash join scanning millions
// of unrelated logs when sparse JSON identities have default row estimates.
// OFFSET 0 keeps each candidate lookup a parameterized primary-key probe,
// without global planner settings or unbounded application-side ID arrays.
func (q managedLogQuery) readSourceSQL() (source, where string, args []any) {
	if q.RequestID == "" && q.ClientRequestID == "" {
		where, args = q.whereSQL()
		return "request_logs", where, args
	}
	var candidates string
	if q.RequestID != "" {
		candidates, args = q.requestIdentitySQL(nil)
		q.RequestID = ""
	} else {
		candidates = "SELECT id FROM request_logs WHERE record->>'client_request_id'=$1"
		args = []any{q.ClientRequestID}
		q.ClientRequestID = ""
	}
	where, args = q.whereSQLAfter(args)
	source = "(SELECT matched.* FROM (" + candidates + ") candidates CROSS JOIN LATERAL (SELECT * FROM request_logs WHERE id=candidates.id OFFSET 0) matched) request_logs"
	return source, where, args
}

func queryCanceled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

var errManagedLogNotFound = errors.New("usage record not found")
