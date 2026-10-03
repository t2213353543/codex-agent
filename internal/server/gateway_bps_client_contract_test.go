package server

import (
	"net/http"
	"testing"
)

func TestBPSClientHeadersPreserveDeployedPlatform(t *testing.T) {
	h := make(http.Header)
	applyBPSHeaders(h, Account{AccessToken: "fixture", AccountID: "fixture-workspace"})
	// Cache/usage releases preserve the deployed gateway profile. A prior
	// independent Office-host experiment did not establish a protocol fix.
	if h.Get("X-Openai-Internal-Basispoints-Client-Platform-Class") != "web" || h.Get("X-Openai-Internal-Basispoints-Client-Runtime") != "web" {
		t.Fatal("release changed the deployed platform profile")
	}
	if h.Get("Authorization") != "Bearer fixture" || h.Get("ChatGPT-Account-Id") != "fixture-workspace" {
		t.Fatal("host profile changed authentication")
	}
}
