package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

type gatewayBoundWSAcquireTrace struct {
	gatewaySelectionPGAcquireTrace
	ended chan struct{}
}

func (tr *gatewayBoundWSAcquireTrace) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
	if tr.armed.Load() {
		select {
		case tr.ended <- struct{}{}:
		default:
		}
	}
}

func TestPGGatewayBoundCancellationWebSocketDoesNotInventAuthorizationFailure(t *testing.T) {
	f := newGatewaySelectionPGFixture(t, false, false)
	settings, err := f.writer.Settings.Get()
	if err != nil {
		t.Fatal("cannot read synthetic gateway settings")
	}
	settings.AdminAPI, settings.WebsocketEnabled, settings.StickySessionsEnabled = false, true, true
	if err := f.writer.Settings.Set(settings); err != nil {
		t.Fatal("cannot enable isolated ordinary-key WebSocket fixture")
	}
	f.reader.Settings = NewPGSettingsStore(f.writer.Store.DB)
	metrics, err := NewMetrics(filepath.Join(t.TempDir(), "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(metrics.Close)
	f.reader.Metrics = metrics
	tracer := &gatewayBoundWSAcquireTrace{
		gatewaySelectionPGAcquireTrace: gatewaySelectionPGAcquireTrace{started: make(chan struct{}, 1)},
		ended:                          make(chan struct{}, 1),
	}
	config := f.writer.Store.DB.Pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal("cannot create isolated account-read pool")
	}
	t.Cleanup(pool.Close)
	readerDB := *f.writer.Store.DB
	readerDB.Pool = pool
	// Only authoritative account reads use this pool. Settings, key documents,
	// session leases and local metrics remain independently available.
	f.reader.Store.DB = &readerDB
	var handshakes, creates atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handshakes.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			creates.Add(1)
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.created","stream_id":"first","response":{"id":"bound-cancel-first","status":"in_progress"}}`)); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	f.reader.BaseURL, f.reader.Client = upstream.URL, upstream.Client()
	t.Cleanup(f.reader.closeGatewayClients)
	const session = "bound-cancel-live-websocket"
	threadKey := fmt.Sprintf("%sgateway:thread:%x", readerDB.Namespace, sha256.Sum256([]byte(f.key.ID+"\x00"+session)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = readerDB.Redis.Del(ctx, threadKey).Err()
	})
	parentCancel := make(chan context.CancelFunc, 1)
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer close(handlerDone)
		parentCancel <- cancel
		f.reader.ServeHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	headers := http.Header{"Authorization": {"Bearer " + f.key.Key}, "Session-Id": {session}}
	conn, _, err := websocket.Dial(ctx, server.URL+"/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal("cannot connect the ordinary-key bound WebSocket fixture")
	}
	defer conn.CloseNow()
	cancelRequest := <-parentCancel
	defer cancelRequest()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"first","model":"fixture","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_ = gatewayWSRead(t, conn)
	if creates.Load() != 1 || f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 1 {
		t.Fatal("first WebSocket turn must remain active before blocking the next authorization")
	}
	held, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal("cannot occupy the account pool before the next turn")
	}
	defer held.Release()
	defer func() {
		cancelRequest()
		held.Release()
		_ = conn.CloseNow()
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			t.Error("WebSocket handler did not stop after fixture cleanup")
		}
	}()
	f.keepOriginalCacheWarm(t)
	beforeCanceled := pool.Stat().CanceledAcquireCount()
	tracer.armed.Store(true)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","stream_id":"second","model":"fixture","previous_response_id":"bound-cancel-first","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tracer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("second WebSocket turn did not block in the authoritative account read")
	}
	cancelRequest()
	select {
	case <-tracer.ended:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled WebSocket authorization did not complete its pending database acquire")
	}
	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the request did not interrupt the blocked WebSocket authorization")
	}
	if pool.Stat().CanceledAcquireCount() != beforeCanceled+1 || pool.Stat().AcquiredConns() != 1 {
		t.Fatal("cancellation did not interrupt exactly the pending authoritative account read")
	}
	if handshakes.Load() != 1 || creates.Load() != 1 || f.reader.inFlight(gatewayAdmissionKey(f.primary)) != 0 {
		t.Fatal("cancelled authorization sent a second create, reconnected or retained admission")
	}
	_, _, readErr := conn.Read(ctx)
	if readErr == nil {
		t.Fatal("cancelled authorization fabricated a downstream error event")
	}
	records := metrics.RecentForAccount(f.primary.Name, 10)
	if len(records) != 1 || records[0].Status != "aborted" || records[0].ErrorSource != "" || records[0].ErrorPhase != "" || records[0].ResponseID != "bound-cancel-first" {
		t.Fatal("cancelling the second authorization polluted the pending first turn's error source")
	}
	current, err := f.writer.Store.getAccountByID(f.primary.ID)
	if err != nil || current.LastError != f.primary.LastError || current.CredentialVersion != f.primary.CredentialVersion || current.OwnerRevision != f.primary.OwnerRevision {
		t.Fatal("cancelled WebSocket authorization changed credential or account failure state")
	}
	bound, err := f.reader.sessionAccount(t.Context(), f.key.ID, session, "")
	if err != nil || bound != gatewayAccountRef(f.primary) {
		t.Fatal("cancelled WebSocket authorization changed its existing session binding")
	}
}
