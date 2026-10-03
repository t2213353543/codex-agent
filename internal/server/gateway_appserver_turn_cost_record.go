package server

import (
	"context"
	"time"
)

// An auxiliary cost-index write must not hold the user's terminal model
// response open on Redis failure. Missing provenance stays pending at query.
func (a *appServerCLI) recordTurnCost(keyID, clientTurn string, mapping appServerTurnCostMapping) {
	if a == nil || a.turnCosts == nil {
		return
	}
	if a.turnCosts.db == nil {
		_ = a.turnCosts.Record(context.Background(), keyID, clientTurn, mapping)
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.turnCostWrites.Add(1)
	a.mu.Unlock()
	go func() {
		defer a.turnCostWrites.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.turnCosts.Record(ctx, keyID, clientTurn, mapping)
	}()
}
