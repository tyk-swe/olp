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
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		for _, c := range value {
			if c < '0' || c > '9' {
				return 0
			}
		}
		const maxSeconds = uint64((1<<63 - 1) / time.Second)
		return time.Duration(min(seconds, maxSeconds)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}
