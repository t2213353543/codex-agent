package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func managedResourcePipelineFixture(groups, proxies int, prefix string) managedBundleResourcesInput {
	input := managedBundleResourcesInput{Resources: managedBundleResources{Version: 1}, ConfirmProxyCredentials: true}
	for i := range groups {
		input.Resources.Groups = append(input.Resources.Groups, managedBackupGroup{ID: fmt.Sprintf("source-g-%d", i), Name: fmt.Sprintf("%s-group-%04d", prefix, i), Description: "synthetic restored description"})
	}
	for i := range proxies {
		input.Resources.Proxies = append(input.Resources.Proxies, managedBackupProxy{ID: fmt.Sprintf("source-p-%d", i), Name: fmt.Sprintf("%s-proxy-%04d", prefix, i), Type: "http", Host: "127.0.0.1", Port: 1234 + i%1000, Enabled: i%2 == 0, HasCredentials: true, CredentialsIncluded: true, Username: "synthetic-resource-user", Password: fmt.Sprintf("synthetic-resource-password-%d", i)})
	}
	return input
}

func TestManagedResourcePipelineEligibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*managedBundleResourcesInput)
	}{
		{"dry_run", func(v *managedBundleResourcesInput) { v.DryRun = true }},
		{"group_duplicate_id", func(v *managedBundleResourcesInput) { v.Resources.Groups[1].ID = v.Resources.Groups[0].ID }},
		{"group_duplicate_name", func(v *managedBundleResourcesInput) { v.Resources.Groups[1].Name = v.Resources.Groups[0].Name }},
		{"group_mapping", func(v *managedBundleResourcesInput) {
			v.GroupMapping = map[string]string{v.Resources.Groups[0].ID: "local-group"}
		}},
		{"proxy_duplicate_id", func(v *managedBundleResourcesInput) { v.Resources.Proxies[1].ID = v.Resources.Proxies[0].ID }},
		{"proxy_duplicate_name", func(v *managedBundleResourcesInput) { v.Resources.Proxies[1].Name = v.Resources.Proxies[0].Name }},
		{"proxy_mapping", func(v *managedBundleResourcesInput) {
			v.ProxyMapping = map[string]string{v.Resources.Proxies[0].ID: "local-proxy"}
		}},
		{"invalid_proxy", func(v *managedBundleResourcesInput) { v.Resources.Proxies[0].Port = 0 }},
		{"missing_credentials", func(v *managedBundleResourcesInput) { v.Resources.Proxies[0].CredentialsIncluded = false }},
		{"credentials_confirmation", func(v *managedBundleResourcesInput) { v.ConfirmProxyCredentials = false }},
		{"long_password", func(v *managedBundleResourcesInput) { v.Resources.Proxies[0].Password = strings.Repeat("p", 16385) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := managedResourcePipelineFixture(2, 2, "eligible")
			if !managedResourcePipelineEligible(input) {
				t.Fatal("normal input not eligible")
			}
			tc.edit(&input)
			if managedResourcePipelineEligible(input) {
				t.Fatal("non-independent input unexpectedly eligible")
			}
		})
	}
}

func TestManagedResourcePipelineCachePublication(t *testing.T) {
	p := &Proxy{Proxies: &ProxyStore{index: map[string]*ProxyEntry{"stale": {ID: "stale"}, "existing": {ID: "existing", Version: 4, Password: "newer", Enabled: true}}}}
	p.publishManagedResourceProxy(ProxyEntry{ID: "first", Version: 1, Password: "first-password", Enabled: false}, []ProxyEntry{{ID: "existing", Version: 3, Password: "older", Enabled: true}}, true)
	p.publishManagedResourceProxy(ProxyEntry{ID: "second", Version: 1, Password: "second-password", Enabled: true}, nil, false)
	if _, ok := p.Proxies.index["stale"]; ok {
		t.Fatal("validated full cache retained deleted entry")
	}
	if len(p.Proxies.index) != 3 || p.Proxies.index["existing"].Password != "newer" || p.Proxies.index["first"].Password != "first-password" {
		t.Fatal("cache publication lost credentials or replaced newer version")
	}
	if _, ok := p.Proxies.Get("first"); ok {
		t.Fatal("disabled restored proxy became enabled")
	}
	if v, ok := p.Proxies.Get("second"); !ok || v.Password != "second-password" {
		t.Fatal("runtime lookup missed committed proxy")
	}
}

