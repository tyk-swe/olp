package upstream

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfter parses a Retry-After header as seconds or an HTTP date.
func RetryAfter(value string, now time.Time) time.Duration {
	delay, _ := ParseRetryAfter(value, now)
	return delay
}

func ParseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseUint(trimmed, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		for _, c := range trimmed {
			if c < '0' || c > '9' {
				return 0, false
			}
		}
		const maxSeconds = uint64((1<<63 - 1) / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second, true
	}
	if at, err := http.ParseTime(trimmed); err == nil {
		return max(at.Sub(now), 0), true
	}
	return 0, false
}
