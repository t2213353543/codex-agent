package server

// This maintenance command fills only missing usage proven by an original
// official rollout. It does not infer tokens, reprice existing observations or
// rebuild aggregates; operators run the existing rebuild command afterwards.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const officialUsageRepairRecordLimit = 32 << 20

const officialUsageRepairRoot = "/var/lib/codex-agent/official-runtime"

type officialRepairUsage struct {
	Input     *int64 `json:"input_tokens"`
	Output    *int64 `json:"output_tokens"`
	Cached    *int64 `json:"cached_input_tokens"`
	Write     *int64 `json:"cache_write_input_tokens"`
	Reasoning *int64 `json:"reasoning_output_tokens"`
	Total     *int64 `json:"total_tokens"`
}

type officialUsageRepairEntry struct {
	RecordID         int64               `json:"record_id"`
	EventID          string              `json:"event_id"`
	ResponseID       string              `json:"gateway_response_id"`
	KnownBefore      *bool               `json:"known_before"`
	RequestTime      time.Time           `json:"request_time"`
	LatencyMs        int64               `json:"latency_ms"`
	UsageTime        time.Time           `json:"usage_time"`
	Usage            officialRepairUsage `json:"usage"`
	Model            string              `json:"model"`
	SourceLineSHA256 string              `json:"source_line_sha256"`
	MatchedItemIDs   []string            `json:"matched_item_ids"`
	Rollout          string              `json:"rollout"`
	Line             int                 `json:"line"`
	NativeResponseID string              `json:"native_response_id"`
}

type officialUsageRepairPlan struct {
	Schema  int                        `json:"schema"`
	Mode    string                     `json:"mode"`
	Matches []officialUsageRepairEntry `json:"matches"`
}

