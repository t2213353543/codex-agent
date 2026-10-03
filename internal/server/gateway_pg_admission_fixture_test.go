package server

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Optional for management-only PG tests. A DB-backed inference fixture must
// opt into the same shared-admission store as production, never local fallback.
// Metrics fixtures deliberately do not call this helper or require Redis.
func configureAdminPGAdmissionRedis(t *testing.T, db *Database) {
	t.Helper()
	address := os.Getenv("TEST_REDIS_URL")
	if address == "" || db.Redis != nil {
		return
	}
	target, err := url.Parse(address)
	if err != nil || target.Scheme != "redis" || target.Hostname() != "127.0.0.1" || target.Port() != "26379" || target.User == nil || target.User.Username() != "codex_agent_test" {
		t.Fatal("admin PostgreSQL fixture requires the isolated loopback Redis endpoint")
	}
	if err := db.ConfigureRedis(address); err != nil {
		t.Fatal("isolated admission Redis configuration failed")
	}
	prefix := "codex-agent:integration:pg-admin:" + newID("") + ":"
	db.Namespace = prefix
	client := db.Redis
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal("isolated admission Redis is unavailable")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		defer client.Close()
		// This random prefix was allocated above for this one PG fixture. Do
		// not inspect other test namespaces, and never FLUSH the shared server.
		var cursor uint64
		var keys []string
		for {
			found, next, err := client.Scan(ctx, cursor, prefix+"*", 256).Result()
			if err != nil {
				t.Error("isolated admission fixture key enumeration failed")
				return
			}
			for _, key := range found {
				if !strings.HasPrefix(key, prefix) {
					t.Error("refusing cleanup outside owned test namespace")
					return
				}
				keys = append(keys, key)
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if len(keys) > 0 {
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Error("isolated admission fixture cleanup failed")
			}
		}
	})
}
