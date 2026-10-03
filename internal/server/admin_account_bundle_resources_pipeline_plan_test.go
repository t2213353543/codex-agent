package server

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPGManagedResourceMixedPipelinePlanParityAndBoundedReads(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	group, err := p.Groups.Create("existing-plan-group", "")
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := p.Proxies.Create(ProxyEntry{Name: "existing-plan-proxy", Type: "http", Host: "127.0.0.1", Port: 1234})
	if err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(8, 10, "mixed-plan")
	input.Resources.Groups[1].ID = ""
	input.GroupMapping = map[string]string{input.Resources.Groups[2].ID: group.ID, input.Resources.Groups[3].ID: "missing-group"}
	input.Resources.Groups[4].Name = group.Name
	input.Resources.Groups[5].ID = input.Resources.Groups[0].ID
	input.Resources.Groups[6].Name = ""
	input.Resources.Proxies[1].Port = 0
	input.ProxyMapping = map[string]string{input.Resources.Proxies[2].ID: proxy.ID, input.Resources.Proxies[3].ID: "missing-proxy"}
	input.Resources.Proxies[2].CredentialsIncluded = false // Mapping precedes the credential requirement.
	input.Resources.Proxies[4].Name = proxy.Name
	input.Resources.Proxies[5].ID = input.Resources.Proxies[0].ID
	input.Resources.Proxies[6].CredentialsIncluded = false
	input.Resources.Proxies[7].Password = strings.Repeat("x", 16385)
	before := p.Store.DB.Pool.Stat().AcquireCount()
	reply, handled := p.importManagedBundleResourcesPipeline(t.Context(), input)
	if !handled || reply.Succeeded != 7 || reply.Failed != 11 || len(reply.Results) != 18 {
		t.Fatalf("mixed plan dimensions changed: handled=%t reply=%+v", handled, reply)
	}
	if reads := p.Store.DB.Pool.Stat().AcquireCount() - before; reads != 1 {
		t.Fatalf("mixed input used per-item database acquisitions: %d", reads)
	}
	want := []string{"created", "invalid_resource", "mapped", "mapping_not_found", "name_conflict", "invalid_resource", "invalid_resource", "created", "created", "invalid_resource", "mapped", "mapping_not_found", "name_conflict", "invalid_resource", "credentials_missing", "invalid_resource", "created", "created"}
	created := []string{}
	for i, row := range reply.Results {
		actual := row.Status
		if actual == "failed" {
			actual = row.Code
		}
		if actual != want[i] {
			t.Fatalf("row%d precedence/order got%s want%s", i, actual, want[i])
		}
		if row.Status == "created" {
			created = append(created, row.ID)
		}
	}
	if reply.Results[2].ID != group.ID || reply.Results[10].ID != proxy.ID {
		t.Fatal("mapping target identity changed")
	}
	var stored, audits, transactions int
	if err := p.Store.DB.Pool.QueryRow(t.Context(), resourceDurabilityValidationSQL, created).Scan(&stored, &audits, &transactions); err != nil || stored != 5 || audits != 5 || transactions != 5 {
		t.Fatalf("mixed created rows not independent durable transactions: %d/%d/%d %v", stored, audits, transactions, err)
	}
	// A retry reports name conflicts; it never replays a committed create.
	retried, handled := p.importManagedBundleResourcesPipeline(t.Context(), input)
	if !handled || retried.Succeeded != 2 || retried.Failed != 16 {
		t.Fatalf("retry duplicated creates: %+v", retried)
	}
	for _, i := range []int{0, 7, 8, 16, 17} {
		if retried.Results[i].Code != "name_conflict" {
			t.Fatalf("committed row%d retried: %+v", i, retried.Results[i])
		}
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourceMixedPipelineAllLocalAndLegacyParity(t *testing.T) {
	for _, forceLegacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "pipeline", true: "legacy"}[forceLegacy], func(t *testing.T) {
			p, h := adminPGTestProxy(t)
			group, err := p.Groups.Create("mapped-group", "")
			if err != nil {
				t.Fatal(err)
			}
			proxy, err := p.Proxies.Create(ProxyEntry{Name: "mapped-proxy", Type: "http", Host: "127.0.0.1", Port: 1234})
			if err != nil {
				t.Fatal(err)
			}
			input := managedResourcePipelineFixture(2, 2, "local-plan")
			input.GroupMapping = map[string]string{input.Resources.Groups[0].ID: group.ID}
			input.ProxyMapping = map[string]string{input.Resources.Proxies[0].ID: proxy.ID}
			input.Resources.Groups[1].ID = ""
			input.Resources.Proxies[1].Port = 0
			if forceLegacy {
				if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION keep_resource_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$; CREATE TRIGGER keep_resource_audit BEFORE INSERT ON audit_logs FOR EACH ROW EXECUTE FUNCTION keep_resource_audit()`); err != nil {
					t.Fatal(err)
				}
			}
			var auditBefore int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs").Scan(&auditBefore); err != nil {
				t.Fatal(err)
			}
			var reply managedBundleResourceReply
			if forceLegacy {
				r := managedTestRequest(t, h, http.MethodPost, "/api/accounts/import-resources", input, 200)
				if json.Unmarshal(r.Body.Bytes(), &reply) != nil {
					t.Fatal("invalid legacy response")
				}
			} else {
				var handled bool
				reply, handled = p.importManagedBundleResourcesPipeline(t.Context(), input)
				if !handled {
					t.Fatal("all-local plan declined")
				}
			}
			if reply.Succeeded != 2 || reply.Failed != 2 || reply.Results[0].Status != "mapped" || reply.Results[1].Code != "invalid_resource" || reply.Results[2].Status != "mapped" || reply.Results[3].Code != "invalid_resource" {
				t.Fatalf("mapped/invalid-only plan changed: %+v", reply)
			}
			if !reflect.DeepEqual(reply.GroupMapping, map[string]string{input.Resources.Groups[0].ID: group.ID}) || !reflect.DeepEqual(reply.ProxyMapping, map[string]string{input.Resources.Proxies[0].ID: proxy.ID}) {
				t.Fatal("local mapping response changed")
			}
			var auditAfter int
			if err := p.Store.DB.Pool.QueryRow(t.Context(), "SELECT count(*) FROM audit_logs").Scan(&auditAfter); err != nil || auditAfter != auditBefore {
				t.Fatal("local-only mapping created audit/write", err)
			}
			assertManagedResourceGatesReleased(t, p)
		})
	}
}

func TestPGManagedResourceMixedPipelineConstraintFailureKeepsLaterItems(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `ALTER TABLE groups ADD CONSTRAINT mixed_group_failure CHECK(name<>'mixed-failure-group-0000'); ALTER TABLE proxies ADD CONSTRAINT mixed_proxy_failure CHECK(name<>'mixed-failure-proxy-0000')`); err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(3, 3, "mixed-failure")
	input.Resources.Groups[1].ID = ""
	input.Resources.Proxies[1].Port = 0
	reply, handled := p.importManagedBundleResourcesPipeline(t.Context(), input)
	if !handled || reply.Succeeded != 2 || reply.Failed != 4 || reply.Results[0].Code != "invalid_request" || reply.Results[1].Code != "invalid_resource" || reply.Results[2].Status != "created" || reply.Results[3].Code != "operation_failed" || reply.Results[4].Code != "invalid_resource" || reply.Results[5].Status != "created" {
		t.Fatalf("SQL failure lost ordered local errors or later independent writes: %+v", reply)
	}
	assertManagedResourceGatesReleased(t, p)
}

func TestPGManagedResourceMixedPipelineCancelDoesNotAcknowledgeMappedTail(t *testing.T) {
	p, _ := adminPGTestProxy(t)
	existing, err := p.Proxies.Create(ProxyEntry{Name: "cancel-mapped-target", Type: "http", Host: "127.0.0.1", Port: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Store.DB.Pool.Exec(t.Context(), `CREATE FUNCTION mixed_cancel_barrier(n text) RETURNS boolean LANGUAGE plpgsql AS $$ BEGIN IF n='mixed-cancel-proxy-0001' THEN PERFORM pg_advisory_xact_lock(761043); END IF; RETURN true; END $$; ALTER TABLE proxies ADD CONSTRAINT mixed_cancel_barrier CHECK(mixed_cancel_barrier(name))`); err != nil {
		t.Fatal(err)
	}
	blocker, err := p.Store.DB.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = blocker.Exec(ctx, `SELECT pg_advisory_unlock(761043)`)
		blocker.Release()
	}()
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_lock(761043)`); err != nil {
		t.Fatal(err)
	}
	input := managedResourcePipelineFixture(0, 100, "mixed-cancel")
	input.Resources.Proxies[2].Port = 0
	input.ProxyMapping = map[string]string{input.Resources.Proxies[99].ID: existing.ID}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan managedBundleResourceReply, 1)
	go func() { reply, _ := p.importManagedBundleResourcesPipeline(ctx, input); done <- reply }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var first bool
		if err := p.Store.DB.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM proxies WHERE name='mixed-cancel-proxy-0000')`).Scan(&first); err != nil {
			t.Fatal(err)
		}
		if first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first create never committed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case reply := <-done:
		if len(reply.Results) != 100 || reply.Results[99].Code != "canceled" || reply.ProxyMapping[input.Resources.Proxies[99].ID] != "" {
			t.Fatal("cancellation acknowledged a mapped tail before reaching it")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mixed import ignored cancellation")
	}
}