func TestManagedResourcePreviewDoesNotEraseRuntimeProxyPassword(t *testing.T) {
	p, handler := webTestSetup(t)
	v, err := p.Proxies.Create(ProxyEntry{Name: "existing-auth-proxy", Type: "http", Host: "127.0.0.1", Port: 1, Password: "synthetic-retained-password", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(2, 2, "file-preview")
	input.DryRun = true
	managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	if cached, ok := p.Proxies.Get(v.ID); !ok || cached.Password != v.Password {
		t.Fatal("resource preview redacted shared runtime proxy credentials")
	}
}

func TestPGManagedResourcePipelineIndependentCommits(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	input := managedResourcePipelineFixture(40, 40, "pipeline")
	before := p.Store.DB.Pool.Stat().AcquireCount()
	r := managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	if acquires := p.Store.DB.Pool.Stat().AcquireCount() - before; acquires > 8 {
		t.Fatalf("resource import still acquired per item: %d", acquires)
	}
	var reply managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Succeeded != 80 || reply.Failed != 0 || len(reply.Results) != 80 {
		t.Fatal("independent resource import result mismatch")
	}
	if bytes.Contains(r.Body.Bytes(), []byte("synthetic-resource-password")) || bytes.Contains(r.Body.Bytes(), []byte("synthetic-resource-user")) {
		t.Fatal("resource import response leaked credentials")
	}
	for i, row := range reply.Results {
		wantKind, wantID := "group", fmt.Sprintf("source-g-%d", i)
		if i >= 40 {
			wantKind, wantID = "proxy", fmt.Sprintf("source-p-%d", i-40)
		}
		if row.Kind != wantKind || row.SourceID != wantID || row.Status != "created" || row.ID == "" {
			t.Fatalf("resource order/result mismatch at %d", i)
		}
	}
	var distinctTransactions, audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(DISTINCT x) FROM (SELECT xmin::text AS x FROM groups UNION ALL SELECT xmin::text FROM proxies) q`).Scan(&distinctTransactions); err != nil || distinctTransactions != 80 {
		t.Fatalf("items did not commit independently: transactions=%d err=%v", distinctTransactions, err)
	}
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_logs WHERE action IN ('group.create','proxy.restore')`).Scan(&audits); err != nil || audits != 80 {
		t.Fatalf("per-item audit mismatch: %d %v", audits, err)
	}
	for _, original := range input.Resources.Proxies {
		id := reply.ProxyMapping[original.ID]
		var encrypted []byte
		if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT password_cipher FROM proxies WHERE id=$1`, id).Scan(&encrypted); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encrypted, []byte(original.Password)) {
			t.Fatal("proxy password stored without encryption")
		}
		plain, err := p.Store.DB.Decrypt(encrypted)
		if err != nil || string(plain) != original.Password {
			t.Fatal("encrypted proxy password failed roundtrip")
		}
		p.Proxies.mu.RLock()
		cached := *p.Proxies.index[id]
		p.Proxies.mu.RUnlock()
		if cached.Enabled != original.Enabled || cached.Password != original.Password || cached.Username != original.Username || cached.Version != 1 {
			t.Fatal("runtime index lost restored state")
		}
	}
	assertManagedResourceGatesReleased(t, p)
}

func assertManagedResourceGatesReleased(t *testing.T, p *Proxy) {
	t.Helper()
	conn, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, lock := range []struct {
		suffix string
		seed   int
	}{{"proxy-resource-import", 981}, {"proxy-resource-write", 982}} {
		var count int
		if err := conn.QueryRow(t.Context(), `WITH wanted AS(SELECT hashtextextended(current_schema() || ':' || $1,$2) AS id)
 SELECT count(*) FROM pg_locks,wanted WHERE locktype='advisory' AND granted AND objsubid=1
 AND classid=((wanted.id>>32)&4294967295)::oid AND objid=(wanted.id&4294967295)::oid`, lock.suffix, lock.seed).Scan(&count); err != nil || count != 0 {
			t.Fatalf("resource session lock remains owned: %s count=%d err=%v", lock.suffix, count, err)
		}
		var held bool
		if err := conn.QueryRow(t.Context(), `SELECT pg_try_advisory_lock(hashtextextended(current_schema() || ':' || $1,$2))`, lock.suffix, lock.seed).Scan(&held); err != nil || !held {
			t.Fatalf("resource gate leaked: %s %v", lock.suffix, err)
		}
		if _, err := conn.Exec(t.Context(), `SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':' || $1,$2))`, lock.suffix, lock.seed); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPGManagedResourcePipelinePartialConstraintFailure(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `ALTER TABLE groups ADD CONSTRAINT synthetic_group_restore_check CHECK(name<>'partial-group-0001'); ALTER TABLE proxies ADD CONSTRAINT synthetic_proxy_restore_check CHECK(name<>'partial-proxy-0001')`); err != nil {
		t.Fatal(err)
	}
	reply, handled := p.importManagedBundleResourcesPipeline(t.Context(), managedResourcePipelineFixture(3, 3, "partial"))
	if !handled || reply.Succeeded != 4 || reply.Failed != 2 || reply.Results[1].Code != "invalid_request" || reply.Results[4].Code != "operation_failed" {
		t.Fatalf("partial commit/error mapping changed: handled=%v reply=%+v", handled, reply)
	}
	for _, i := range []int{0, 2, 3, 5} {
		if reply.Results[i].Status != "created" {
			t.Fatalf("one item failure aborted independent item %d", i)
		}
	}
	var groups, proxies, audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM groups),(SELECT count(*) FROM proxies),(SELECT count(*) FROM audit_logs WHERE action IN ('group.create','proxy.restore'))`).Scan(&groups, &proxies, &audits); err != nil || groups != 2 || proxies != 2 || audits != 4 {
		t.Fatalf("partial durability/audits mismatch: %d/%d/%d err=%v", groups, proxies, audits, err)
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourcePipelineFallbackPreservesCipherFailure(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO proxies(id,name,type,host,port,password_cipher) VALUES('broken','broken','http','127.0.0.1',1,decode('01','hex'))`); err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(0, 2, "badcipher")
	if _, handled := p.importManagedBundleResourcesPipeline(t.Context(), input); handled {
		t.Fatal("invalid existing cipher bypassed the original fallback")
	}
	assertManagedResourceGatesReleased(t, p)
	r := managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	var reply managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Succeeded != 0 || reply.Failed != 2 {
		t.Fatal("post-commit credential-validation failure semantics changed")
	}
	var rows int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM proxies WHERE name LIKE 'badcipher-%'`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("legacy committed-but-failed proxy rows changed: %d %v", rows, err)
	}
}

