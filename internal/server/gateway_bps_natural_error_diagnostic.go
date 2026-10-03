package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-agent/internal/publicerrors"
)

const naturalFailureDiagnosticLimit = 32

var naturalFailureDiagnosticMu sync.Mutex
var naturalFailureDiagnosticRecent = make(map[string]int64)

type naturalFailureDiagnostic struct {
	Schema        int    `json:"schema"`
	CapturedAt    int64  `json:"captured_at"`
	AccountSHA256 string `json:"account_sha256"`
	EventType     string `json:"event_type"`
	Status        string `json:"status,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	ErrorType     string `json:"error_type,omitempty"`
	ErrorParam    string `json:"error_param,omitempty"`
	ErrorStatus   string `json:"error_status,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	HTTPProtocol  string `json:"http_protocol,omitempty"`
	RetryAfter    *int64 `json:"retry_after_seconds,omitempty"`
	RetryAt       string `json:"retry_after_at,omitempty"`
}

func (p *Proxy) naturalFailureDirectory() string {
	if p == nil || os.Getenv("CODEX_NATURAL_FAILURE_DIAGNOSTICS") != "1" {
		return ""
	}
	if p.Store.Path != "" {
		return filepath.Join(filepath.Dir(p.Store.Path), "diagnostics")
	}
	if state := os.Getenv("STATE_DIRECTORY"); filepath.IsAbs(state) {
		return filepath.Join(state, "diagnostics")
	}
	return ""
}

// This opt-in diagnostic neither consumes/replays a response nor changes its
// classification. Only a small projection of a failure is retained, including
// codes not currently recognized by the normal metrics classifier.
func (p *Proxy) captureBPSNaturalFailure(account Account, event []byte, extraSecrets ...string) {
	directory := p.naturalFailureDirectory()
	if directory == "" {
		return
	}
	record, ok := projectNaturalFailureDiagnostic(account, event, time.Now(), extraSecrets...)
	if !ok {
		return
	}
	// Diagnostics are best effort. Storage errors must not change the user's
	// original upstream status, terminal event, cancellation or retry policy.
	_ = storeNaturalFailureDiagnostic(directory, record)
}

// Header-level errors never enter the terminal-event observer. Reuse the
// already-consumed bounded error prefix: no second read, retry or response
// rewrite. Keep only structured error fields and a parsed Retry-After value;
// arbitrary response headers and unstructured HTML/text are not retained.
func (p *Proxy) captureBPSNaturalHTTPFailure(response *http.Response, prefix []byte) {
	directory := p.naturalFailureDirectory()
	if directory == "" || response == nil || response.Request == nil || response.StatusCode < 400 {
		return
	}
	account, ok := response.Request.Context().Value(gatewayCooldownAccountContextKey{}).(Account)
	if !ok || account.ID == "" {
		return
	}
	fields, _ := gatewayWSParseFields(prefix)
	var detail map[string]json.RawMessage
	for _, value := range []json.RawMessage{fields["error"], fields["detail"]} {
		if candidate, err := gatewayWSParseFields(value); err == nil {
			detail = candidate
			break
		}
	}
	if detail == nil {
		detail = fields
	}
	selected := make(map[string]json.RawMessage)
	for _, name := range []string{"code", "type", "param", "status", "message"} {
		if value := detail[name]; len(value) > 0 {
			selected[name] = value
		}
	}
	event, err := json.Marshal(map[string]any{"type": "error", "error": selected})
	if err != nil {
		return
	}
	record, ok := projectNaturalFailureDiagnostic(account, event, time.Now(), bearerToken(response.Request))
	if !ok {
		return
	}
	record.EventType, record.HTTPStatus = "http.error", response.StatusCode
	if response.Proto == "HTTP/1.1" || response.Proto == "HTTP/2.0" {
		record.HTTPProtocol = response.Proto
	}
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		record.RetryAfter = &seconds
	} else if at, err := http.ParseTime(value); err == nil {
		record.RetryAt = at.UTC().Format(time.RFC3339)
	}
	_ = storeNaturalFailureDiagnostic(directory, record)
}