type officialUsageRepairSummary struct {
	DryRun          bool   `json:"dry_run"`
	PlanSHA256      string `json:"plan_sha256"`
	Verified        int    `json:"verified"`
	Repairable      int    `json:"repairable"`
	Updated         int    `json:"updated"`
	AlreadyRepaired int    `json:"already_repaired"`
	CostKnown       int    `json:"cost_known"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	RebuildRequired bool   `json:"rebuild_required"`
}

func repairOfficialUsage(ctx context.Context, db *Database, source string, dryRun bool) error {
	summary, err := repairOfficialUsageWithRoot(ctx, db, source, officialUsageRepairRoot, dryRun)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(summary)
}

func validateOfficialRepairUsage(usage officialRepairUsage) error {
	for _, value := range []*int64{usage.Input, usage.Output, usage.Cached, usage.Write, usage.Reasoning, usage.Total} {
		if value == nil || *value < 0 {
			return errors.New("official repair requires all six nonnegative usage counters")
		}
	}
	if *usage.Input > math.MaxInt64-*usage.Output || *usage.Total != *usage.Input+*usage.Output ||
		*usage.Cached > *usage.Input || *usage.Write > *usage.Input-*usage.Cached || *usage.Reasoning > *usage.Output {
		return errors.New("official repair usage counters are inconsistent")
	}
	return nil
}

func readOfficialUsageRepairPlan(source string) (officialUsageRepairPlan, string, error) {
	var plan officialUsageRepairPlan
	file, err := os.Open(source)
	if err != nil {
		return plan, "", errors.New("official repair plan unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > officialUsageRepairRecordLimit {
		return plan, "", errors.New("official repair plan is not a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, officialUsageRepairRecordLimit+1))
	if err != nil || len(raw) > officialUsageRepairRecordLimit || json.Unmarshal(raw, &plan) != nil || plan.Schema != 1 || plan.Mode != "read_only_recovery_evidence" || len(plan.Matches) == 0 || len(plan.Matches) > 1000 {
		return plan, "", errors.New("official repair plan is invalid or exceeds limits")
	}
	ids, events, responses, sources, nativeIDs := map[int64]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, entry := range plan.Matches {
		sourceKey := entry.Rollout + "\x00" + fmt.Sprint(entry.Line)
		if entry.RecordID <= 0 || entry.EventID == "" || entry.ResponseID == "" || entry.NativeResponseID == "" || entry.Model == "" ||
			entry.KnownBefore != nil && *entry.KnownBefore || ids[entry.RecordID] || events[entry.EventID] || responses[entry.ResponseID] || sources[sourceKey] || nativeIDs[entry.NativeResponseID] ||
			entry.Line <= 0 || entry.Line > 100000 || len(entry.SourceLineSHA256) != 64 || len(entry.MatchedItemIDs) == 0 || len(entry.MatchedItemIDs) > 1024 ||
			entry.RequestTime.IsZero() || entry.UsageTime.IsZero() || entry.LatencyMs < 0 || entry.LatencyMs > int64((24*time.Hour)/time.Millisecond) {
			return plan, "", errors.New("official repair plan identity, time or cardinality is invalid")
		}
		if _, err := hex.DecodeString(entry.SourceLineSHA256); err != nil {
			return plan, "", errors.New("official repair line checksum is invalid")
		}
		// Request rows retain whole Unix seconds. One extra second covers only
		// that lost fractional start time, not arbitrary neighboring requests.
		if entry.UsageTime.Before(entry.RequestTime) || entry.UsageTime.After(entry.RequestTime.Add(time.Duration(entry.LatencyMs)*time.Millisecond+time.Second)) {
			return plan, "", errors.New("official repair usage time falls outside the request")
		}
		if err := validateOfficialRepairUsage(entry.Usage); err != nil {
			return plan, "", err
		}
		ids[entry.RecordID], events[entry.EventID], responses[entry.ResponseID], sources[sourceKey] = true, true, true, true
		nativeIDs[entry.NativeResponseID] = true
	}
	sort.Slice(plan.Matches, func(i, j int) bool { return plan.Matches[i].RecordID < plan.Matches[j].RecordID })
	sum := sha256.Sum256(raw)
	return plan, hex.EncodeToString(sum[:]), nil
}

func openOfficialRepairRollout(root, name string) (*os.File, error) {
	root, err := filepath.Abs(root)
	if err != nil || !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("official repair rollout path is invalid")
	}
	relative, err := filepath.Rel(root, name)
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if err != nil || len(parts) < 5 || !strings.HasPrefix(parts[0], "session-") || parts[0] == "session-" || parts[1] != "home" || parts[2] != "sessions" || !strings.HasSuffix(parts[len(parts)-1], ".jsonl") {
		return nil, errors.New("official repair rollout escaped the official runtime sessions")
	}
	// DynamicUser's managed parent may itself be a symlink. Below the trusted
	// runtime root every component must be a real directory/file, never a link.
	current := root
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("official repair rollout path is invalid")
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("official repair rollout is missing or contains a symlink")
		}
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("official repair rollout unavailable")
	}
	opened, err := file.Stat()
	named, namedErr := os.Lstat(name)
	if err != nil || namedErr != nil || !opened.Mode().IsRegular() || opened.Size() > 128<<20 || !os.SameFile(opened, named) {
		file.Close()
		return nil, errors.New("official repair rollout changed or exceeds the size limit")
	}
	return file, nil
}

func verifyOfficialRepairRollout(root string, entry officialUsageRepairEntry) error {
	file, err := openOfficialRepairRollout(root, entry.Rollout)
	if err != nil {
		return err
	}
	defer file.Close()
	scan := bufio.NewScanner(io.LimitReader(file, 128<<20+1))
	scan.Buffer(make([]byte, 64<<10), officialUsageRepairRecordLimit)
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if n := bytes.IndexByte(data, '\n'); n >= 0 {
			return n + 1, data[:n+1], nil
		}
		if atEOF && len(data) != 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	model := ""
	items := map[string]bool{}
	line := 0
	for scan.Scan() {
		line++
		var event struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type, Role, ID, Model string
				CallID                string              `json:"call_id"`
				ResponseID            string              `json:"response_id"`
				Usage                 officialRepairUsage `json:"usage"`
			} `json:"payload"`
		}
		if json.Unmarshal(scan.Bytes(), &event) != nil {
			return errors.New("official repair rollout contains an invalid event")
		}
		switch event.Type {
		case "turn_context":
			model = event.Payload.Model
		case "response_item":
			if event.Payload.Type != "message" || event.Payload.Role == "assistant" {
				identity := event.Payload.ID
				if identity == "" {
					identity = event.Payload.CallID
				}
				if identity != "" {
					items[identity] = true
				}
			}
		}
		if line == entry.Line {
			digest := sha256.Sum256(scan.Bytes())
			if event.Type != "token_usage_record" || hex.EncodeToString(digest[:]) != entry.SourceLineSHA256 || event.Payload.ResponseID != entry.NativeResponseID || !event.Timestamp.Equal(entry.UsageTime) || model != entry.Model {
				return errors.New("official repair rollout provenance differs from plan")
			}
			actual, _ := json.Marshal(event.Payload.Usage)
			expected, _ := json.Marshal(entry.Usage)
			if !bytes.Equal(actual, expected) {
				return errors.New("official repair rollout usage differs from plan")
			}
			seen := map[string]bool{}
			for _, identity := range entry.MatchedItemIDs {
				if identity == "" || !items[identity] || seen[identity] {
					return errors.New("official repair response item identity differs from rollout")
				}
				seen[identity] = true
			}
			return nil
		}
		if event.Type == "token_usage_record" {
			items = map[string]bool{}
		}
	}
	return errors.New("official repair rollout line is unavailable or exceeds limits")
}

func officialRepairRecordPatch(raw []byte, entry officialUsageRepairEntry) ([]byte, bool, bool, error) {
	var record RequestRecord
	if json.Unmarshal(raw, &record) != nil || record.EventID != entry.EventID || record.ResponseID != entry.ResponseID || record.RequestType != "official_app_server" || record.Status != "ok" || record.HTTPCode != 200 || record.Time != entry.RequestTime.Unix() || record.LatencyMs != entry.LatencyMs {
		return nil, false, false, errors.New("official repair request identity or timing differs from plan")
	}
	u := entry.Usage
	if record.UsageKnown != nil && *record.UsageKnown {
		if record.InputTokens != *u.Input || record.OutputTokens != *u.Output || record.CachedTokens != *u.Cached || record.UsageTotalTokens == nil || *record.UsageTotalTokens != *u.Total || record.ReasoningTokens == nil || *record.ReasoningTokens != *u.Reasoning || record.CacheWriteTokens == nil || *record.CacheWriteTokens != *u.Write || record.UsageSchemaSource != "official_rollout" {
			return nil, false, false, errors.New("official repair already-known usage conflicts with evidence")
		}
		// Older repairs omitted cache knowledge. Exact rollout evidence can
		// fill that flag without rewriting frozen usage, timing or cost.
		if record.CachedTokensKnown == nil || !*record.CachedTokensKnown {
			return []byte(`{"cached_tokens_known":true}`), true, record.WindowCostUSD != nil, nil
		}
		return nil, true, record.WindowCostUSD != nil, nil
	}
	if record.InputTokens != 0 || record.OutputTokens != 0 || record.CachedTokens != 0 || record.WindowCostUSD != nil || record.WindowCostPriceVersion != "" {
		return nil, false, false, errors.New("official repair would overwrite an existing token or cost observation")
	}
	for _, pair := range [][2]*int64{{record.UsageTotalTokens, u.Total}, {record.ReasoningTokens, u.Reasoning}, {record.CacheWriteTokens, u.Write}} {
		if pair[0] != nil && *pair[0] != *pair[1] {
			return nil, false, false, errors.New("official repair would overwrite a conflicting partial usage observation")
		}
	}
	record.InputTokens, record.OutputTokens, record.CachedTokens = *u.Input, *u.Output, *u.Cached
	known := true
	record.UsageKnown, record.CachedTokensKnown, record.UsageTotalTokens, record.ReasoningTokens, record.CacheWriteTokens = &known, &known, u.Total, u.Reasoning, u.Write
	patch := map[string]any{"usage_known": true, "input_tokens": *u.Input, "output_tokens": *u.Output, "cached_tokens": *u.Cached, "cached_tokens_known": true, "usage_total_tokens": *u.Total, "reasoning_tokens": *u.Reasoning, "cache_write_tokens": *u.Write, "usage_schema_source": "official_rollout"}
	if record.UpstreamModel == "" && record.Model == entry.Model {
		record.UpstreamModel = entry.Model
		patch["upstream_model"], patch["upstream_model_source"] = entry.Model, "appserver_thread"
	}
	freezeManagedAccountRequestUSD(&record)
	if record.WindowCostUSD != nil {
		patch["window_cost_usd"], patch["window_cost_price_version"] = record.WindowCostUSD, record.WindowCostPriceVersion
	}
	encoded, err := json.Marshal(patch)
	return encoded, false, record.WindowCostUSD != nil, err
}

func repairOfficialUsageWithRoot(ctx context.Context, db *Database, source, root string, dryRun bool) (officialUsageRepairSummary, error) {
	result := officialUsageRepairSummary{DryRun: dryRun}
	plan, digest, err := readOfficialUsageRepairPlan(source)
	if err != nil {
		return result, err
	}
	result.PlanSHA256 = digest
	for _, entry := range plan.Matches {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := verifyOfficialRepairRollout(root, entry); err != nil {
			return result, fmt.Errorf("official repair record %d evidence rejected: %w", entry.RecordID, err)
		}
	}
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	// Serialize against aggregate rebuild and ordinary metric ingestion. This
	// does not alter aggregates here; the explicit rebuild remains a separate step.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_schema(),$1))", metricsAggregateLock); err != nil {
		return result, err
	}
	usageUpdated := false
	for _, entry := range plan.Matches {
		var raw []byte
		var eventID, logStatus, eventStatus string
		var logTime, eventTime time.Time
		var input, output, cached, latency int64
		err = tx.QueryRow(ctx, `SELECT l.record,l.event_id,l.created_at,l.status,e.created_at,e.status,e.input_tokens,e.output_tokens,e.cached_tokens,e.latency_ms
 FROM request_logs l JOIN usage_events e ON e.event_id=l.event_id WHERE l.id=$1 FOR UPDATE OF l,e`, entry.RecordID).Scan(&raw, &eventID, &logTime, &logStatus, &eventTime, &eventStatus, &input, &output, &cached, &latency)
		if err != nil {
			return result, fmt.Errorf("official repair record %d lock failed", entry.RecordID)
		}
		if eventID != entry.EventID || !logTime.Equal(entry.RequestTime) || !eventTime.Equal(logTime) || logStatus != "ok" || eventStatus != "ok" || latency != entry.LatencyMs {
			return result, fmt.Errorf("official repair record %d durable identity differs", entry.RecordID)
		}
		patch, already, costKnown, err := officialRepairRecordPatch(raw, entry)
		if err != nil {
			return result, fmt.Errorf("official repair record %d refused: %w", entry.RecordID, err)
		}
		result.Verified++
		if already {
			if input != *entry.Usage.Input || output != *entry.Usage.Output || cached != *entry.Usage.Cached {
				return result, errors.New("official repair known log differs from durable usage")
			}
			if len(patch) == 0 {
				result.AlreadyRepaired++
				continue
			}
			result.Repairable++
			if !dryRun {
				command, err := tx.Exec(ctx, "UPDATE request_logs SET record=record||$2::jsonb WHERE id=$1", entry.RecordID, patch)
				if err != nil || command.RowsAffected() != 1 {
					return result, errors.New("official repair cache knowledge update failed")
				}
				result.Updated++
			}
			continue
		}
		if input != 0 || output != 0 || cached != 0 {
			return result, errors.New("official repair would overwrite durable usage")
		}
		result.Repairable++
		if result.InputTokens > math.MaxInt64-*entry.Usage.Input || result.OutputTokens > math.MaxInt64-*entry.Usage.Output || result.CachedTokens > math.MaxInt64-*entry.Usage.Cached {
			return result, errors.New("official repair summary counters exceed integer range")
		}
		result.InputTokens += *entry.Usage.Input
		result.OutputTokens += *entry.Usage.Output
		result.CachedTokens += *entry.Usage.Cached
		if costKnown {
			result.CostKnown++
		}
		if !dryRun {
			command, err := tx.Exec(ctx, "UPDATE request_logs SET record=record||$2::jsonb WHERE id=$1", entry.RecordID, patch)
			if err != nil || command.RowsAffected() != 1 {
				return result, errors.New("official repair request update failed")
			}
			command, err = tx.Exec(ctx, "UPDATE usage_events SET input_tokens=$2,output_tokens=$3,cached_tokens=$4 WHERE event_id=$1", entry.EventID, *entry.Usage.Input, *entry.Usage.Output, *entry.Usage.Cached)
			if err != nil || command.RowsAffected() != 1 {
				return result, errors.New("official repair durable usage update failed")
			}
			result.Updated++
			usageUpdated = true
		}
	}
	if !dryRun {
		if err = tx.Commit(ctx); err != nil {
			return result, err
		}
		result.RebuildRequired = usageUpdated
	}
	return result, nil
}
