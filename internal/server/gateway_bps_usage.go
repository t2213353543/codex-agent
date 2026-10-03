package server

// Record the terminal supplied by BPS, retaining the distinction between a
// measured zero cache hit and a response which never reported cache usage.
func recordBPSTerminalUsage(record *RequestRecord, raw []byte) {
	recordGatewayHTTPResponseOutcome(record, raw)
}
