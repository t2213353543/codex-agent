package server

import (
	"bytes"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var officialModelUnauthorizedStatus = regexp.MustCompile(`(?i)(?:\b401 unauthorized\b|\bstatus(?: code)?[:= ]+401\b)`)

// The official model manager consumes catalog HTTP failures instead of emitting
// external-auth requests. Keep only its explicit 401 classification; never log
// or persist stderr, response bodies, headers, credentials or arbitrary text.
type officialModelAuthDiagnostic struct {
	mu           sync.Mutex
	line         []byte
	unauthorized atomic.Bool
}

func (d *officialModelAuthDiagnostic) Write(raw []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, part := range bytes.SplitAfter(raw, []byte{'\n'}) {
		if len(d.line) < 8192 {
			take := min(len(part), 8192-len(d.line))
			d.line = append(d.line, part[:take]...)
		}
		if bytes.HasSuffix(part, []byte{'\n'}) {
			text := string(d.line)
			if strings.Contains(text, "codex_models_manager") && strings.Contains(text, "failed to refresh available models") && officialModelUnauthorizedStatus.MatchString(text) {
				d.unauthorized.Store(true)
			}
			d.line = nil
		}
	}
	return len(raw), nil
}
