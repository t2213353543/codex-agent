package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

const gatewayRuntimeIdentityEnv = "CODEX_GATEWAY_IDENTITY_FILE"
const gatewayRuntimeIdentityLimit = 16 << 10
const gatewayRuntimeCLILimit = 512 << 20

// This snapshot comes from an operator's local, offline initialize capture of
// the pinned official CLI. The gateway validates its source binary and platform
// at startup; it does not synthesize an OS/terminal or run a CLI per request.
// The capture is provenance supplied by the operator, not cryptographic proof
// that an arbitrary User-Agent was produced by that binary.
type gatewayRuntimeIdentity struct {
	Schema          string `json:"schema"`
	CLIPath         string `json:"cli_path"`
	SourceCLISHA256 string `json:"source_cli_sha256"`
	CLIVersion      string `json:"cli_version"`
	Originator      string `json:"originator"`
	UserAgent       string `json:"user_agent"`
	PlatformOS      string `json:"platform_os"`
	PlatformArch    string `json:"platform_arch"`
}

var configuredGatewayRuntimeIdentity atomic.Pointer[gatewayRuntimeIdentity]

func gatewayDefaultApplicationIdentity() (userAgent, originator string) {
	if identity := configuredGatewayRuntimeIdentity.Load(); identity != nil {
		return identity.UserAgent, identity.Originator
	}
	return clientOriginator + "/" + clientVersion, clientOriginator
}

func gatewayRuntimeIdentityError(reason string) error {
	return fmt.Errorf("invalid %s: %s", gatewayRuntimeIdentityEnv, reason)
}

func configureGatewayRuntimeIdentity(getenv func(string) string) error {
	path := getenv(gatewayRuntimeIdentityEnv)
	if path == "" {
		configuredGatewayRuntimeIdentity.Store(nil)
		return nil
	}
	identity, err := loadGatewayRuntimeIdentity(path, clientOriginator)
	if err != nil {
		return err
	}
	configuredGatewayRuntimeIdentity.Store(identity)
	return nil
}

func loadGatewayRuntimeIdentity(path, expectedOriginator string) (*gatewayRuntimeIdentity, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
		return nil, gatewayRuntimeIdentityError("configuration path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, gatewayRuntimeIdentityError("cannot open configuration")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > gatewayRuntimeIdentityLimit {
		return nil, gatewayRuntimeIdentityError("configuration must be a bounded regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, gatewayRuntimeIdentityLimit+1))
	if err != nil || len(raw) > gatewayRuntimeIdentityLimit {
		return nil, gatewayRuntimeIdentityError("cannot read bounded configuration")
	}
	identity, err := parseGatewayRuntimeIdentityForOriginator(raw, expectedOriginator)
	if err != nil {
		return nil, err
	}
	if err := verifyGatewayRuntimeIdentitySource(identity); err != nil {
		return nil, err
	}
	// All validation precedes publication. Readers observe one complete pair,
	// and changing the file later cannot silently change a live connection.
	return identity, nil
}

func parseGatewayRuntimeIdentity(raw []byte) (*gatewayRuntimeIdentity, error) {
	return parseGatewayRuntimeIdentityForOriginator(raw, clientOriginator)
}

func parseGatewayRuntimeIdentityForOriginator(raw []byte, expectedOriginator string) (*gatewayRuntimeIdentity, error) {
	if len(raw) == 0 || len(raw) > gatewayRuntimeIdentityLimit {
		return nil, gatewayRuntimeIdentityError("configuration size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, gatewayRuntimeIdentityError("configuration must be one JSON object")
	}
	fields := make(map[string]string, 8)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, gatewayRuntimeIdentityError("invalid JSON field")
		}
		switch name {
		case "schema", "cli_path", "source_cli_sha256", "cli_version", "originator", "user_agent", "platform_os", "platform_arch":
		default:
			return nil, gatewayRuntimeIdentityError("unknown JSON field")
		}
		if _, exists := fields[name]; exists {
			return nil, gatewayRuntimeIdentityError("duplicate JSON field")
		}
		var value string
		if decoder.Decode(&value) != nil || value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, gatewayRuntimeIdentityError("fields must contain nonempty single-line strings")
		}
		fields[name] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, gatewayRuntimeIdentityError("invalid JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) || len(fields) != 8 {
		return nil, gatewayRuntimeIdentityError("missing fields or trailing JSON")
	}
	identity := &gatewayRuntimeIdentity{
		Schema: fields["schema"], CLIPath: fields["cli_path"], SourceCLISHA256: fields["source_cli_sha256"],
		CLIVersion: fields["cli_version"], Originator: fields["originator"], UserAgent: fields["user_agent"],
		PlatformOS: fields["platform_os"], PlatformArch: fields["platform_arch"],
	}
	if identity.Schema != "codex-runtime-identity-v1" || identity.CLIVersion != clientVersion || identity.Originator != expectedOriginator {
		return nil, gatewayRuntimeIdentityError("schema, CLI version or originator does not match the pinned baseline")
	}
	if identity.PlatformOS != runtime.GOOS || identity.PlatformArch != runtime.GOARCH {
		return nil, gatewayRuntimeIdentityError("capture platform differs from the gateway runtime")
	}
	if !filepath.IsAbs(identity.CLIPath) {
		return nil, gatewayRuntimeIdentityError("CLI path must be absolute")
	}
	digest, err := hex.DecodeString(identity.SourceCLISHA256)
	if err != nil || len(digest) != sha256.Size || identity.SourceCLISHA256 != strings.ToLower(identity.SourceCLISHA256) {
		return nil, gatewayRuntimeIdentityError("CLI SHA256 is invalid")
	}
	prefix := identity.Originator + "/" + identity.CLIVersion + " ("
	if !strings.HasPrefix(identity.UserAgent, prefix) || len(identity.UserAgent) > 1024 {
		return nil, gatewayRuntimeIdentityError("User-Agent does not match the pinned CLI identity")
	}
	platform := strings.TrimPrefix(identity.UserAgent, prefix)
	osEnd := strings.Index(platform, ") ")
	if osEnd < 1 || !strings.Contains(platform[:osEnd], "; ") || strings.TrimSpace(platform[osEnd+2:]) == "" {
		return nil, gatewayRuntimeIdentityError("User-Agent lacks CLI platform or terminal identity")
	}
	for _, value := range identity.UserAgent {
		if value < ' ' || value > '~' {
			return nil, gatewayRuntimeIdentityError("User-Agent must contain printable ASCII only")
		}
	}
	return identity, nil
}

func verifyGatewayRuntimeIdentitySource(identity *gatewayRuntimeIdentity) error {
	file, err := os.Open(identity.CLIPath)
	if err != nil {
		return gatewayRuntimeIdentityError("cannot open captured CLI binary")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > gatewayRuntimeCLILimit {
		return gatewayRuntimeIdentityError("CLI binary must be a bounded regular file")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(file, gatewayRuntimeCLILimit+1))
	if err != nil || n > gatewayRuntimeCLILimit || hex.EncodeToString(digest.Sum(nil)) != identity.SourceCLISHA256 {
		return gatewayRuntimeIdentityError("captured CLI binary SHA256 mismatch")
	}
	return nil
}
