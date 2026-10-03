package server

import (
	"net/http"
	"testing"
)

func TestAppServerBackendObservedSurfaceAllowlist(t *testing.T) {
	for _, path := range []string{
		"/backend-api/wham/settings/user", "/backend-api/wham/config/bundle", "/backend-api/wham/accounts/check",
		"/backend-api/wham/profiles/me", "/backend-api/wham/usage", "/backend-api/wham/rate-limit-reset-credits",
		"/backend-api/wham/accounts/send_add_credits_nudge_email",
		"/backend-api/wham/rate-limit-reset-credits/consume", "/backend-api/wham/tasks", "/backend-api/wham/tasks/list",
		"/backend-api/wham/tasks/task-1", "/backend-api/wham/tasks/task-1/turns/turn-1/sibling_turns",
		"/backend-api/wham/usage/thread_usage/query", "/backend-api/wham/workspace-messages",
		"/backend-api/plugins/featured", "/backend-api/plugins/export/curated", "/backend-api/ps/plugins/search",
		"/backend-api/ps/plugins/plugin-1", "/backend-api/ps/plugins/plugin-1/install", "/backend-api/ps/plugins/plugin-1/shares",
		"/backend-api/ps/plugins/workspace/created", "/backend-api/public/plugins/workspace/plugin-1",
		"/backend-api/public/plugins/workspace", "/backend-api/public/plugins/workspace/upload-url",
	} {
		if !appServerBackendPath(path) {
			t.Fatalf("observed App Server path not allowlisted: %s", path)
		}
	}
	if ok, method := appServerBackendDynamicPath("/backend-api/wham/tasks/task-1"); !ok || method != http.MethodGet {
		t.Fatal("task detail method mismatch")
	}
	if ok, method := appServerBackendDynamicPath("/backend-api/ps/plugins/p1/install"); !ok || method != http.MethodPost {
		t.Fatal("plugin install method mismatch")
	}
	if method, ok := appServerBackendAllowedMethod("/backend-api/wham/accounts/send_add_credits_nudge_email"); !ok || method != http.MethodPost {
		t.Fatal("credits nudge method mismatch")
	}
	if method, ok := appServerBackendAllowedMethod("/backend-api/public/plugins/workspace/p1"); !ok || method != http.MethodPost {
		t.Fatal("workspace publish method mismatch")
	}
	if alias := appServerModelAlias("/backend-api/codex/realtime/calls"); alias != "/v1/realtime/calls" {
		t.Fatalf("realtime backend alias mismatch: %q", alias)
	}
	for _, path := range []string{"/backend-api/wham/tasks/../secret", "/backend-api/ps/plugins/a/b/c", "/backend-api/ps/plugins/https:%2f%2fevil"} {
		if appServerBackendPath(path) {
			t.Fatalf("path escape accepted: %s", path)
		}
	}
}
