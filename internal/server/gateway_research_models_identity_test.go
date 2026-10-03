package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

type researchModelsRequest struct {
	path, owner, validator, version, session string
}

func researchModelsFixture(t *testing.T, native bool) (*Proxy, string, <-chan researchModelsRequest) {
	t.Helper()
	observed := make(chan researchModelsRequest, 8)
	p := proxyFixture(t, func(w http.ResponseWriter, r *http.Request) {
		owner := "original"
		if r.Header.Get("Authorization") == "Bearer synthetic-models-alternate" {
			owner = "alternate"
		}
		version := r.URL.Query().Get("client_version")
		observed <- researchModelsRequest{r.URL.Path, owner, r.Header.Get("If-None-Match"), version, r.Header.Get("Session-Id")}
		if r.URL.Path == "/responses" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Models-Etag", `"catalog-`+owner+`-`+clientVersion+`"`)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"models-thread-response\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		etag := `"catalog-` + owner + `-` + version + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{
			"slug": "fixture-" + owner, "display_name": "Fixture " + owner,
			"base_instructions": "synthetic " + owner + " instructions", "default_service_tier": owner,
		}}, "fixture_client_version": version})
	})
	if err := p.Store.ImportWith("models-alternate", []byte(`{"access_token":"synthetic-models-alternate","chatgpt_account_id":"models-workspace-b","expires_at":4102444800}`), AccountDefaults{Priority: -10, Concurrency: 2}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("test", func(a *Account) error {
		a.Priority, a.Groups = 100, []string{"models-fixture-group"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	original, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.UpdateAccount("models-alternate", func(a *Account) error { a.Groups = append([]string(nil), original.Groups...); return nil }); err != nil {
		t.Fatal(err)
	}
	if native {
		realGatewayNativePool(t, p)
	}
	gateway := httptest.NewServer(p)
	t.Cleanup(gateway.Close)
	return p, gateway.URL, observed
}

func researchModelsSend(t *testing.T, target, path, session, validator string, credentials ...string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	method, body := http.MethodGet, ""
	if strings.HasPrefix(path, "/responses") {
		method, body = http.MethodPost, `{"model":"fixture","stream":true,"input":[]}`
	}
	req, err := http.NewRequestWithContext(ctx, method, target+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	credential := "test-client-key"
	if len(credentials) > 0 {
		credential = credentials[0]
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if session != "" {
		req.Header.Set("Session-Id", session)
	}
	if validator != "" {
		req.Header.Set("If-None-Match", validator)
	}
	res, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return res.StatusCode, res.Header, raw
}

func researchModelsPreferAlternate(t *testing.T, p *Proxy) {
	t.Helper()
	for name, priority := range map[string]int{"test": -10, "models-alternate": 100} {
		if err := p.Store.UpdateAccount(name, func(a *Account) error { a.Priority = priority; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	p.invalidateAccounts()
}

func TestResearchModelsCatalogRespectsEstablishedExplicitSession(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			if native && (runtime.GOOS != "linux" || os.Getenv("CODEX_NATIVE_HTTP_TEST_BIN") == "") {
				t.Skip("Linux and explicit real Rust helper required")
			}
			for _, conditional := range []bool{false, true} {
				t.Run(fmt.Sprintf("conditional=%v", conditional), func(t *testing.T) {
					p, target, observed := researchModelsFixture(t, native)
					const session = "established-models-thread"
					if status, _, _ := researchModelsSend(t, target, "/responses", session, ""); status != 200 {
						t.Fatal("could not establish inference owner")
					}
					if first := <-observed; first.owner != "original" {
						t.Fatal("priority fixture selected unexpected first owner")
					}
					status, headers, _ := researchModelsSend(t, target, "/models?client_version="+clientVersion, session, "")
					if status != 200 || headers.Get("ETag") == "" || (<-observed).owner != "original" {
						t.Fatal("first model catalog did not come from inference owner")
					}
					researchModelsPreferAlternate(t, p)
					validator := ""
					if conditional {
						validator = headers.Get("ETag")
					}
					status, _, body := researchModelsSend(t, target, "/models?client_version="+clientVersion, session, validator)
					actual := <-observed
					wantStatus := 200
					if conditional {
						wantStatus = http.StatusNotModified
					}
					if actual.owner != "original" || status != wantStatus || strings.Contains(string(body), "fixture-alternate") {
						t.Errorf("explicit bound thread refreshed another account's catalog: owner=%s status=%d want=%d", actual.owner, status, wantStatus)
					}
					// The same explicit thread still performs inference using A;
					// this isolates catalog/response inconsistency from session loss.
					if status, _, _ := researchModelsSend(t, target, "/responses", session, ""); status != 200 || (<-observed).owner != "original" {
						t.Fatal("established inference session changed owner")
					}
				})
			}
		})
	}
}

func TestResearchModelsUnboundAccountAndVersionETagControls(t *testing.T) {
	p, target, observed := researchModelsFixture(t, false)
	status, header, original := researchModelsSend(t, target, "/models?client_version="+clientVersion, "", "")
	if status != 200 || (<-observed).owner != "original" {
		t.Fatal("first independent catalog failed")
	}
	etag := header.Get("ETag")
	status, header, body := researchModelsSend(t, target, "/models?client_version="+clientVersion, "", etag)
	if status != http.StatusNotModified || len(body) != 0 || header.Get("ETag") != etag || (<-observed).validator != etag {
		t.Fatal("unchanged owner/catalog conditional request did not preserve 304")
	}
	researchModelsPreferAlternate(t, p)
	status, header, body = researchModelsSend(t, target, "/models?client_version="+clientVersion, "", etag)
	if status != 200 || header.Get("ETag") == etag || string(body) == string(original) || (<-observed).owner != "alternate" {
		t.Fatal("different upstream catalog was incorrectly reused as 304")
	}
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.CodexFingerprintEnabled = false
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	prior := header.Get("ETag")
	status, header, _ = researchModelsSend(t, target, "/models?client_version=0.154.0", "", prior)
	if status != 200 || header.Get("ETag") == prior || (<-observed).version != "0.154.0" {
		t.Fatal("different client-version representation incorrectly reused prior validator")
	}
	prior = header.Get("ETag")
	settings, err = p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.CodexFingerprintEnabled = true
	if err := p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	status, header, _ = researchModelsSend(t, target, "/models?client_version=0.154.0", "", prior)
	if status != 200 || header.Get("ETag") == prior || (<-observed).version != clientVersion {
		t.Fatal("profile version override and upstream validator became incoherent")
	}
	p.sessionMu.Lock()
	bindings := len(p.sessionRoutes)
	p.sessionMu.Unlock()
	if bindings != 0 {
		t.Fatal("unbound model discovery unexpectedly established inference affinity")
	}
}

func TestResearchModelsExplicitSessionOnlyUsesExistingEnabledAffinity(t *testing.T) {
	for _, scenario := range []string{"unbound_header", "sticky_disabled", "bound_disabled", "other_key_same_header"} {
		t.Run(scenario, func(t *testing.T) {
			p, target, observed := researchModelsFixture(t, false)
			const session = "models-explicit-session-boundary"
			if scenario != "unbound_header" {
				status, _, _ := researchModelsSend(t, target, "/responses", session, "")
				if status != 200 || (<-observed).owner != "original" {
					t.Fatal("could not establish control inference binding")
				}
			} else {
				status, _, _ := researchModelsSend(t, target, "/models", session, "")
				if status != 200 || (<-observed).owner != "original" {
					t.Fatal("initial unbound catalog request failed")
				}
				if binding, err := p.sessionAccount(t.Context(), "admin", session, ""); err != nil || binding != "" {
					t.Fatal("unbound catalog GET established an inference binding")
				}
			}
			researchModelsPreferAlternate(t, p)
			credential, keyID := "test-client-key", "admin"
			switch scenario {
			case "sticky_disabled":
				settings, err := p.Settings.Get()
				if err != nil {
					t.Fatal(err)
				}
				settings.StickySessionsEnabled = false
				if err := p.Settings.Set(settings); err != nil {
					t.Fatal(err)
				}
			case "bound_disabled":
				if err := p.Store.UpdateAccount("test", func(a *Account) error { a.Enabled = false; return nil }); err != nil {
					t.Fatal(err)
				}
				p.invalidateAccounts()
			case "other_key_same_header":
				account, err := p.Store.Get("test")
				if err != nil {
					t.Fatal(err)
				}
				key, err := p.Keys.Create("models-other-key", account.Groups)
				if err != nil {
					t.Fatal(err)
				}
				credential, keyID = key.Key, key.ID
			}
			status, _, _ := researchModelsSend(t, target, "/models", session, "", credential)
			if scenario == "bound_disabled" {
				if status < 400 || len(observed) != 0 {
					t.Fatal("unavailable bound catalog owner silently switched to an alternate account")
				}
			} else {
				if status != 200 || (<-observed).owner != "alternate" {
					t.Fatal("unbound/disabled/different-key catalog incorrectly inherited another owner")
				}
			}
			if scenario == "unbound_header" || scenario == "other_key_same_header" {
				if binding, err := p.sessionAccount(t.Context(), keyID, session, ""); err != nil || binding != "" {
					t.Fatal("catalog discovery created a new binding or reused another key namespace")
				}
			}
		})
	}
}

func TestResearchModelsBindingStoreFailureCannotFallBack(t *testing.T) {
	p := newTestProxy(t, 1)
	settings, err := p.Settings.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings.StickySessionsEnabled = true
	// Exercise the catalog selection boundary directly: no authenticated
	// ingress/PG user-store fixture is implied by this missing Redis handle.
	p.Store.DB = &Database{}
	r := proxyRequest(http.MethodGet, "/models", "")
	r.Header.Set("Session-Id", "existing-or-unreadable-thread")
	rec := &RequestRecord{KeyID: "admin"}
	w := httptest.NewRecorder()
	p.forwardModels(w, r, rec, settings)
	if w.Code < 400 || rec.Status != "error" || rec.ErrorPhase != "account_selection" || !strings.Contains(rec.Error, "session routing unavailable") {
		t.Fatal("missing shared binding storage did not fail closed before catalog account selection")
	}
}
