package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const gatewayIngressQueueLimit = 256
const gatewayIngressRetentionLimit = 1000
const gatewayIngressSource = "gateway_ingress_v1"

type gatewayIngressEvent struct {
	ID               string `json:"id"`
	Sequence         int64  `json:"sequence"`
	TimeMs           int64  `json:"time_ms"`
	HTTPCode         int    `json:"http_code"`
	Reason           string `json:"reason"`
	Endpoint         string `json:"endpoint,omitempty"`
	ClientRequestID  string `json:"client_request_id,omitempty"`
	AuthenticationMs *int64 `json:"authentication_ms,omitempty"`
}

type gatewayIngressDocument struct {
	Epoch        string                `json:"epoch"`
	NextSequence int64                 `json:"next_sequence"`
	EvictedTotal int64                 `json:"evicted_total"`
	Events       []gatewayIngressEvent `json:"events"`
}

type gatewayIngressCollectorStatus struct {
	Configured             bool   `json:"configured"`
	Running                bool   `json:"running"`
	QueueCapacity          int    `json:"queue_capacity"`
	Queued                 int    `json:"queued"`
	Pending                int64  `json:"pending"`
	QueueDropped           int64  `json:"queue_dropped"`
	PersistenceFailures    int64  `json:"persistence_failures"`
	PersistenceUnconfirmed int64  `json:"persistence_unconfirmed"`
	LastPersistedAt        int64  `json:"last_persisted_at"`
	LastError              string `json:"last_error"`
	UnpersistedAtShutdown  int64  `json:"unpersisted_at_shutdown"`
	CounterScope           string `json:"counter_scope"`
}

type gatewayIngressEventSink struct {
	store       managementStateStore
	queue       chan gatewayIngressEvent
	stop        chan struct{}
	done        chan struct{}
	mu          sync.RWMutex
	accepting   bool
	closeOnce   sync.Once
	prefix      string
	serial      atomic.Uint64
	dropped     atomic.Int64
	pending     atomic.Int64
	failures    atomic.Int64
	unconfirmed atomic.Int64
	lastSaved   atomic.Int64
	failed      atomic.Bool
	unpersisted atomic.Int64
	flushErr    error
	closeCtx    context.Context
	// One worker owns persistence; injection is used only by bounded-queue tests.
	persist func(context.Context, []gatewayIngressEvent) error
}

func gatewayIngressReasonStatus(reason string) int {
	switch reason {
	case "invalid_api_key":
		return 401
	case "settings_unavailable", "key_store_unavailable":
		return 503
	case "method_not_allowed":
		return 405
	case "unknown_endpoint":
		return 404
	case "websocket_disabled":
		return 426
	}
	return 0
}

func validGatewayIngressCorrelation(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.' || ch == ':') {
			return false
		}
	}
	return true
}

func ingressEndpoint(value string) string {
	switch value {
	case "/responses", "/responses/compact", "/models", "/guardian", "/guardian-classifier", "/v1/responses", "/v1/responses/compact", "/v1/models", "/v1/guardian", "/v1/guardian-classifier":
		return value
	}
	return ""
}

func (p *Proxy) startGatewayIngressEvents() error {
	if p.IngressEvents != nil {
		return errors.New("ingress collector already initialized")
	}
	if p.Store.DB == nil && p.Store.Path == "" {
		return errors.New("ingress collector requires configured management storage")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return errors.New("ingress collector identity unavailable")
	}
	sink := &gatewayIngressEventSink{store: p.managementState(), queue: make(chan gatewayIngressEvent, gatewayIngressQueueLimit), stop: make(chan struct{}), done: make(chan struct{}), accepting: true, prefix: hex.EncodeToString(random[:])}
	sink.persist = sink.appendEvents
	p.IngressEvents = sink
	go sink.run()
	return nil
}

func (p *Proxy) recordGatewayIngressEvent(status int, reason, endpoint, clientRequestID string, authenticationMs *int64) {
	sink := p.IngressEvents
	if sink == nil || gatewayIngressReasonStatus(reason) != status {
		return
	}
	event := gatewayIngressEvent{TimeMs: time.Now().UnixMilli(), HTTPCode: status, Reason: reason, Endpoint: ingressEndpoint(endpoint)}
	if validGatewayIngressCorrelation(clientRequestID) {
		event.ClientRequestID = clientRequestID
	}
	if authenticationMs != nil && *authenticationMs >= 0 {
		value := *authenticationMs
		event.AuthenticationMs = &value
	}
	sink.enqueue(event)
}

func (s *gatewayIngressEventSink) enqueue(event gatewayIngressEvent) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.accepting {
		s.dropped.Add(1)
		return
	}
	// This path never reads storage, blocks on queue capacity or spawns a goroutine.
	event.ID = s.prefix + "_" + ingressSequenceID(s.serial.Add(1))
	select {
	case s.queue <- event:
	default:
		s.dropped.Add(1)
	}
}

func ingressSequenceID(value uint64) string {
	var raw [8]byte
	for i := 7; i >= 0; i-- {
		raw[i] = byte(value)
		value >>= 8
	}
	return hex.EncodeToString(raw[:])
}

