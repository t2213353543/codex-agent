package bps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestStreamQuotaCamelAliasesPreserved(t *testing.T) {
	for _, code := range []string{"rateLimitExceeded", "usageLimitExceeded"} {
		wire := fmt.Sprintf(`data: {"type":"response.failed","response":{"id":"quota","status":"failed","error":{"code":%q}}}`, code) + "\n\n"
		_, err := Stream(context.Background(), io.Discard, strings.NewReader(wire), StreamOptions{})
		var terminal *TerminalError
		if !errors.As(err, &terminal) || terminal.Code != code || !strings.Contains(terminal.Error(), "code="+code) {
			t.Fatalf("known camel quota silently discarded: code=%s err=%v", code, err)
		}
	}
}
