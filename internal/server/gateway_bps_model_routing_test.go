package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-agent/internal/bps"
)

const bpsRouteFixtureCatalog = `{"allowed":true,"model_catalog":{"default_model":"gpt-6-astra","models":[{"id":"gpt-6-astra","efforts":[{"value":"low"},{"value":"medium"},{"value":"high"},{"value":"xhigh"}],"default_effort":"medium"},{"id":"gpt-5.6-sol","efforts":[{"value":"none"},{"value":"low"},{"value":"medium"},{"value":"high"},{"value":"xhigh"}],"default_effort":"medium"},{"id":"gpt-6-sol","efforts":[{"value":"low"},{"value":"high"}],"default_effort":"high"},{"id":"gpt-5.6-luna","efforts":[{"value":"none"},{"value":"low"},{"value":"medium"}],"default_effort":"medium"}]}}`

func bpsRouteCatalogFixture(t *testing.T) *bps.AccessCatalog {
	t.Helper()
	catalog, err := bps.ParseAccessCatalog([]byte(bpsRouteFixtureCatalog))
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestBPSModelRoutingUsesAdvertisedNearestModelAndEffort(t *testing.T) {
	catalog := bpsRouteCatalogFixture(t)
	for _, tc := range []struct{ model, effort, wantModel, wantEffort string }{
		{"gpt-5.5", "max", "gpt-5.6-sol", "xhigh"},
		{"gpt-5.4", "ultra", "gpt-5.6-sol", "xhigh"},
		{"gpt-reserve", "max", "gpt-5.6-sol", "xhigh"},
		{"gpt-5.2", "high", "gpt-5.6-sol", "high"},
		{"codex-auto-review", "", "gpt-6-astra", "xhigh"},
		{"codex-auto", "low", "gpt-6-astra", "low"},
		{"gpt-6-astra", "none", "gpt-6-astra", "low"},
		{"gpt-6-sol", "medium", "gpt-6-sol", "high"},
		{"gpt-6-astra", "medium", "gpt-6-astra", "medium"},
		{"gpt-6-astra", "", "gpt-6-astra", ""},
		{"gpt-5.1-codex-mini", "max", "gpt-5.6-luna", "medium"},
	} {
		route, err := chooseBPSModelRoute(catalog, tc.model, tc.effort)
		if err != nil || route.ResolvedModel != tc.wantModel || route.ResolvedEffort != tc.wantEffort {
			t.Errorf("%s/%s => %s/%s, error %v", tc.model, tc.effort, route.ResolvedModel, route.ResolvedEffort, err)
		}
	}
	for _, name := range []string{"gpt-5.55-typo", "gpt-daybreak-blue-latest", "arbitrary-model"} {
		route, err := chooseBPSModelRoute(catalog, name, "high")
		if err != nil || route.ResolvedModel != "gpt-6-astra" {
			t.Fatal("missing model did not use the authorized available fallback")
		}
		unavailable := bpsRouteCatalogFixture(t)
		for _, entry := range unavailable.ModelCatalog.Models {
			id, _ := json.Marshal(entry.ID)
			unavailable.ModelCatalog.RestrictedModels = append(unavailable.ModelCatalog.RestrictedModels, id)
		}
		_, err = chooseBPSModelRoute(unavailable, name, "high")
		var routing *bpsModelRoutingError
		if !errors.As(err, &routing) || routing.Param != "model" {
			t.Fatal("request without any usable catalog model was not rejected")
		}
	}
	_, err := chooseBPSModelRoute(catalog, "gpt-6-astra", "invented-effort")
	var routing *bpsModelRoutingError
	if !errors.As(err, &routing) || routing.Param != "reasoning.effort" {
		t.Fatal("unknown effort was silently guessed")
	}
}

func TestBPSModelRoutingPreservesExactAndFutureAdvertisedValues(t *testing.T) {
	catalog, err := bps.ParseAccessCatalog([]byte(`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-5.5","efforts":[{"value":"provider-special"}],"default_effort":"provider-special"},{"id":"gpt-6-sol","efforts":[{"value":"high"}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	route, err := chooseBPSModelRoute(catalog, "gpt-5.5", "provider-special")
	if err != nil || route.Changed || route.ResolvedModel != "gpt-5.5" || route.ResolvedEffort != "provider-special" {
		t.Fatal("advertised exact model/effort changed")
	}
	fallback := bpsRouteCatalogFixture(t)
	fallback.ModelCatalog.Models = fallback.ModelCatalog.Models[2:]
	route, err = chooseBPSModelRoute(fallback, "gpt-5.5", "max")
	if err != nil || route.ResolvedModel != "gpt-6-sol" || route.ResolvedEffort != "high" {
		t.Fatal("nearest available fallback was not selected")
	}
	fallback.ModelCatalog.Models = fallback.ModelCatalog.Models[1:]
	for _, model := range []string{"gpt-5.5", "gpt-reserve", "codex-auto-review"} {
		route, err = chooseBPSModelRoute(fallback, model, "max")
		if err != nil || route.ResolvedModel != "gpt-5.6-luna" || route.ResolvedEffort != "medium" {
			t.Fatal("authorized downward model compatibility stopped before the remaining available model")
		}
	}
}

func TestBPSModelRoutingStaticMappingPreservesExactRequestBytes(t *testing.T) {
	var gets atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/responses/access" || r.URL.Query().Get("include_models") != "true" {
			t.Error("routing issued a non-catalog request")
		}
		gets.Add(1)
		_, _ = io.WriteString(w, bpsRouteFixtureCatalog)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer test-client-key")
	const original = `{"input" : [{ "type":"message", "role":"user", "content":"exact \\ bytes", "large":900719925474099312345 }], "model":"gpt-5.5", "reasoning_effort":"max"}`
	for i := 0; i < 2; i++ {
		prepared := &bps.Request{Body: []byte(original), Model: "gpt-5.5"}
		route, err := p.resolveBPSModelRoute(context.Background(), r, account, nil, "tenant-a", prepared, settings)
		if err != nil || !route.Changed || prepared.Model != "gpt-5.6-sol" {
			t.Fatalf("resolve failed: %v", err)
		}
		want := strings.Replace(strings.Replace(original, `"model":"gpt-5.5"`, `"model":"gpt-5.6-sol"`, 1), `"reasoning_effort":"max"`, `"reasoning_effort":"xhigh"`, 1)
		if string(prepared.Body) != want {
			t.Fatal("routing changed input, whitespace or raw numeric bytes")
		}
	}
	if gets.Load() != 0 {
		t.Fatal("static routing unexpectedly fetched a catalog")
	}
	for _, mutate := range []func(){
		func() { account.CredentialVersion++ },
		func() {
			p.BPS.modelCatalog.mu.Lock()
			for k, v := range p.BPS.modelCatalog.entries {
				v.expires = time.Now().Add(-time.Second)
				p.BPS.modelCatalog.entries[k] = v
			}
			p.BPS.modelCatalog.mu.Unlock()
		},
	} {
		mutate()
		prepared := &bps.Request{Body: []byte(original), Model: "gpt-5.5"}
		if _, err := p.resolveBPSModelRoute(context.Background(), r, account, nil, "tenant-a", prepared, settings); err != nil {
			t.Fatal(err)
		}
	}
	if gets.Load() != 0 {
		t.Fatal("credential/TTL change triggered a forbidden catalog probe")
	}
}

func TestBPSModelRoutingCanonicalPathUsesOnlySavedRules(t *testing.T) {
	var gets atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/responses/access" {
			t.Fatal("model resolver dispatched a generation")
		}
		gets.Add(1)
		_, _ = io.WriteString(w, bpsRouteFixtureCatalog)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
	request := &bps.Request{Body: []byte(`{"model":"gpt-6-sol","reasoning_effort":"medium","input":[]}`), Model: "gpt-6-sol"}
	original := bytes.Clone(request.Body)
	route, err := p.resolveBPSModelRoute(context.Background(), r, account, nil, "tenant", request, settings)
	if err != nil || route.CatalogChecked || route.ResolvedEffort != "medium" || gets.Load() != 0 {
		t.Fatal("canonical path queried catalog instead of static verified capabilities")
	}
	request.Body = bytes.Clone(original)
	route, err = p.resolveBPSModelRoute(context.Background(), r, account, nil, "tenant", request, settings)
	if err != nil || route.CatalogChecked || route.ResolvedEffort != "medium" || gets.Load() != 0 {
		t.Fatal("repeated canonical request changed or queried a catalog")
	}
}

func TestBPSModelRoutingCatalogSingleFlightAndCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var gets atomic.Int32
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if gets.Add(1) == 1 {
			close(entered)
		}
		<-release
		_, _ = io.WriteString(w, bpsRouteFixtureCatalog)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
	leader := make(chan error, 1)
	go func() {
		_, err := p.loadBPSModelCatalog(context.Background(), r, account, nil, "tenant", settings)
		leader <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("catalog fetch did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.loadBPSModelCatalog(ctx, r, account, nil, "tenant", settings); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled request waited for shared fetch")
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			if _, err := p.loadBPSModelCatalog(context.Background(), r, account, nil, "tenant", settings); err != nil {
				t.Error(err)
			}
		})
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	group.Wait()
	if gets.Load() != 1 {
		t.Fatal("concurrent alias requests duplicated catalog fetch")
	}
}

func TestBPSModelRoutingPrepareOptInAndCompactContract(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[],"reasoning":{"effort":"ultra"}}`)
	if _, err := bps.Prepare(body, bps.Options{TaskID: "task", TurnID: "turn"}); err == nil {
		t.Fatal("standalone prepare accepted unresolved effort")
	}
	prepared, err := bps.Prepare(body, bps.Options{TaskID: "task", TurnID: "turn", ResolveCapabilities: true, Compact: true})
	if err != nil {
		t.Fatal(err)
	}
	p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("seeded catalog should not fetch") })
	account, settings := bpsAttachmentFixtureAccount(t, p)
	p.rememberBPSModelCatalog(account, nil, "tenant", bpsRouteCatalogFixture(t))
	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses/compact", nil)
	route, err := p.resolveBPSModelRoute(context.Background(), r, account, nil, "tenant", prepared, settings)
	if err != nil || route.ResolvedModel != "gpt-5.6-sol" || route.ResolvedEffort != "xhigh" {
		t.Fatalf("compact alias routing failed: %v", err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(prepared.Body, &fields)
	if fields["reasoning_effort"] != nil || fields["stream"] != nil {
		t.Fatal("routing reintroduced forbidden compact fields")
	}
	if string(prepared.Policy.ClientControls["reasoning_effort"]) != `"xhigh"` {
		t.Fatal("compact effective reasoning diagnostics were lost")
	}
}

func TestBPSModelRoutingSkipsRestrictedModelsAndDoesNotCacheFailure(t *testing.T) {
	catalog := bpsRouteCatalogFixture(t)
	catalog.ModelCatalog.RestrictedModels = []json.RawMessage{json.RawMessage(`"gpt-5.6-sol"`)}
	route, err := chooseBPSModelRoute(catalog, "gpt-5.5", "max")
	if err != nil || route.ResolvedModel != "gpt-6-sol" {
		t.Fatal("routing selected an explicitly restricted model")
	}
	for _, tc := range []struct {
		status   int
		body     string
		expected int
	}{
		{403, `{"error":"private detail"}`, 403},
		{200, `{"allowed":false}`, 403},
		{200, `{"allowed":true,"model_catalog":{"models":[]}}`, 502},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var gets atomic.Int32
			p := bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
				gets.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			account, settings := bpsAttachmentFixtureAccount(t, p)
			r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
			request := &bps.Request{Model: "gpt-5.5", Body: []byte(`{"model":"gpt-5.5","reasoning_effort":"max","input":[]}`)}
			original := bytes.Clone(request.Body)
			_, err := p.loadBPSModelCatalog(context.Background(), r, account, nil, "tenant", settings)
			var rejected *bpsModelRoutingError
			if !errors.As(err, &rejected) || rejected.StatusCode != tc.expected || strings.Contains(err.Error(), "private detail") {
				t.Fatal("catalog failure was misclassified or leaked response")
			}
			if gets.Load() != 1 || len(p.BPS.modelCatalog.entries) != 0 || !bytes.Equal(original, request.Body) {
				t.Fatal("catalog failure was retried, cached or partially rewrote request")
			}
		})
	}
}

