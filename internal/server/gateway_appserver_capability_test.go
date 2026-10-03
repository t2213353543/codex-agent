package server

import "testing"

func TestAppServerCLICapabilityManifestDoesNotInventHostedRuntime(t *testing.T) {
	manifest := (&appServerCLI{}).capabilityManifest()
	if manifest["hosted_runtime"] != false || manifest["client_owned_tools"] != true || manifest["client_owned_history"] != true {
		t.Fatalf("wrong ownership manifest: %#v", manifest)
	}
	if manifest["apps_directory"] != "app_server_executor_only" {
		t.Fatalf("connector discovery ownership was not explicit: %#v", manifest)
	}
	conditional, ok := manifest["conditional_unverified"].([]string)
	if !ok || len(conditional) < 5 {
		t.Fatalf("conditional gaps missing: %#v", manifest["conditional_unverified"])
	}
}
