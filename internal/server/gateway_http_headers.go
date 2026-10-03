package server

import (
	"net/http"
	"strings"
)

// gatewayHTTPResponseView removes headers belonging to the upstream HTTP hop
// before either policy/observation or downstream forwarding consumes them.
// Filtering only when copying to the client leaves internal Retry-After and
// request metadata able to act on fields the upstream explicitly made local.
// The body, status, transport accounting and end-to-end opaque values are not
// changed. In particular this is not used for WebSocket upgrade responses.
func gatewayHTTPResponseView(response *http.Response) *http.Response {
	if response == nil {
		return nil
	}
	hop := map[string]bool{
		"connection": true, "proxy-connection": true, "keep-alive": true,
		"proxy-authenticate": true, "proxy-authorization": true, "te": true,
		"trailer": true, "transfer-encoding": true, "upgrade": true,
	}
	for name, values := range response.Header {
		if strings.EqualFold(name, "Connection") {
			for _, value := range values {
				for _, nominated := range strings.Split(value, ",") {
					hop[strings.ToLower(strings.TrimSpace(nominated))] = true
				}
			}
		}
	}
	view := *response
	view.Header = response.Header.Clone()
	for name := range view.Header {
		if hop[strings.ToLower(name)] {
			delete(view.Header, name)
		}
	}
	return &view
}
