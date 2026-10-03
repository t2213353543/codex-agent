package bps

import "codex-agent/internal/publicerrors"

// ParameterError identifies a capability the selected upstream cannot preserve.
// The gateway returns an OpenAI-shaped error instead of a generic proxy failure.
type ParameterError struct{ Param, Message string }

func (e *ParameterError) Error() string {
	return publicerrors.Message("upstream: " + e.Message + "; consult /models for supported capabilities")
}
func unsupportedParameter(param, message string) error {
	return &ParameterError{Param: param, Message: message}
}
