package server

import "encoding/json"

func observeOfficialModelSource(rec *RequestRecord, raw []byte) {
	object := gatewayResponseObject(raw)
	if object == nil || !gatewayResponseMatchesRecord(rec, object) {
		return
	}
	var metadata map[string]string
	if json.Unmarshal(object["metadata"], &metadata) == nil {
		switch source := metadata["app_server_model_source"]; source {
		case "app_server_thread", "app_server_settings", "app_server_rerouted":
			rec.UpstreamModelSource = source
		}
	}
}

// JSON and SSE share the identical completed response accounting boundary.
// event() observes progress/TTFT; it intentionally does not account usage.
func recordOfficialResponseOutcome(rec *RequestRecord, result []byte) {
	recordGatewayHTTPResponseOutcome(rec, result)
	observeOfficialModelSource(rec, result)
	var response struct {
		Metadata struct {
			ModelSource string `json:"app_server_model_source"`
			UsageSource string `json:"app_server_usage_source"`
		} `json:"metadata"`
	}
	if json.Unmarshal(result, &response) != nil {
		return
	}
	if source := response.Metadata.ModelSource; source != "" {
		rec.UpstreamModelSource = boundedGatewayMetadata(source, 128)
	}
	if rec.UsageKnown != nil && *rec.UsageKnown {
		rec.UsageSchemaSource = "app_server_raw_response"
		if source := response.Metadata.UsageSource; source != "" {
			rec.UsageSchemaSource = boundedGatewayMetadata(source, 128)
		}
	}
}
