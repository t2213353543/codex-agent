package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// acquire checks Err before waiting; this signal identifies its actual wait on
// a shared starting entry without relying on scheduler sleeps.
type nativeStartupObservedWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *nativeStartupObservedWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestGatewayNativeStartupFirstCallerCancellationKeepsOtherWaiter(t *testing.T) {
	commandEntered := make(chan struct{})
	allowCommand := make(chan struct{})
	var unblock sync.Once
	releaseCommand := func() { unblock.Do(func() { close(allowCommand) }) }
	defer releaseCommand()
	var starts atomic.Int32
	pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
		original := options.command
		options.command = func(path string) *exec.Cmd {
			if starts.Add(1) == 1 {
				close(commandEntered)
				<-allowCommand
			}
			return original(path)
		}
	})
	key := gatewayNativeKey{origin: "http://shared-startup.invalid", timeout: time.Second}
	type acquired struct {
		child *gatewayNativeProcess
		err   error
	}
	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	firstDone := make(chan acquired, 1)
	go func() {
		child, err := pool.acquire(firstCtx, key, nil)
		firstDone <- acquired{child, err}
	}()
	select {
	case <-commandEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first caller did not reach the controlled startup barrier")
	}
	secondCtx, cancelSecond := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelSecond()
	observed := &nativeStartupObservedWaitContext{Context: secondCtx, waiting: make(chan struct{})}
	secondDone := make(chan acquired, 1)
	go func() {
		child, err := pool.acquire(observed, key, nil)
		secondDone <- acquired{child, err}
	}()
	select {
	case <-observed.waiting:
	case <-secondCtx.Done():
		t.Fatal("second caller did not wait for the same startup result")
	}
	pool.mu.Lock()
	shared := pool.entries[key]
	sharedStarting := shared != nil && shared.starting && len(pool.entries) == 1
	pool.mu.Unlock()
	if !sharedStarting || starts.Load() != 1 {
		t.Fatal("fixture did not establish one shared in-progress helper start")
	}
	cancelFirst()
	select {
	case first := <-firstDone:
		if first.child != nil {
			pool.release(first.child)
			t.Fatal("cancelled first caller acquired a slot before startup was released")
		}
		if !errors.Is(first.err, context.Canceled) {
			t.Fatal("first startup waiter lost its own cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("first caller stayed blocked on another caller's startup")
	}
	releaseCommand()
	var second acquired
	select {
	case second = <-secondDone:
	case <-secondCtx.Done():
		t.Fatal("second caller did not finish after the startup barrier opened")
	}
	if second.err != nil || second.child == nil {
		t.Fatalf("first caller cancellation killed a helper still needed by another waiter: %v", second.err)
	}
	pool.mu.Lock()
	active := second.child.active
	pool.mu.Unlock()
	pool.release(second.child)
	if active != 1 || starts.Load() != 1 {
		t.Fatal("cancelled caller consumed a slot or recovery started another helper")
	}
	// Only the surviving caller sends IPC after readiness. The cancelled
	// acquire never has a connection on which request bytes could be sent.
	req, err := http.NewRequestWithContext(secondCtx, http.MethodPost, key.origin+"/echo", strings.NewReader("surviving-waiter"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := pool.RoundTripper(nil, key.timeout).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "surviving-waiter" || starts.Load() != 1 {
		t.Fatal("surviving caller did not complete one echo using the original shared helper")
	}
}

type nativeStartupAcquireResult struct {
	child *gatewayNativeProcess
	err   error
}

func nativeStartupAcquireAsync(pool *gatewayNativePool, ctx context.Context, key gatewayNativeKey) <-chan nativeStartupAcquireResult {
	done := make(chan nativeStartupAcquireResult, 1)
	go func() { child, err := pool.acquire(ctx, key, nil); done <- nativeStartupAcquireResult{child, err} }()
	return done
}

func nativeStartupReceive(t *testing.T, done <-chan nativeStartupAcquireResult) nativeStartupAcquireResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("controlled startup acquire did not finish")
		return nativeStartupAcquireResult{}
	}
}

func nativeStartupBarrierPool(t *testing.T) (*gatewayNativePool, <-chan struct{}, func(), *atomic.Int32) {
	t.Helper()
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(proceed) }) }
	starts := &atomic.Int32{}
	pool := nativeHTTPFixturePool(t, func(options *gatewayNativeOptions) {
		options.idleProcessLimit = 1
		original := options.command
		options.command = func(path string) *exec.Cmd {
			if starts.Add(1) == 1 {
				close(entered)
				<-proceed
			}
			return original(path)
		}
	})
	// Registered after pool cleanup so a failing test cannot leave Close
	// waiting forever for its deliberately blocked command factory.
	t.Cleanup(release)
	return pool, entered, release, starts
}

func nativeStartupWaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("controlled startup synchronization not reached")
	}
}

func TestGatewayNativeStartupSecondCallerCancellationKeepsCreator(t *testing.T) {
	pool, entered, release, starts := nativeStartupBarrierPool(t)
	key := gatewayNativeKey{origin: "http://second-cancel.invalid", timeout: time.Second}
	first := nativeStartupAcquireAsync(pool, t.Context(), key)
	nativeStartupWaitSignal(t, entered)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &nativeStartupObservedWaitContext{Context: ctx, waiting: make(chan struct{})}
	second := nativeStartupAcquireAsync(pool, observed, key)
	nativeStartupWaitSignal(t, observed.waiting)
	cancel()
	result := nativeStartupReceive(t, second)
	if result.child != nil || !errors.Is(result.err, context.Canceled) {
		t.Fatal("second cancellation acquired a helper or lost own error")
	}
	pool.mu.Lock()
	child := pool.entries[key]
	valid := child != nil && child.starting && child.startupWaiters == 1 && !child.startupAbandoned && !child.retired
	pool.mu.Unlock()
	if !valid {
		t.Fatal("second cancellation retired creator's still-needed startup")
	}
	release()
	result = nativeStartupReceive(t, first)
	if result.err != nil || result.child == nil {
		t.Fatal("creator failed after another waiter cancelled")
	}
	pool.release(result.child)
	if starts.Load() != 1 {
		t.Fatal("second cancellation restarted the creator's helper")
	}
}

