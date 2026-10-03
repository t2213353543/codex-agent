package server

import (
	"net/http"
	"strings"
)

const appServerClientOriginator = "codex_agent_gateway"
const appServerIdentityEnv = "CODEX_APP_SERVER_IDENTITY_FILE"

// Load the offline initialize response of the pinned executable for the same
// clientInfo used by gateway_official_runtime. No process is started here and
// no OS/terminal identity is manufactured from another reference profile.
func (a *appServerCLI) configureIdentity(getenv func(string) string) error {
	path := getenv(appServerIdentityEnv)
	if path == "" {
		return nil
	}
	identity, err := loadGatewayRuntimeIdentity(path, appServerClientOriginator)
	if err != nil {
		return err
	}
	a.identity = identity
	return nil
}

func (a *appServerCLI) applyIdentity(headers http.Header) {
	if a == nil || a.identity == nil {
		return
	}
	for name := range headers {
		if strings.EqualFold(name, "User-Agent") || strings.EqualFold(name, "Originator") || strings.EqualFold(name, "Version") || strings.EqualFold(name, "X-Oai-Attestation") {
			delete(headers, name)
		}
	}
	headers.Set("User-Agent", a.identity.UserAgent)
	headers.Set("Originator", a.identity.Originator)
	headers.Set("Version", a.identity.CLIVersion)
}
