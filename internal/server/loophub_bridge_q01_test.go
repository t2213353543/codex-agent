package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLoophubBridgeQ01RealManagementContract(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python is required for the cross-language loopback contract")
	}
	for _, scenario := range []string{"active_current_expiry", "fresh_401", "stale_401", "capacity_429", "expired_current_at"} {
		t.Run(scenario, func(t *testing.T) {
			p, handler := webTestSetup(t)
			group, err := p.Groups.Create("q01-contract-group", "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(time.Hour).Unix()
			if scenario == "expired_current_at" {
				expiry = time.Now().Add(-time.Minute).Unix()
			}
			raw, _ := json.Marshal(map[string]any{"access_token": "q01-synthetic-at", "account_id": "synthetic-workspace", "expires_at": expiry})
			if err := p.Store.Import("q01-contract", raw); err != nil {
				t.Fatal(err)
			}
			a, err := p.Store.Get("q01-contract")
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte("synthetic-workspace"))
			a, err = p.Store.UpdateAccountVersion(a.ID, a.Version, func(account *Account) error {
				account.Tags = []string{"loophub-28765-" + hex.EncodeToString(digest[:])[:16]}
				account.Groups = []string{group.ID}
				account.Enabled = true
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "fresh_401" || scenario == "stale_401" || scenario == "capacity_429" {
				code := 401
				if scenario == "capacity_429" {
					code = 429
				}
				// Populate the same independent store as a completed management
				// check, without issuing any model, quota, or OAuth request.
				snapshot := managedUpstreamSnapshot{ID: a.ID, Version: a.Version,
					CredentialVersion: managedAccountCredentialVersion(p, a), Kind: "stream", HTTPCode: code,
					CheckedAt: time.Now().Unix(), Source: "upstream",
					Error: &managedUpstreamError{Code: "upstream_http_error", Message: "synthetic rejection"}}
				if scenario == "stale_401" {
					snapshot.CredentialVersion--
				}
				if err := p.cacheManagedUpstream(context.Background(), snapshot); err != nil {
					t.Fatal(err)
				}
			}
			expected := map[string]any{"status": "active", "needs_reauth": false, "status_message": ""}
			switch scenario {
			case "fresh_401":
				expected = map[string]any{"status": "error", "needs_reauth": true, "status_message": "upstream_http_401"}
			case "capacity_429":
				// Management checks do not affect scheduling/summary status.
				// A 429 never becomes an authentication-repair instruction.
			case "expired_current_at":
				expected = map[string]any{"status": "error", "needs_reauth": true, "status_message": "token_expired"}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || (r.URL.Path != "/api/accounts/"+a.ID && r.URL.Path != "/api/accounts/"+a.ID+"/upstream") {
					t.Errorf("bridge made a non-read-only request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			params, _ := json.Marshal(map[string]any{"base": server.URL, "key": webTestKey,
				"account_id": a.ID, "group_id": group.ID, "expected": expected})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, python, filepath.Join(projectRoot(t), "tools", "loophub_bridge_contract_q01.py"))
			command.Stdin = bytes.NewReader(params)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("real Go API/Python bridge contract failed: %v\n%s", err, output)
			}
			var result struct {
				OK     bool `json:"ok"`
				Checks int  `json:"checks"`
			}
			if json.Unmarshal(output, &result) != nil || !result.OK || result.Checks != len(expected) {
				t.Fatalf("invalid contract result: %s", output)
			}
		})
	}
}