func TestPGManagedResourcePipelineUnknownTriggerFallback(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION resource_test_audit_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='proxy.restore' AND (SELECT name FROM proxies WHERE id=NEW.entity_id)='trigger-proxy-0001' THEN RAISE EXCEPTION 'synthetic resource audit rejection'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER resource_test_audit_reject BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION resource_test_audit_reject()`); err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(0, 3, "trigger")
	if _, handled := p.importManagedBundleResourcesPipeline(t.Context(), input); handled {
		t.Fatal("unknown audit trigger bypassed original ordered path")
	}
	r := managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	var reply managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &reply) != nil || reply.Succeeded != 2 || reply.Failed != 1 || reply.Results[1].Status != "failed" {
		t.Fatal("audit failure did not preserve per-item rollback and remaining successes")
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourcePipelineMappedAndInvalidParity(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	input := managedResourcePipelineFixture(2, 2, "parity")
	preview := input
	preview.DryRun = true
	r := managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", preview, http.StatusOK)
	var planned managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &planned) != nil || planned.Succeeded != 4 || planned.Failed != 0 {
		t.Fatal("dry-run planning changed")
	}
	var rows int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM groups)+(SELECT count(*) FROM proxies)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("dry-run persisted resources")
	}
	r = managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	var created managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &created) != nil || created.Succeeded != 4 {
		t.Fatal("normal restore failed")
	}
	input.GroupMapping, input.ProxyMapping = created.GroupMapping, created.ProxyMapping
	input.Resources.Groups = append(input.Resources.Groups, input.Resources.Groups[0])
	input.Resources.Proxies = append(input.Resources.Proxies, managedBackupProxy{ID: "invalid"})
	r = managedTestRequest(t, handler, http.MethodPost, "/api/accounts/import-resources", input, http.StatusOK)
	var mapped managedBundleResourceReply
	if json.Unmarshal(r.Body.Bytes(), &mapped) != nil || mapped.Succeeded != 4 || mapped.Failed != 2 || mapped.Results[2].Code != "invalid_resource" || mapped.Results[5].Code != "invalid_resource" || !reflect.DeepEqual(mapped.GroupMapping, created.GroupMapping) || !reflect.DeepEqual(mapped.ProxyMapping, created.ProxyMapping) {
		t.Fatal("mapping/duplicate/invalid ordering changed")
	}
}

func TestPGManagedResourcePipelineConcurrentNameConflicts(t *testing.T) {
	p, handler := adminPGTestProxy(t)
	input := managedResourcePipelineFixture(0, 48, "concurrent")
	raw, _ := json.Marshal(input)
	start := make(chan struct{})
	type outcome struct {
		status int
		reply  managedBundleResourceReply
		err    error
	}
	done := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			r := webTestRequest(handler, http.MethodPost, "/api/accounts/import-resources", string(raw), "Bearer "+webTestKey)
			out := outcome{status: r.Code}
			out.err = json.Unmarshal(r.Body.Bytes(), &out.reply)
			done <- out
		}()
	}
	close(start)
	succeeded, failed := 0, 0
	for range 2 {
		out := <-done
		if out.status != http.StatusOK || out.err != nil || len(out.reply.Results) != 48 {
			t.Fatal("concurrent resource request failed unexpectedly")
		}
		succeeded += out.reply.Succeeded
		failed += out.reply.Failed
		for _, row := range out.reply.Results {
			if row.Status == "failed" && row.Code != "name_conflict" && row.Code != "conflict" {
				t.Fatalf("unexpected concurrent conflict code %q", row.Code)
			}
		}
	}
	var rows, names, audits int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT count(*),count(DISTINCT name),(SELECT count(*) FROM audit_logs WHERE action='proxy.restore') FROM proxies`).Scan(&rows, &names, &audits); err != nil || rows != 48 || names != 48 || audits != 48 || succeeded != 48 || failed != 48 {
		t.Fatalf("concurrent resource uniqueness/audit mismatch: %d/%d/%d succeeded=%d failed=%d err=%v", rows, names, audits, succeeded, failed, err)
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourceWriteGateBlocksConcurrentCorruption(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `INSERT INTO proxies(id,name,type,host,port) VALUES('gate-target','gate-target','http','127.0.0.1',1)`); err != nil {
		t.Fatal(err)
	}
	owner, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(t.Context(), `SELECT pg_advisory_lock(hashtextextended(current_schema() || ':proxy-resource-import',981)); SELECT pg_advisory_lock(hashtextextended(current_schema() || ':proxy-resource-write',982))`); err != nil {
		owner.Release()
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			releaseManagedResourceConnection(owner, true, true)
			released = true
		}
	}
	defer release()
	writer, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	var pid int
	if err := writer.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := writer.Exec(t.Context(), `UPDATE proxies SET password_cipher=decode('01','hex') WHERE id='gate-target'`)
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("concurrent proxy corruption crossed gate: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("writer never reached shared gate lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shared writer did not resume after exclusive gate release")
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourcePipelineCancelKeepsCommittedPrefix(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION resource_cancel_barrier(n text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN IF n='cancel-proxy-0001' THEN PERFORM pg_advisory_xact_lock(761042); END IF; RETURN true; END $$; ALTER TABLE proxies ADD CONSTRAINT resource_cancel_barrier CHECK(resource_cancel_barrier(name))`); err != nil {
		t.Fatal(err)
	}
	blocker, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_, _ = blocker.Exec(cleanup, `SELECT pg_advisory_unlock(761042)`)
		blocker.Release()
	}()
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_lock(761042)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan managedBundleResourceReply, 1)
	go func() {
		reply, _ := p.importManagedBundleResourcesPipeline(ctx, managedResourcePipelineFixture(0, 100, "cancel"))
		done <- reply
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var first bool
		if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM proxies WHERE name='cancel-proxy-0000')`).Scan(&first); err != nil {
			t.Fatal(err)
		}
		if first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first independent item never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case reply := <-done:
		if len(reply.Results) != 100 || reply.Results[0].Status != "created" || reply.Results[99].Code != "canceled" {
			t.Fatal("cancel lost acknowledged prefix or dispatched a later chunk")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resource pipeline did not honor cancellation")
	}
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_unlock(761042)`); err != nil {
		t.Fatal(err)
	}
	var first, later bool
	if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM proxies WHERE name='cancel-proxy-0000'),EXISTS(SELECT 1 FROM proxies WHERE name='cancel-proxy-0099')`).Scan(&first, &later); err != nil || !first || later {
		t.Fatalf("cancel durability mismatch: first=%v later=%v %v", first, later, err)
	}
}
