package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Separate maintenance from measured HTTP so large index construction never
// masquerades as a slow endpoint and can share a host with fixture generation.
func TestPerformanceMaintenanceMigrations(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("explicit synthetic maintenance only")
	}
	schema := os.Getenv("PERFORMANCE_SLO_SCHEMA")
	if !regexp.MustCompile(`^perf_(slo|uniform)_[a-z0-9_]{8,60}$`).MatchString(schema) {
		t.Fatal("synthetic schema required")
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil || u.User == nil || u.User.Username() != "codex_agent_test" || u.Path != "/codex_agent_integration" {
		t.Fatal("isolated test database required")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	key, id := byte(17), "slo-synthetic"
	if strings.HasPrefix(schema, "perf_uniform_") {
		key, id = 19, "uniform-synthetic"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	db, err := OpenDatabase(ctx, u.String(), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{key}, 32)), id)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var exists bool
	if err := db.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&exists); err != nil || !exists {
		t.Fatal("existing owned fixture required", err)
	}
	started := time.Now()
	performanceSLOEmit(t, "MAINTENANCE_START", map[string]any{"schema": schema, "started_at": started.UTC().Format(time.RFC3339), "scope": "migration only; no HTTP measurement"})
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	performanceSLOEmit(t, "MAINTENANCE_COMPLETE", map[string]any{"schema": schema, "elapsed_s": time.Since(started).Seconds(), "scope": "migration only; do not treat fixture/index construction as request latency"})
}
