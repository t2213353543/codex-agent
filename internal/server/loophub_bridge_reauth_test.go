package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLoophubBridgeReauthorizationUpdatesRealDestinationAndStatus(t *testing.T) {
	runLoophubBridgeReauthorization(t, webTestSetup)
}

func TestPGLoophubBridgeReauthorizationUpdatesRealDestinationAndStatus(t *testing.T) {
	runLoophubBridgeReauthorization(t, adminPGTestProxy)
}

func runLoophubBridgeReauthorization(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	python, err := exec.LookPath("python")
	if err != nil {
		python, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python required for the loopback contract")
	}
	p, handler := setup(t)
	group, err := p.Groups.Create("reauth-contract", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture/reject" && r.URL.Path != "/fixture/replace" && r.URL.Path != "/fixture/delete" {
			handler.ServeHTTP(w, r)
			return
		}
		var in struct {
			ID          string `json:"id"`
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture", 400)
			return
		}
		a, err := p.Store.Get(in.ID)
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture account", 500)
			return
		}
		if r.URL.Path == "/fixture/reject" {
			p.Metrics.Record(RequestRecord{Time: time.Now().Unix(), Account: a.Name, AccountID: a.ID, AccountCredentialVersion: a.CredentialVersion,
				Status: "error", HTTPCode: 401, Error: "upstream returned HTTP 401"})
			err = p.cacheManagedUpstream(r.Context(), managedUpstreamSnapshot{ID: a.ID, Version: a.Version, CredentialVersion: a.CredentialVersion,
				Kind: "usage", CheckedAt: time.Now().Unix(), HTTPCode: 401, Source: "upstream", Error: &managedUpstreamError{Code: "upstream_http_error", Message: "synthetic rejection"}})
		} else if r.URL.Path == "/fixture/delete" {
			err = p.Store.DeleteVersion(a.ID, a.Version)
		} else {
			incoming := a
			incoming.AccessToken = in.AccessToken
			_, err = p.Store.reauthorizeManagedAccount(r.Context(), a.ID, a.Version, incoming, false)
		}
		if err != nil {
			t.Error(err)
			http.Error(w, "fixture failed", 500)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
	defer server.Close()
	params, _ := json.Marshal(map[string]any{"base": server.URL, "key": webTestKey, "group_id": group.ID})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, filepath.Join(projectRoot(t), "tools", "loophub_bridge_reauth_contract.py"))
	command.Stdin = bytes.NewReader(params)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real bridge reauthorization failed: %v\n%s", err, output)
	}
	var result struct {
		OK     bool `json:"ok"`
		Checks int  `json:"checks"`
	}
	if json.Unmarshal(output, &result) != nil || !result.OK || result.Checks != 14 {
		t.Fatalf("invalid bridge receipt: %s", output)
	}
}