func TestGatewayNativeStartupAllCancelThenLateWaiterStartsFresh(t *testing.T) {
	pool, entered, release, starts := nativeStartupBarrierPool(t)
	key := gatewayNativeKey{origin: "http://all-cancel.invalid", timeout: time.Second}
	firstCtx, firstCancel := context.WithCancel(t.Context())
	defer firstCancel()
	first := nativeStartupAcquireAsync(pool, firstCtx, key)
	nativeStartupWaitSignal(t, entered)
	secondCtx, secondCancel := context.WithCancel(t.Context())
	defer secondCancel()
	secondObserved := &nativeStartupObservedWaitContext{Context: secondCtx, waiting: make(chan struct{})}
	second := nativeStartupAcquireAsync(pool, secondObserved, key)
	nativeStartupWaitSignal(t, secondObserved.waiting)
	firstCancel()
	secondCancel()
	for _, pending := range []<-chan nativeStartupAcquireResult{first, second} {
		result := nativeStartupReceive(t, pending)
		if result.child != nil || !errors.Is(result.err, context.Canceled) {
			t.Fatal("abandoned caller acquired a slot or lost cancellation")
		}
	}
	pool.mu.Lock()
	abandoned := pool.entries[key]
	valid := abandoned != nil && abandoned.starting && abandoned.startupWaiters == 0 && abandoned.startupAbandoned && abandoned.retired && len(pool.entries) == 1
	pool.mu.Unlock()
	if !valid {
		t.Fatal("all-cancel startup was not marked abandoned while retaining its process reservation")
	}
	lateCtx, cancelLate := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelLate()
	lateObserved := &nativeStartupObservedWaitContext{Context: lateCtx, waiting: make(chan struct{})}
	late := nativeStartupAcquireAsync(pool, lateObserved, key)
	nativeStartupWaitSignal(t, lateObserved.waiting)
	select {
	case <-late:
		t.Fatal("late caller reused abandoned startup before cleanup")
	default:
	}
	if starts.Load() != 1 {
		t.Fatal("late caller started a second helper before abandoned reservation was cleaned")
	}
	release()
	result := nativeStartupReceive(t, late)
	if result.err != nil || result.child == nil || result.child == abandoned {
		t.Fatal("late caller did not receive a fresh helper after cleanup")
	}
	select {
	case <-abandoned.finished:
	default:
		t.Fatal("fresh helper started before abandoned process cleanup finished")
	}
	pool.mu.Lock()
	active := result.child.active
	entries := len(pool.entries)
	pool.mu.Unlock()
	pool.release(result.child)
	if starts.Load() != 2 || entries != 1 || active != 1 {
		t.Fatal("fresh startup violated process/admission limits")
	}
	req, _ := http.NewRequestWithContext(lateCtx, "POST", key.origin+"/echo", strings.NewReader("late-once"))
	response, err := pool.RoundTripper(nil, key.timeout).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || string(body) != "late-once" || starts.Load() != 2 {
		t.Fatal("late request did not complete on the single replacement helper")
	}
}

func TestGatewayNativeStartupCloseKeepsPendingReservationUntilReaped(t *testing.T) {
	pool, entered, release, starts := nativeStartupBarrierPool(t)
	key := gatewayNativeKey{origin: "http://close-startup.invalid", timeout: time.Second}
	first := nativeStartupAcquireAsync(pool, t.Context(), key)
	nativeStartupWaitSignal(t, entered)
	closed := make(chan error, 1)
	go func() { closed <- pool.Close() }()
	nativeStartupWaitSignal(t, pool.stop)
	pool.mu.Lock()
	child := pool.entries[key]
	valid := pool.closed && child != nil && child.starting && child.retired && len(pool.entries) == 1
	pool.mu.Unlock()
	if !valid {
		t.Fatal("Close forgot an in-progress startup reservation")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before blocked startup was reaped")
	default:
	}
	if acquired, err := pool.acquire(t.Context(), key, nil); acquired != nil || !errors.Is(err, errGatewayNativeClosed) {
		t.Fatal("closed pool admitted another startup")
	}
	release()
	result := nativeStartupReceive(t, first)
	if result.child != nil || !errors.Is(result.err, errGatewayNativeClosed) {
		t.Fatal("pending waiter obtained helper after pool close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pool Close did not finish after startup released")
	}
	if starts.Load() != 1 || len(pool.entries) != 0 {
		t.Fatal("closed pool retained resources or spawned replacement")
	}
}

func TestGatewayNativeStartupReadyAndCanceledPrefersCallerCancellation(t *testing.T) {
	for iteration := range 4 {
		ctx, cancel := context.WithCancel(t.Context())
		child := &gatewayNativeProcess{ready: make(chan struct{}), startupWaiters: 1}
		pool := &gatewayNativePool{entries: make(map[gatewayNativeKey]*gatewayNativeProcess)}
		if iteration%2 == 0 {
			close(child.ready)
			cancel()
		} else {
			cancel()
			close(child.ready)
		}
		if err := pool.waitStartup(ctx, child); !errors.Is(err, context.Canceled) {
			t.Fatal("ready notification replaced this caller's cancellation")
		}
		if child.startupWaiters != 0 || child.startupAbandoned || child.retired {
			t.Fatal("cancel after ready corrupted waiter accounting or retired healthy shared startup")
		}
	}
}
