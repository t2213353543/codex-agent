package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestGatewayBoundCancellationPrecedesAccountRead(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled", true: "deadline"}[deadline], func(t *testing.T) {
			p := newTestProxy(t, 1)
			account, err := p.Store.Get("test")
			if err != nil {
				t.Fatal(err)
			}
			// A read of this store would fail. The expired caller must win before
			// any storage error is mistaken for an ownership or authorization error.
			p.Store.Path = filepath.Join(t.TempDir(), "missing", "accounts.json")
			ctx, cancel := context.WithCancel(t.Context())
			want := error(context.Canceled)
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			}
			defer cancel()
			cancel()
			current, err := p.gatewayCurrentBoundAccountContext(ctx, account, gatewayAccountRef(account))
			if !errors.Is(err, want) || current.ID != "" {
				t.Fatal("expired bound lookup touched storage or returned an account")
			}
		})
	}
}
