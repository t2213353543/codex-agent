package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type ingressPageFixture struct {
	Items     []gatewayIngressEvent         `json:"items"`
	Total     int                           `json:"total"`
	HasMore   bool                          `json:"has_more"`
	Next      string                        `json:"next_cursor"`
	Scope     string                        `json:"scope"`
	Source    string                        `json:"source"`
	Limit     int                           `json:"retention_limit"`
	Evicted   int64                         `json:"evicted_total"`
	Collector gatewayIngressCollectorStatus `json:"collector"`
}

func ingressFixture(index int) gatewayIngressEvent {
	return gatewayIngressEvent{ID: fmt.Sprintf("ingress-fixture-%d", index), TimeMs: 1700000000000 + int64(index)*1000, HTTPCode: 401, Reason: "invalid_api_key", Endpoint: "/v1/responses", ClientRequestID: "client-fixture"}
}
func TestManagedIngressEvents(t *testing.T)   { exerciseManagedIngressEvents(t, webTestSetup) }
func TestPGManagedIngressEvents(t *testing.T) { exerciseManagedIngressEvents(t, adminPGTestProxy) }
func exerciseManagedIngressEvents(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, h := setup(t)
	unauth := webTestRequest(h, "GET", "/api/ingress-events", "", "")
	if unauth.Code != 401 {
		t.Fatal("ingress observation is not admin-only")
	}
	read := func(query string) ingressPageFixture {
		t.Helper()
		res := managedTestRequest(t, h, "GET", "/api/ingress-events"+query, nil, 200)
		var page ingressPageFixture
		if json.Unmarshal(res.Body.Bytes(), &page) != nil {
			t.Fatal("invalid page")
		}
		if page.Source != gatewayIngressSource || page.Scope != "retained_ingress_events" || page.Limit != 1000 {
			t.Fatal("missing scope")
		}
		return page
	}
	if page := read(""); len(page.Items) != 0 || page.Collector.Configured {
		t.Fatal("uninitialized fixture collector claims running")
	}
	sink := &gatewayIngressEventSink{store: p.managementState()}
	appendBatch := func(start, end int) {
		t.Helper()
		batch := []gatewayIngressEvent{}
		for i := start; i < end; i++ {
			batch = append(batch, ingressFixture(i))
		}
		if err := sink.appendEvents(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	appendBatch(0, 5)
	first := read("?limit=2")
	if first.Total != 5 || len(first.Items) != 2 || !first.HasMore || first.Items[0].Sequence != 5 {
		t.Fatal("first page order/total")
	}
	appendBatch(5, 6)
	next := read("?limit=2&cursor=" + url.QueryEscape(first.Next))
	if next.Total != 5 || next.Items[0].Sequence != 3 {
		t.Fatal("new arrivals changed fixed ceiling")
	}
	last := read("?limit=2&cursor=" + url.QueryEscape(next.Next))
	if last.Total != 5 || last.HasMore || len(last.Items) != 1 || last.Items[0].Sequence != 1 {
		t.Fatal("last page")
	}
	filtered := read("?status=401&reason=invalid_api_key&client_request_id=client-fixture&from=1700000001&to=1700000004")
	if filtered.Total != 3 || len(filtered.Items) != 3 {
		t.Fatal("half-open time/correlation/status filtering")
	}
	if read("?status=503").Total != 0 {
		t.Fatal("status filter mismatch")
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?status=0", "?status=200", "?reason=arbitrary", "?client_request_id=%0Asecret", "?from=99999999999999", "?from=2&to=1", "?limit=1&limit=2", "?token=secret", "?cursor=invalid", "?status=%ZZ", "?status=401;reason=invalid_api_key"} {
		managedTestRequest(t, h, "GET", "/api/ingress-events"+query, nil, 400)
	}
	managedTestRequest(t, h, "GET", "/api/ingress-events?limit=2&status=401&cursor="+url.QueryEscape(first.Next), nil, 400)
	// The retained ring and its monotonic eviction count are global to the store.
	appendBatch(6, 1007)
	page := read("?limit=200")
	if page.Total != 1000 || page.Evicted != 7 || page.Items[0].Sequence != 1007 {
		t.Fatal("ring not bounded")
	}
	managedTestRequest(t, h, "GET", "/api/ingress-events?limit=2&cursor="+url.QueryEscape(first.Next), nil, 409)
	beforeEvicted := page.Evicted
	appendBatch(1000, 1007)
	if after := read(""); after.Evicted != beforeEvicted || after.Items[0].Sequence != 1007 {
		t.Fatal("retained duplicate IDs appended twice")
	}
	resetCursor := read("?limit=2").Next
	if err := p.managementState().Delete(context.Background(), "ingress-events", "recent", 0); err != nil {
		t.Fatal(err)
	}
	appendBatch(2000, 2010)
	managedTestRequest(t, h, "GET", "/api/ingress-events?limit=2&cursor="+url.QueryEscape(resetCursor), nil, 409)
	if result := managedTestRequest(t, h, "GET", "/api/logs?exact_total=true", nil, 200); strings.Contains(result.Body.String(), "ingress-fixture") {
		t.Fatal("ingress entered business logs")
	}
	if p.Store.DB != nil {
		for _, table := range []string{"request_logs", "usage_events"} {
			var count int
			if err := p.Store.DB.Pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
				t.Fatalf("ingress changed %s: %v", table, err)
			}
		}
	}
}

func TestGatewayIngressCollectorLifecycleAndPrivacy(t *testing.T) {
	p, h := webTestSetup(t)
	if err := p.startGatewayIngressEvents(); err != nil {
		t.Fatal(err)
	}
	if err := p.startGatewayIngressEvents(); err == nil {
		t.Fatal("collector started twice")
	}
	zero := int64(0)
	p.recordGatewayIngressEvent(401, "invalid_api_key", "/v1/responses?secret=HIDDEN", "Bearer HIDDEN", &zero)
	p.recordGatewayIngressEvent(503, "settings_unavailable", "/v1/models", "safe-client-123", nil)
	p.recordGatewayIngressEvent(500, "untrusted-secret-reason", "/secret", "secret", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.IngressEvents.Close(ctx); err != nil {
		t.Fatal(err)
	}
	p.recordGatewayIngressEvent(401, "invalid_api_key", "/v1/responses", "after-close", nil)
	r := managedTestRequest(t, h, "GET", "/api/ingress-events", nil, 200)
	var page ingressPageFixture
	_ = json.Unmarshal(r.Body.Bytes(), &page)
	if page.Total != 2 || page.Collector.Running || !page.Collector.Configured || page.Collector.QueueDropped != 1 || page.Collector.Pending != 0 || page.Collector.Queued != 0 {
		t.Fatal("bad final collector counters")
	}
	if strings.Contains(r.Body.String(), "HIDDEN") || strings.Contains(r.Body.String(), "untrusted") {
		t.Fatal("raw metadata persisted")
	}
	if page.Items[1].AuthenticationMs == nil || *page.Items[1].AuthenticationMs != 0 || page.Items[1].Endpoint != "" || page.Items[1].ClientRequestID != "" {
		t.Fatal("zero/nil or allowlist mismatch")
	}
	p.Metrics.mu.Lock()
	businessRecords := len(p.Metrics.accounts)
	p.Metrics.mu.Unlock()
	if businessRecords != 0 {
		t.Fatal("ingress modified metrics")
	}
	if err := (&Proxy{}).startGatewayIngressEvents(); err == nil {
		t.Fatal("missing storage silently enabled")
	}
	if err := (*gatewayIngressEventSink)(nil).Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayIngressQueueAndUncertainPersistence(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var batches [][]string
	s := &gatewayIngressEventSink{queue: make(chan gatewayIngressEvent, 256), stop: make(chan struct{}), done: make(chan struct{}), accepting: true, prefix: "test-instance"}
	s.persist = func(ctx context.Context, items []gatewayIngressEvent) error {
		calls++
		ids := []string{}
		for _, event := range items {
			ids = append(ids, event.ID)
		}
		batches = append(batches, ids)
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return errors.New("synthetic ambiguous commit with PRIVATE diagnostics")
		}
		return nil
	}
	for i := 0; i < 256; i++ {
		s.enqueue(ingressFixture(i))
	}
	go s.run()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not begin")
	}
	started := time.Now()
	for i := 0; i < 512; i++ {
		s.enqueue(ingressFixture(i + 256))
	}
	if time.Since(started) > time.Second {
		t.Fatal("enqueue blocked on persistence")
	}
	if status := s.status(); status.Pending != 256 || status.Queued != 256 || status.QueueDropped != 256 {
		t.Fatal("buffer bound violated")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status := s.status()
	if calls != 2 || status.PersistenceFailures != 1 || status.PersistenceUnconfirmed != 256 || status.QueueDropped != 256 || status.Pending != 0 || status.LastError != "" {
		t.Fatal("ambiguous persistence did not recover truthfully")
	}
	seen := map[string]bool{}
	for _, id := range batches[0] {
		seen[id] = true
	}
	for _, id := range batches[1] {
		if seen[id] {
			t.Fatal("ambiguous batch was replayed")
		}
	}
}

func TestGatewayIngressCancellationAndConcurrentPersistence(t *testing.T) {
	store := managementStateStore{directory: t.TempDir()}
	s := &gatewayIngressEventSink{store: store}
	managementFileMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := s.appendEvents(ctx, []gatewayIngressEvent{ingressFixture(1)})
	cancel()
	managementFileMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("file mutex ignored cancellation")
	}
	name, _ := managementStateKey("ingress-events", "recent")
	file := store.file(name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file+".lock", 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Millisecond)
	err = s.appendEvents(ctx, []gatewayIngressEvent{ingressFixture(1)})
	cancel()
	_ = os.Remove(file + ".lock")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("file directory lock ignored cancellation")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sink := &gatewayIngressEventSink{store: store}
			batch := []gatewayIngressEvent{}
			for n := 0; n < 150; n++ {
				batch = append(batch, ingressFixture(index*150+n))
			}
			if err := sink.appendEvents(context.Background(), batch); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	entry, err := store.Get(context.Background(), "ingress-events", "recent")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeGatewayIngressDocument(entry)
	if err != nil || len(doc.Events) != 1000 || doc.NextSequence != 1200 || doc.EvictedTotal != 200 {
		t.Fatal("concurrent writer lost ring events")
	}
}

func TestGatewayIngressCorruptStorageFailsClosed(t *testing.T) {
	p, h := webTestSetup(t)
	if err := p.managementState().Update(context.Background(), "ingress-events", "recent", func(entry *managementStateEntry) error {
		entry.Payload = json.RawMessage(`{"epoch":"fixture","next_sequence":1,"events":[{"id":"secret\n","sequence":1,"time_ms":1,"http_code":401,"reason":"invalid_api_key"}]}`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := managedTestRequest(t, h, "GET", "/api/ingress-events", nil, 500)
	if strings.Contains(r.Body.String(), "secret") {
		t.Fatal("corrupt event leaked")
	}
	empty := &Proxy{}
	w := httptest.NewRecorder()
	empty.handleManagedIngressEvents(w, httptest.NewRequest("GET", "/api/ingress-events", nil))
	if w.Code != 503 {
		t.Fatal("unconfigured storage did not fail closed")
	}
}

func TestGatewayIngressShutdownFailureAndBounds(t *testing.T) {
	t.Run("shutdown_error_is_not_reported_as_flushed", func(t *testing.T) {
		s := &gatewayIngressEventSink{queue: make(chan gatewayIngressEvent, 256), stop: make(chan struct{}), done: make(chan struct{}), accepting: true, prefix: "shutdown-failure"}
		s.persist = func(context.Context, []gatewayIngressEvent) error { return errors.New("synthetic unavailable") }
		s.enqueue(ingressFixture(1))
		go s.run()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err == nil {
			t.Fatal("failed shutdown flush reported success")
		}
		status := s.status()
		if status.PersistenceUnconfirmed != 1 || status.PersistenceFailures != 1 || status.LastError != "storage_unavailable" {
			t.Fatal("shutdown failure counters")
		}
	})
	t.Run("deadline_stops_worker_without_per_event_goroutines", func(t *testing.T) {
		s := &gatewayIngressEventSink{queue: make(chan gatewayIngressEvent, 256), stop: make(chan struct{}), done: make(chan struct{}), accepting: true, prefix: "shutdown-deadline"}
		s.persist = func(ctx context.Context, _ []gatewayIngressEvent) error { <-ctx.Done(); return ctx.Err() }
		for i := 0; i < 256; i++ {
			s.enqueue(ingressFixture(i))
		}
		go s.run()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := s.Close(ctx); err == nil {
			t.Fatal("deadline reported flushed")
		}
		select {
		case <-s.done:
		case <-time.After(time.Second):
			t.Fatal("worker did not exit after cancel")
		}
		if status := s.status(); status.PersistenceUnconfirmed+status.UnpersistedAtShutdown != 256 {
			t.Fatal("shutdown lost unconfirmed sample count")
		}
	})
	t.Run("maximal_metadata_stays_bounded", func(t *testing.T) {
		p, h := webTestSetup(t)
		if err := p.startGatewayIngressEvents(); err != nil {
			t.Fatal(err)
		}
		valid := strings.Repeat("x", 128)
		invalid := strings.Repeat("y", 129)
		negative := int64(-1)
		p.recordGatewayIngressEvent(401, "invalid_api_key", "/v1/responses", valid, nil)
		p.recordGatewayIngressEvent(401, "invalid_api_key", strings.Repeat("/secret", 1000), invalid, &negative)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.IngressEvents.Close(ctx); err != nil {
			t.Fatal(err)
		}
		res := managedTestRequest(t, h, "GET", "/api/ingress-events", nil, 200)
		var page ingressPageFixture
		_ = json.Unmarshal(res.Body.Bytes(), &page)
		if page.Total != 2 || page.Items[0].Endpoint != "" || page.Items[0].ClientRequestID != "" || page.Items[0].AuthenticationMs != nil || page.Items[1].ClientRequestID != valid {
			t.Fatal("metadata length/presence bounds")
		}
		entry, err := p.managementState().Get(context.Background(), "ingress-events", "recent")
		if err != nil {
			t.Fatal(err)
		}
		if len(entry.Payload) > 4096 {
			t.Fatal("bounded fields produced oversized document")
		}
	})
}