func projectNaturalFailureDiagnostic(account Account, event []byte, now time.Time, extraSecrets ...string) (naturalFailureDiagnostic, bool) {
	var record naturalFailureDiagnostic
	if account.ID == "" {
		return record, false
	}
	fields, err := gatewayWSParseFields(event)
	if err != nil {
		return record, false
	}
	text := func(raw json.RawMessage) string {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
		var status int
		if json.Unmarshal(raw, &status) == nil && status >= 100 && status <= 599 {
			return fmt.Sprint(status)
		}
		return ""
	}
	kind := text(fields["type"])
	switch kind {
	case "error", "response.failed", "response.incomplete", "response.cancelled", "basispoints.response.cancelled":
	default:
		return record, false
	}
	response, _ := gatewayWSParseFields(fields["response"])
	// Same error-object precedence as the BPS terminal parser. Null or absent
	// event.error permits response.error; an existing empty object does not.
	detail, err := gatewayWSParseFields(fields["error"])
	if err != nil {
		detail, err = gatewayWSParseFields(response["error"])
	}
	if err != nil && kind == "error" {
		detail = fields
	}
	code := text(detail["code"])
	secrets := []string{account.AccessToken, account.RefreshToken, account.AccountID, account.ID, account.Name}
	secrets = append(secrets, extraSecrets...)
	if account.Identity != nil {
		secrets = append(secrets, account.Identity.UserID, account.Identity.WorkspaceID)
	}
	clean := func(value string) string {
		for _, secret := range secrets {
			if secret != "" {
				value = strings.ReplaceAll(value, secret, "[REDACTED]")
			}
		}
		value = publicerrors.Message(redactMetricError(value))
		// Character count is bounded without cutting a UTF-8 sequence. This
		// also prevents error-code/type/param fields from becoming a body sink.
		runes := []rune(value)
		if len(runes) > 400 {
			value = string(runes[:400])
		}
		return value
	}
	digest := sha256.Sum256([]byte(account.ID))
	record = naturalFailureDiagnostic{Schema: 1, CapturedAt: now.Unix(), AccountSHA256: hex.EncodeToString(digest[:]),
		EventType: kind, Status: clean(text(response["status"])), ErrorCode: clean(code), ErrorType: clean(text(detail["type"])),
		ErrorParam: clean(text(detail["param"])), ErrorStatus: clean(text(detail["status"])), ErrorMessage: clean(text(detail["message"]))}
	return record, true
}

// Fixed filenames keep disk retention bounded across process restarts and
// overlapping runtime generations. Existing samples enforce the per-account
// minute budget without an unbounded in-memory account map.
func storeNaturalFailureDiagnostic(directory string, record naturalFailureDiagnostic) error {
	if !naturalFailureDiagnosticMu.TryLock() {
		return nil // Diagnostics never queue a request behind another sample.
	}
	defer naturalFailureDiagnosticMu.Unlock()
	key := directory + ":" + record.AccountSHA256
	if last, ok := naturalFailureDiagnosticRecent[key]; ok && record.CapturedAt-last < 60 {
		return nil
	}
	for key, last := range naturalFailureDiagnosticRecent {
		if record.CapturedAt-last >= 60 {
			delete(naturalFailureDiagnosticRecent, key)
		}
	}
	if len(naturalFailureDiagnosticRecent) >= naturalFailureDiagnosticLimit {
		return nil
	}
	// Reserve before touching disk, including storage errors or a busy sibling
	// process; a broken diagnostics directory must not cause an I/O storm.
	naturalFailureDiagnosticRecent[key] = record.CapturedAt
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("diagnostic directory unavailable")
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	// Runtime generations share this private directory. An exclusive sibling
	// lock also serializes the small rotation across processes. A stale lock
	// after a crash safely disables sampling until an operator removes it.
	lock := directory + ".natural-failure-lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	defer os.Remove(lock)
	slot, oldest := -1, int64(1<<63-1)
	for i := range naturalFailureDiagnosticLimit {
		name := filepath.Join(directory, fmt.Sprintf("natural-failure-%02d.json", i))
		info, err := os.Lstat(name)
		if os.IsNotExist(err) {
			if oldest != -1 {
				slot, oldest = i, -1
			}
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 16<<10 {
			return fmt.Errorf("diagnostic slot unavailable")
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		var previous naturalFailureDiagnostic
		if json.Unmarshal(raw, &previous) != nil {
			return fmt.Errorf("diagnostic slot invalid")
		}
		if previous.AccountSHA256 == record.AccountSHA256 && record.CapturedAt-previous.CapturedAt < 60 {
			return nil
		}
		if previous.CapturedAt < oldest {
			slot, oldest = i, previous.CapturedAt
		}
	}
	if slot < 0 {
		return fmt.Errorf("diagnostic slot unavailable")
	}
	if oldest >= 0 && record.CapturedAt-oldest < 60 {
		return nil // Never evict a still-active account sampling window.
	}
	name := filepath.Join(directory, fmt.Sprintf("natural-failure-%02d.json", slot))
	// The directory belongs to this service. Remove the previous owned regular
	// slot before exclusive create so no symlink or broad file mode is followed.
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(file).Encode(record)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