func TestBPSModelRoutingCatalogScopeAndOwnerReplacement(t *testing.T) {
	var p *Proxy
	p = bpsGatewayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := p.Store.UpdateAccount("test", func(account *Account) error { account.AccountID = "replaced"; return nil }); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, bpsRouteFixtureCatalog)
	})
	account, settings := bpsAttachmentFixtureAccount(t, p)
	r := httptest.NewRequest(http.MethodPost, "http://gateway/v1/responses", nil)
	_, err := p.loadBPSModelCatalog(context.Background(), r, account, nil, "tenant", settings)
	var owner *gatewayOwnerChangedError
	if !errors.As(err, &owner) || len(p.BPS.modelCatalog.entries) != 0 {
		t.Fatal("old owner catalog was committed after replacement")
	}
	base := bpsModelCatalogCacheKey(account, nil, "tenant", "https://example.invalid")
	for _, key := range []string{
		bpsModelCatalogCacheKey(account, nil, "another", "https://example.invalid"),
		bpsModelCatalogCacheKey(account, nil, "tenant", "https://another.invalid"),
		bpsModelCatalogCacheKey(account, &ProxyEntry{ID: "another"}, "tenant", "https://example.invalid"),
	} {
		if key == base {
			t.Fatal("catalog cache scope leaked")
		}
	}
}
