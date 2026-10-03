package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

type localWriteNoUpstream struct{ calls atomic.Int64 }

func (d *localWriteNoUpstream) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls.Add(1)
	return nil, errors.New("performance fixture forbids upstream requests")
}

// Only this new test file implements the fixture. Public handlers run on the
// real admin router and loopback TCP. Schedules are disabled and no worker is
// started; the one cleanup worker step is explicit and separately timed.
func TestPerformanceManagementLocalWrites(t *testing.T) {
	if os.Getenv("PERFORMANCE_SLO") != "1" {
		t.Skip("isolated synthetic runner required")
	}
	p, handler := adminPGTestProxy(t)
	deny := new(localWriteNoUpstream)
	p.Client = &http.Client{Transport: deny}
	p.BaseURL = "http://127.0.0.1:1/forbidden-provider"
	account := managedTestAccount(t, p, "local-write-main", nil)
	groups := []Group{}
	for i := range 2 {
		g, err := p.Groups.Create(fmt.Sprintf("local-write-group-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, g)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "local-write-proxy", Type: "http", Host: "127.0.0.1", Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	adminKey := webTestKey
	samples := envInt("PERFORMANCE_SLO_SAMPLES", 5, 1, 20)
	enforce := os.Getenv("PERFORMANCE_LOCAL_WRITE_FUNCTIONAL") != "1"
	type sample struct {
		MS     float64 `json:"ms"`
		Status int     `json:"status"`
		Error  string  `json:"error,omitempty"`
	}
	measured := map[string][]sample{}
	routes := map[string]map[string]string{}
	request := func(name, method, path string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+adminKey)
		r.Header.Set("Content-Type", "application/json")
		start := time.Now()
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		value := map[string]any{}
		s := sample{MS: float64(time.Since(start).Microseconds()) / 1000, Status: response.StatusCode}
		if readErr != nil || response.StatusCode != 200 || json.Unmarshal(content, &value) != nil {
			s.Error = "http_or_json_failure"
		}
		measured[name] = append(measured[name], s)
		routes[name] = map[string]string{"method": method, "path": path}
		if s.Error != "" {
			t.Fatalf("%s: %d %s", name, s.Status, content)
		}
		return value
	}
	object := func(value map[string]any, key string) map[string]any {
		t.Helper()
		result, ok := value[key].(map[string]any)
		if !ok {
			t.Fatalf("missing object %s", key)
		}
		return result
	}
	current := func() Account {
		t.Helper()
		a, err := p.Store.GetMetadata(account.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	leaseVersion, policyVersion := int64(0), int64(0)
	for iteration := range samples {
		selected := request("account_selection", http.MethodPost, "/api/accounts/selection", map[string]any{"filters": map[string]any{"search": "local-write-main"}})
		if selected["total"] != float64(1) || len(selected["items"].([]any)) != 1 {
			t.Fatal("selection identity/count mismatch")
		}
		settings, err := p.Settings.Latest()
		if err != nil {
			t.Fatal(err)
		}
		prompt := fmt.Sprintf("synthetic operator prompt %d", iteration)
		saved := request("settings_save", http.MethodPost, "/api/settings", map[string]any{"settings_version": settings.Version, "builtin_prompt": prompt, "builtin_prompt_enabled": false})
		settingsNow, err := p.Settings.Latest()
		if err != nil || saved["ok"] != true || settingsNow.BuiltinPrompt != prompt || settingsNow.Version != settings.Version+1 {
			t.Fatal("settings write did not persist")
		}
		for _, bound := range []bool{true, false} {
			a := current()
			g, pids := []string{}, []string{}
			name := "account_unbind"
			if bound {
				g = []string{groups[1].ID, groups[0].ID}
				pids = []string{proxy.ID}
				name = "account_bind"
			}
			out := request(name, http.MethodPut, "/api/accounts/"+a.ID, map[string]any{"version": a.Version, "groups": g, "proxies": pids})
			after := current()
			sort.Strings(g)
			if out["ok"] != true || after.Version != a.Version+1 || !reflect.DeepEqual(after.Groups, g) || !reflect.DeepEqual(after.Proxies, pids) {
				t.Fatalf("binding change not durable: %+v", after)
			}
		}
		a := current()
		lease := request("account_lease_update", http.MethodPut, "/api/accounts/"+a.ID+"/lease", map[string]any{"version": leaseVersion, "account_version": a.Version, "expires_at": time.Now().Unix() + 3600, "auto_disable": false, "restore_on_extend": false})
		leaseVersion++
		if object(lease, "lease")["version"] != float64(leaseVersion) {
			t.Fatal("lease version mismatch")
		}
		policy := request("account_auto_reset_off", http.MethodPut, "/api/accounts/"+a.ID+"/auto-reset", map[string]any{"version": policyVersion, "account_version": a.Version, "enabled": false, "threshold_percent": 95, "interval_minutes": 5})
		policyVersion++
		if object(policy, "policy")["version"] != float64(policyVersion) || object(policy, "policy")["phase"] != "off" {
			t.Fatal("policy write failed")
		}
		planInput := managedScheduleInput{Kind: "models", Cron: "0 * * * *", Timezone: "UTC", Enabled: false, MaxResults: 100}
		plan := request("schedule_create", http.MethodPost, "/api/accounts/"+a.ID+"/schedules", planInput)
		id, ok := plan["id"].(string)
		if !ok || plan["version"] != float64(1) {
			t.Fatal("schedule create failed")
		}
		planInput.Version = 1
		planInput.Cron = "30 * * * *"
		updated := request("schedule_update", http.MethodPut, "/api/account-schedules/"+id, planInput)
		if updated["version"] != float64(2) || updated["cron"] != planInput.Cron {
			t.Fatal("schedule update failed")
		}
		deleted := request("schedule_delete", http.MethodDelete, "/api/account-schedules/"+id+"?version=2", nil)
		if deleted["ok"] != true {
			t.Fatal("schedule delete failed")
		}
		entry, err := p.managementState().Get(t.Context(), "account-schedules", a.ID)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := decodeManagedSchedules(&entry)
		if err != nil || len(doc.Plans) != 0 {
			t.Fatal("schedule delete not durable")
		}
		prefix := fmt.Sprintf("local-write-log-%d-", iteration)
		_, err = p.Store.DB.Pool.Exec(t.Context(), `WITH events AS (
 INSERT INTO usage_events(event_id,created_at,status,input_tokens) SELECT $1::text||i,to_timestamp(1700000000),'error',7 FROM generate_series(1,1000)i RETURNING *)
 INSERT INTO request_logs(event_id,created_at,status,model,record)
 SELECT event_id,created_at,status,'local-write-cleanup',jsonb_build_object('event_id',event_id,'time',1700000000,'status',status,'model','local-write-cleanup','input_tokens',7,'http_code',500) FROM events`, prefix)
		if err != nil {
			t.Fatal(err)
		}
		review := request("error_review_update", http.MethodPut, "/api/logs/"+prefix+"1/review", map[string]any{"version": 0, "resolved": true, "note": "Synthetic reviewed failure"})
		if object(review, "review")["resolved"] != true || object(review, "review")["version"] != float64(1) {
			t.Fatal("review mutation failed")
		}
		filters := map[string]string{"from": "1699999999", "to": "1700000001", "model": "local-write-cleanup"}
		snapshot := request("usage_snapshot_1000", http.MethodPost, "/api/logs/snapshot", map[string]any{"filters": filters})
		if snapshot["total"] != float64(1000) {
			t.Fatal("snapshot total mismatch")
		}
		var members int64
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM management_cleanup_targets WHERE preview_id=$1", snapshot["as_of"]).Scan(&members); err != nil || members != 1000 {
			t.Fatalf("snapshot membership %d %v", members, err)
		}
		preview := request("cleanup_preview_1000", http.MethodPost, "/api/logs/cleanup/preview", map[string]any{"filters": filters})
		if preview["total"] != float64(1000) {
			t.Fatal("cleanup preview total mismatch")
		}
		created := request("cleanup_job_create", http.MethodPost, "/api/logs/cleanup/jobs", map[string]any{"preview_id": preview["preview_id"], "confirmation_token": preview["confirmation_token"], "request_id": fmt.Sprintf("local-write-cancel-%d", iteration)})
		jobID := object(created, "job")["id"].(string)
		canceled := request("cleanup_job_cancel", http.MethodPost, "/api/logs/cleanup/jobs/"+jobID+"/cancel", map[string]any{})
		if object(canceled, "job")["status"] != "canceled" {
			t.Fatal("cleanup cancellation failed")
		}
		if err := p.runManagedCleanupBatch(t.Context(), jobID); err != nil {
			t.Fatal(err)
		}
		var logs, events, retained, dimensions int64
		if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM request_logs").Scan(&logs); err != nil || logs != 1000 {
			t.Fatalf("cancel deleted details: %d %v", logs, err)
		}
		preview = request("cleanup_preview_1000", http.MethodPost, "/api/logs/cleanup/preview", map[string]any{"filters": filters})
		created = request("cleanup_job_create", http.MethodPost, "/api/logs/cleanup/jobs", map[string]any{"preview_id": preview["preview_id"], "confirmation_token": preview["confirmation_token"], "request_id": fmt.Sprintf("local-write-apply-%d", iteration)})
		jobID = object(created, "job")["id"].(string)
		workerStart := time.Now()
		if err := p.runManagedCleanupBatch(t.Context(), jobID); err != nil {
			t.Fatal(err)
		}
		performanceSLOEmit(t, "WORKER_COMPLETION", map[string]any{"name": "cleanup_worker_1000", "elapsed_ms": float64(time.Since(workerStart).Microseconds()) / 1000, "not_http_request": true})
		if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM request_logs),(SELECT count(*) FROM usage_events),
 (SELECT COALESCE(sum(requests),0)::bigint FROM retained_log_minute),(SELECT COALESCE(sum(requests),0)::bigint FROM retained_usage_minute WHERE kind='total')`).Scan(&logs, &events, &retained, &dimensions); err != nil || logs != 0 || events != int64((iteration+1)*1000) || retained != 0 || dimensions != 0 {
			t.Fatalf("cleanup damaged retained/durable invariants logs=%d events=%d retained=%d dim=%d err=%v", logs, events, retained, dimensions, err)
		}
		old := adminKey
		rotated := request("admin_key_rotation", http.MethodPost, "/api/settings/rotate-key", map[string]any{"request_id": fmt.Sprintf("local-write-rotation-%d", iteration)})
		fresh, ok := rotated["admin_key"].(string)
		if !ok || fresh == "" || fresh == old {
			t.Fatal("rotation failed")
		}
		adminKey = fresh
		currentKey, err := p.currentAdminKey(t.Context())
		if err != nil || currentKey != fresh {
			t.Fatal("rotation not authoritative")
		}
	}
	if deny.calls.Load() != 0 {
		t.Fatalf("local management invoked upstream %d times", deny.calls.Load())
	}
	names := make([]string, 0, len(measured))
	for name := range measured {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values := measured[name]
		latencies := make([]float64, len(values))
		over := 0
		for i, v := range values {
			latencies[i] = v.MS
			if v.MS >= 1000 {
				over++
			}
		}
		sort.Float64s(latencies)
		percentile := func(p int) float64 { return latencies[(len(latencies)*p+99)/100-1] }
		performanceSLOEmit(t, "WRITE_RESULT", map[string]any{"name": name, "route": routes[name], "samples": values, "failed": 0, "over_slo": over, "threshold_ms": 1000, "max_ms": latencies[len(latencies)-1], "p50_ms": percentile(50), "p95_ms": percentile(95), "p99_ms": percentile(99), "functional_only": !enforce, "scope": "isolated synthetic schema; real admin HTTP; no background worker or provider"})
		if enforce && over > 0 {
			t.Errorf("%s exceeded1s in %d samples", name, over)
		}
	}
}
