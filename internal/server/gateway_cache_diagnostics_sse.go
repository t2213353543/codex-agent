package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
)

// gatewayCacheResponseObserver only retains the current bounded SSE frame. Once
// a terminal frame is processed, only aggregate counts and usage remain. This
// observer is intentionally separate from the gateway's production accounting.
type gatewayCacheResponseObserver struct {
	mu          sync.Mutex
	diagnostics *gatewayCacheDiagnostics
	id          uint64
	line        []byte
	eventData   []byte
	skipLF      bool
	started     bool
	done        bool
	err         error
}

// ResponseObserver can be supplied to io.TeeReader in a benchmark RoundTripper.
// Always call Finish on body close, including cancelled and failed requests.
// The caller provides the decoded SSE body, without content encoding.
func (d *gatewayCacheDiagnostics) ResponseObserver(id uint64) *gatewayCacheResponseObserver {
	return &gatewayCacheResponseObserver{diagnostics: d, id: id}
}

// Write never interrupts transport because a diagnostic could not be collected.
// Any diagnostic failure is returned by Finish, without including upstream data.
func (o *gatewayCacheResponseObserver) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	for len(p) > 0 && !o.done {
		if o.skipLF {
			o.skipLF = false
			if p[0] == '\n' {
				p = p[1:]
				continue
			}
		}
		index := bytes.IndexAny(p, "\r\n")
		size := index
		if size < 0 {
			size = len(p)
		}
		if size > maxBody-len(o.line)-len(o.eventData) {
			o.stop(errors.New("cache diagnostics SSE event exceeds 32 MiB"))
			break
		}
		o.line = append(o.line, p[:size]...)
		if index < 0 {
			break
		}
		o.consumeLine(o.line)
		o.line = o.line[:0]
		o.skipLF = p[index] == '\r'
		p = p[index+1:]
	}
	return n, nil
}

func (o *gatewayCacheResponseObserver) consumeLine(line []byte) {
	line = stripInitialSSEBOM(line, &o.started)
	if !validSSELine(line) {
		o.stop(errors.New("cache diagnostics SSE contains invalid UTF-8"))
		return
	}
	if len(line) == 0 {
		payload := bytes.TrimSuffix(o.eventData, []byte{'\n'})
		o.eventData = nil
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &event) != nil {
			return
		}
		switch event.Type {
		case "error", "response.failed", "response.completed", "response.done", "response.incomplete":
			o.stop(o.diagnostics.ObserveResponse(o.id, payload))
		}
		return
	}
	var value []byte
	switch {
	case bytes.HasPrefix(line, []byte("data:")):
		value = bytes.TrimPrefix(line[len("data:"):], []byte(" "))
	case bytes.Equal(line, []byte("data")):
	default:
		return
	}
	if len(value)+1 > maxBody-len(o.eventData) {
		o.stop(errors.New("cache diagnostics SSE event exceeds 32 MiB"))
		return
	}
	o.eventData = append(o.eventData, value...)
	o.eventData = append(o.eventData, '\n')
}

func (o *gatewayCacheResponseObserver) stop(err error) {
	o.done, o.err = true, err
	o.line, o.eventData = nil, nil
	if err != nil {
		o.diagnostics.abandon(o.id)
	}
}

// Finish requires the terminal event's blank line, matching the native CLI's
// parser. An unterminated JSON tail is an unfinished stream, not a cache miss.
func (o *gatewayCacheResponseObserver) Finish() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done {
		o.stop(errors.New("cache diagnostics stream ended without a terminal response"))
	}
	return o.err
}
