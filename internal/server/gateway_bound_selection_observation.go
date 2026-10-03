package server

import (
	"context"
	"time"
)

// GatewayBoundSelectionObservation describes the first exact missing-bound
// selection branch in a logical BPS request. It contains no identities/body.
// Absence is unobserved; zero candidates and zero snapshot age remain known.
type GatewayBoundSelectionObservation struct {
	BoundSnapshotState     string `json:"bound_snapshot_state"`
	EligibleCandidateCount int    `json:"eligible_candidate_count"`
	SnapshotOrigin         string `json:"snapshot_origin"`
	SnapshotAgeMS          *int64 `json:"snapshot_age_ms,omitempty"`
	ReplayClass            string `json:"replay_class,omitempty"`
	RecoveryOutcome        string `json:"recovery_outcome,omitempty"`
}

type gatewayAccountSnapshotObservation struct {
	origin string
	ageMS  int64
	known  bool
}

func (o *gatewayAccountSnapshotObservation) capture(origin string, now, loaded time.Time) {
	if o == nil {
		return
	}
	o.origin = origin
	o.known = !loaded.IsZero()
	if o.known {
		o.ageMS = max(int64(0), now.Sub(loaded).Milliseconds())
	}
}

// This descriptor lives only on pickSessionAccount's stack. accounts and
// allowed are borrowed from the exact selection operation, never put in ctx,
// failover state, RequestRecord, storage or another goroutine.
type gatewayBoundCandidateSnapshot struct {
	accounts []Account
	allowed  map[string]bool
	admin    bool
	snapshot gatewayAccountSnapshotObservation
}

type gatewayBoundSelectionCaptureKey struct{}
type gatewayBoundSelectionCapture struct {
	observation *GatewayBoundSelectionObservation
}

func (snapshot *gatewayBoundCandidateSnapshot) missing(bound string, eligible int) *GatewayBoundSelectionObservation {
	if snapshot == nil || snapshot.snapshot.origin == "" {
		return nil
	}
	state := "missing_or_replaced"
	for _, account := range snapshot.accounts {
		if gatewayAccountRef(account) != bound {
			continue
		}
		state = "unclassified"
		if !account.Enabled {
			state = "exact_disabled"
		} else if !snapshot.admin {
			permitted := false
			for _, group := range account.Groups {
				permitted = permitted || snapshot.allowed[group]
			}
			if !permitted {
				state = "exact_group_denied"
			}
		}
		break
	}
	observation := &GatewayBoundSelectionObservation{BoundSnapshotState: state, EligibleCandidateCount: eligible, SnapshotOrigin: snapshot.snapshot.origin}
	if snapshot.snapshot.known {
		age := snapshot.snapshot.ageMS
		observation.SnapshotAgeMS = &age
	}
	return observation
}

func cloneGatewayBoundSelection(value *GatewayBoundSelectionObservation) *GatewayBoundSelectionObservation {
	if value == nil {
		return nil
	}
	copy := *value
	if value.SnapshotAgeMS != nil {
		age := *value.SnapshotAgeMS
		copy.SnapshotAgeMS = &age
	}
	return &copy
}

func (s *officialFailoverState) rememberBoundSelection(value *GatewayBoundSelectionObservation) {
	if s == nil || value == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boundSelection == nil {
		s.boundSelection = cloneGatewayBoundSelection(value)
		s.boundSelection.ReplayClass = s.boundReplayClass
	}
}

func (s *officialFailoverState) attachBoundSelection(record *RequestRecord, outcome string) {
	if s == nil || record == nil {
		return
	}
	s.mu.Lock()
	value := cloneGatewayBoundSelection(s.boundSelection)
	s.mu.Unlock()
	if value == nil {
		return
	}
	value.RecoveryOutcome = outcome
	record.BoundSelection = value
}

// Evaluate only after commit/delivery has set the final record disposition.
// Starting replacement selection is not evidence of committed recovery.
func (s *officialFailoverState) finishBoundSelection(ctx context.Context, record *RequestRecord, exhausted bool) {
	if s == nil || record == nil {
		return
	}
	outcome := "blocked_other_guard"
	switch {
	case ctx.Err() != nil:
		outcome = "blocked_context"
	case record.Status == "aborted":
		// A delivery failure can follow owner commit. Do not report recovered.
		if s.retrying {
			outcome = "replacement_failed"
		}
	case exhausted:
		outcome = "candidates_exhausted"
	case s.retrying && record.Status == "ok" && s.committed && s.failure == nil:
		outcome = "recovered"
	case s.retrying:
		outcome = "replacement_failed"
	case s.replayBlocked:
		outcome = "blocked_input"
	case appServerDelegatedOwner(ctx) != "":
		outcome = "blocked_delegation"
	}
	s.attachBoundSelection(record, outcome)
}
