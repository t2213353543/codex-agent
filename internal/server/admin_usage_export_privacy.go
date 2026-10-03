package server

import (
	"crypto/sha256"
	"encoding/hex"
)

func managedExportPseudonym(kind, value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(kind + ":" + value))
	return kind + "_" + hex.EncodeToString(sum[:8])
}
func managedExportRecord(r RequestRecord, redacted bool) managedLogRecord {
	out := managedRecord(r)
	for _, value := range []*string{&out.ClientRequestID, &out.ClientRequestIDSource, &out.ClientRequestIDScope, &out.TimingSource, &out.ImageInputTokensSource, &out.ImageOutputTokensSource} {
		*value = redactMetricError(*value)
	}
	for _, value := range []*string{&out.SentModel, &out.SentReasoningEffort, &out.ReturnedModel, &out.InboundPath, &out.UpstreamPath, &out.UsageSchemaSource, &out.FinalUpstreamErrorCode, &out.FinalUpstreamErrorSource} {
		*value = redactMetricError(*value)
	}
	if len(out.Attempts) > 0 {
		out.Attempts = append([]GatewayAttemptSummary(nil), out.Attempts...)
		for i := range out.Attempts {
			attempt := &out.Attempts[i]
			for _, value := range []*string{&attempt.Phase, &attempt.Endpoint, &attempt.ErrorSource, &attempt.ErrorCode} {
				*value = redactMetricError(*value)
			}
		}
	}
	// Treat all caller/upstream-controlled string metadata as untrusted text.
	for _, value := range []*string{&out.Account, &out.AccountNameSnapshot, &out.KeyName, &out.Model, &out.UpstreamModel, &out.UserAgent, &out.ProxyName, &out.ServiceTier, &out.ReasoningEffort, &out.RequestID, &out.ResponseID, &out.UsageAmount, &out.RequestedServiceTier, &out.RequestedReasoningEffort, &out.ServiceTierSource, &out.ReasoningEffortSource, &out.UpstreamModelSource, &out.FirstOutputKind, &out.TerminalStatus, &out.IncompleteReason, &out.ErrorSource, &out.ErrorPhase} {
		*value = redactMetricError(*value)
	}
	if !redacted {
		return out
	}
	out.EventID = managedExportPseudonym("event", out.EventID)
	out.AccountID = managedExportPseudonym("account", out.AccountID)
	out.KeyID = managedExportPseudonym("key", out.KeyID)
	out.RequestID = managedExportPseudonym("request", out.RequestID)
	out.ClientRequestID = managedExportPseudonym("client_request", out.ClientRequestID)
	out.ResponseID = managedExportPseudonym("response", out.ResponseID)
	groups := []string{}
	for _, id := range out.Groups {
		groups = append(groups, managedExportPseudonym("group", id))
	}
	out.Groups = groups
	out.Account = ""
	out.AccountNameSnapshot = ""
	out.KeyName = ""
	out.ProxyName = ""
	out.IP = ""
	out.UserAgent = ""
	out.Error = ""
	return out
}
