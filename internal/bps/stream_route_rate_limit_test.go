package bps

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStreamBPSRouteRateLimitExactCode(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}`,
		`{"type":"error","error":{"code":"basispoints_route_rate_limit_exceeded","message":"Rate limit exceeded"}}`,
		`{"type":"response.failed","response":{"id":"route","status":"failed","error":{"code":"basispoints_route_rate_limit_exceeded"}}}`,
	} {
		_, err := Stream(context.Background(), io.Discard, strings.NewReader("data: "+event+"\n\n"), StreamOptions{})
		var terminal *TerminalError
		if !errors.As(err, &terminal) || terminal.Code != "basispoints_route_rate_limit_exceeded" || !strings.Contains(terminal.Error(), "(code=basispoints_route_rate_limit_exceeded)") || !strings.Contains(string(terminal.Payload), "basispoints_route_rate_limit_exceeded") {
			t.Fatalf("route quota code lost: %v", err)
		}
	}
}
