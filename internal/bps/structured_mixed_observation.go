package bps

import "errors"

// StructuredMixedObservation describes the conflicting final output shapes.
// It contains no tool identities, arguments, text, schemas or opaque state.
// Zero counts and false flags are observed values and remain in JSON.
type StructuredMixedObservation struct {
	FormatterCount  int  `json:"formatter_count"`
	ClientCallCount int  `json:"client_call_count"`
	OtherCallCount  int  `json:"other_call_count"`
	HasText         bool `json:"has_text"`
	HasRefusal      bool `json:"has_refusal"`
}

type structuredMixedError struct {
	observation StructuredMixedObservation
}

func (*structuredMixedError) Error() string {
	return "structured output mixed incompatible final outputs"
}

// ObserveStructuredMixed returns an independent metadata copy for an actual
// mixed-output rejection, including through the existing validation wrapper.
// Matching error text alone never establishes this observation.
func ObserveStructuredMixed(err error) *StructuredMixedObservation {
	var mixed *structuredMixedError
	if !errors.As(err, &mixed) || mixed == nil {
		return nil
	}
	observation := mixed.observation
	return &observation
}