func decodeGatewayIngressDocument(entry managementStateEntry) (gatewayIngressDocument, error) {
	doc := gatewayIngressDocument{Events: []gatewayIngressEvent{}}
	if entry.Version == 0 {
		return doc, nil
	}
	if json.Unmarshal(entry.Payload, &doc) != nil || !validGatewayIngressCorrelation(doc.Epoch) || doc.NextSequence < 0 || doc.EvictedTotal < 0 || len(doc.Events) > gatewayIngressRetentionLimit {
		return doc, errors.New("invalid ingress observation document")
	}
	var previous int64
	for _, event := range doc.Events {
		if !validGatewayIngressCorrelation(event.ID) || event.Sequence <= previous || event.Sequence > doc.NextSequence || event.TimeMs <= 0 || event.HTTPCode < 400 || gatewayIngressReasonStatus(event.Reason) != event.HTTPCode || event.Endpoint != ingressEndpoint(event.Endpoint) || event.ClientRequestID != "" && !validGatewayIngressCorrelation(event.ClientRequestID) || event.AuthenticationMs != nil && *event.AuthenticationMs < 0 {
			return doc, errors.New("invalid ingress observation event")
		}
		previous = event.Sequence
	}
	if doc.Events == nil {
		doc.Events = []gatewayIngressEvent{}
	}
	return doc, nil
}

func (s *gatewayIngressEventSink) appendEvents(ctx context.Context, batch []gatewayIngressEvent) error {
	if len(batch) == 0 {
		return nil
	}
	return s.updateDocument(ctx, func(entry *managementStateEntry) error {
		doc, err := decodeGatewayIngressDocument(*entry)
		if err != nil {
			return err
		}
		if doc.Epoch == "" {
			doc.Epoch = batch[0].ID
		}
		seen := make(map[string]bool, len(doc.Events))
		for _, event := range doc.Events {
			seen[event.ID] = true
		}
		for _, event := range batch {
			if seen[event.ID] {
				continue
			}
			if doc.NextSequence == math.MaxInt64 {
				return errors.New("ingress sequence capacity exceeded")
			}
			doc.NextSequence++
			event.Sequence = doc.NextSequence
			doc.Events = append(doc.Events, event)
			seen[event.ID] = true
		}
		if excess := len(doc.Events) - gatewayIngressRetentionLimit; excess > 0 {
			doc.Events = append([]gatewayIngressEvent(nil), doc.Events[excess:]...)
			doc.EvictedTotal += int64(excess)
		}
		entry.Payload, err = json.Marshal(doc)
		return err
	})
}

func (s *gatewayIngressEventSink) updateDocument(ctx context.Context, change func(*managementStateEntry) error) error {
	if s.store.db != nil {
		return s.store.Update(ctx, "ingress-events", "recent", change)
	}
	// File fixtures share the management lock, but an unrelated long writer must
	// not make the ingress worker's shutdown wait forever on sync.Mutex.Lock.
	for !managementFileMu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer managementFileMu.Unlock()
	name, _ := managementStateKey("ingress-events", "recent")
	file := s.store.file(name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := os.Mkdir(file+".lock", 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer os.Remove(file + ".lock")
	return newFileStore[managementStateEntry](file).Modify(func(entry *managementStateEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := change(entry); err != nil {
			return err
		}
		entry.Version++
		entry.UpdatedAt = time.Now().Unix()
		return ctx.Err()
	})
}

func (s *gatewayIngressEventSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	batch := make([]gatewayIngressEvent, 0, gatewayIngressQueueLimit)
	flush := func(ctx context.Context) error {
		for len(batch) < gatewayIngressQueueLimit {
			select {
			case event := <-s.queue:
				batch = append(batch, event)
			default:
				goto persist
			}
		}
	persist:
		s.pending.Store(int64(len(batch)))
		if len(batch) == 0 {
			return nil
		}
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := s.persist(bounded, batch)
		cancel()
		if err != nil {
			s.failures.Add(1)
			s.unconfirmed.Add(int64(len(batch)))
			s.failed.Store(true)
			// A failed commit response can mean committed or absent. Do not replay
			// observations after their IDs may have aged out of the retained ring.
			batch = batch[:0]
			s.pending.Store(0)
			return err
		}
		s.lastSaved.Store(time.Now().Unix())
		s.failed.Store(false)
		batch = batch[:0]
		s.pending.Store(0)
		return nil
	}
	for {
		select {
		case <-ticker.C:
			_ = flush(context.Background())
		case <-s.stop:
			for len(batch) > 0 || len(s.queue) > 0 {
				if s.closeCtx.Err() != nil {
					s.unpersisted.Store(int64(len(batch) + len(s.queue)))
					s.flushErr = errors.New("ingress observations were not fully persisted")
					return
				}
				if flush(s.closeCtx) != nil {
					s.flushErr = errors.New("ingress observations have unconfirmed persistence results")
				}
			}
			return
		}
	}
}

func (s *gatewayIngressEventSink) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.accepting = false
		s.closeCtx = ctx
		close(s.stop)
		s.mu.Unlock()
	})
	select {
	case <-s.done:
		return s.flushErr
	case <-ctx.Done():
		return errors.New("ingress observation flush timed out")
	}
}

func (s *gatewayIngressEventSink) status() gatewayIngressCollectorStatus {
	status := gatewayIngressCollectorStatus{QueueCapacity: gatewayIngressQueueLimit, CounterScope: "current_process"}
	if s == nil {
		return status
	}
	s.mu.RLock()
	status.Configured, status.Running = true, s.accepting
	s.mu.RUnlock()
	status.Queued, status.Pending, status.QueueDropped = len(s.queue), s.pending.Load(), s.dropped.Load()
	status.PersistenceFailures, status.LastPersistedAt = s.failures.Load(), s.lastSaved.Load()
	status.PersistenceUnconfirmed = s.unconfirmed.Load()
	status.UnpersistedAtShutdown = s.unpersisted.Load()
	if s.failed.Load() {
		status.LastError = "storage_unavailable"
	}
	return status
}
