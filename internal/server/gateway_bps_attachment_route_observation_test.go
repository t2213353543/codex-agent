package server

import "testing"

func TestBPSAttachmentFailoverNetworkRouteIsDistinguishable(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"/attachments", "/attachments"},
		{"/basispoints/api/attachments", "/attachments"},
		{"https://bps.openai.com/basispoints/api/attachments?private=not-retained", "/attachments"},
		{"/basispoints/api/responses", "/responses"},
		{"/basispoints/api/responses/compact", "/responses/compact"},
		{"/basispoints/api/attachments/delete/file_private", ""},
		{"/basispoints/api/arbitrary_private_path", ""},
	} {
		if got := gatewayObservedRoute(tc.input); got != tc.want {
			t.Errorf("route(%q)=%q want %q", tc.input, got, tc.want)
		}
	}
}
