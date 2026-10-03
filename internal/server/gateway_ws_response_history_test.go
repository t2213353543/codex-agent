package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestGatewayWSResponseHistoryBoundedFIFO(t *testing.T) {
	var history gatewayWSResponseHistory
	for i := 0; i < maxGatewayWSCompletedResponses; i++ {
		history.add(fmt.Sprintf("response-%d", i))
	}
	if len(history.ids) != maxGatewayWSCompletedResponses || len(history.order) != maxGatewayWSCompletedResponses {
		t.Fatal("completed response history did not fill its fixed window")
	}
	for range 10 {
		history.add("response-0")
	}
	history.add("response-new")
	if history.contains("response-0") || !history.contains("response-1") || !history.contains("response-new") || history.next != 1 {
		t.Fatal("duplicate completion refreshed its FIFO slot or eviction removed a newer ID")
	}
	for i := maxGatewayWSCompletedResponses; i < maxGatewayWSCompletedResponses*4; i++ {
		history.add(fmt.Sprintf("response-%d", i))
		if len(history.ids) != maxGatewayWSCompletedResponses || len(history.order) != maxGatewayWSCompletedResponses {
			t.Fatal("completed response history grew beyond its configured bound")
		}
	}
	for i := 0; i < maxGatewayWSCompletedResponses*4; i++ {
		want := i >= maxGatewayWSCompletedResponses*3
		if history.contains(fmt.Sprintf("response-%d", i)) != want {
			t.Fatal("completed response history did not retain exactly the latest window")
		}
	}
	if history.contains("response-new") {
		t.Fatal("completed response history retained an evicted entry")
	}
}

func TestGatewayWSResponseHistoryExactBoundedIDs(t *testing.T) {
	var history gatewayWSResponseHistory
	oversized := strings.Repeat("x", 1025)
	for _, invalid := range []string{"", oversized} {
		history.add(invalid)
		if history.contains(invalid) || len(history.ids) != 0 || len(history.order) != 0 {
			t.Fatal("missing or oversized identity was retained")
		}
	}
	maximum := oversized[:1024]
	for _, id := range []string{"response", "Response", " response", "response ", maximum} {
		history.add(id)
	}
	if len(history.ids) != 5 || history.contains(oversized) || history.contains("RESPONSE") {
		t.Fatal("response identity was normalized, truncated or conflated")
	}
	for _, id := range []string{"response", "Response", " response", "response ", maximum} {
		if !history.contains(id) {
			t.Fatal("exact completed response identity was lost")
		}
	}
	var otherConnection gatewayWSResponseHistory
	if otherConnection.contains("response") {
		t.Fatal("completed response identity leaked between connections")
	}
}
