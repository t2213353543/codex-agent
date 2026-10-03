package server

import (
	"net/http"
	"testing"
	"time"
)

func TestGatewayRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"", 0, false}, {"garbage", 0, false}, {"-1", 0, false}, {"0.1", 0, false},
		{"0", 0, true}, {" 3 ", 3 * time.Second, true}, {"99999999999999999999999999999", time.Minute, true},
		{now.Add(12 * time.Second).Format(http.TimeFormat), 12 * time.Second, true},
		{now.Add(-time.Second).Format(http.TimeFormat), 0, true},
		{now.Add(time.Hour).Format(http.TimeFormat), time.Minute, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, valid := parseGatewayRetryAfter(tc.value, now, time.Minute)
			if got != tc.want || valid != tc.valid {
				t.Fatalf("got %v/%v, want %v/%v", got, valid, tc.want, tc.valid)
			}
		})
	}
}
