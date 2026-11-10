package harness

import (
	"testing"
	"time"
)

func Eventually(t testing.TB, what string, limit time.Duration, check func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(limit)
	last := ""
	for time.Now().Before(deadline) {
		done, detail := check()
		if done {
			return
		}
		last = detail
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %s: %s", what, limit, last)
}
