package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry-After is either an integer count of seconds or an HTTP date. Bound
// before multiplying to avoid overflow on untrusted headers. A past date or
// explicit zero is valid and is distinct from a missing/malformed header.
func parseGatewayRetryAfter(value string, now time.Time, maxDelay time.Duration) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" || maxDelay <= 0 {
		return 0, false
	}
	digits := true
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxDelay/time.Second) {
			return maxDelay, true
		}
		return min(time.Duration(seconds)*time.Second, maxDelay), true
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return min(max(time.Duration(0), deadline.Sub(now)), maxDelay), true
}
