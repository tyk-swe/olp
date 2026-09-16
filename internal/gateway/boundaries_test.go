package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/runtime"
)

func TestBodyLimitIncludesGzipWireBytes(t *testing.T) {
	const limit = 1024
	s := &Server{cfg: Config{MaxBodyBytes: limit}}
	compress := func(body string, extra int) []byte {
		t.Helper()
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Header.Extra = make([]byte, extra)
		if _, err := io.WriteString(w, body); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	read := func(body []byte, encoding string, want int) {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Content-Encoding", encoding)
		_, err := s.readBody(r)
		got := http.StatusOK
		if err != nil {
			got = err.Status
		}
		if got != want {
			t.Errorf("%s, %d wire bytes: got %d, want %d", encoding, len(body), got, want)
		}
	}
	base := len(compress("{}", 1))
	for _, size := range []int{limit - 1, limit, limit + 1, 2 * limit} {
		want := http.StatusOK
		if size > limit {
			want = http.StatusRequestEntityTooLarge
		}
		body := compress("{}", 1+size-base)
		if len(body) != size {
			t.Fatal("bad gzip fixture")
		}
		read(body, "gzip", want)
		read(bytes.Repeat([]byte(" "), size), "identity", want)
	}
	read(compress(strings.Repeat(" ", limit+1), 0), "gzip", http.StatusRequestEntityTooLarge)
	read([]byte("bad gzip"), "gzip", http.StatusBadRequest)
}

func TestRoutingOverrideIsExactlyOneObject(t *testing.T) {
	route := &runtime.Route{MaxAttempts: 3}
	for _, raw := range []string{"", "null", "{}]", "{}}", "{} {}", "[]", `{"unexpected":true}`, `{"max_attempts":0}`, `{"max_attempts":4}`} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set(routingHeader, raw)
		if _, err := attemptBudget(r, route); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for raw, want := range map[string]int{"{}  ": 3, `{"strategy":"weighted","max_attempts":2}`: 2} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set(routingHeader, raw)
		if got, err := attemptBudget(r, route); err != nil || got != want {
			t.Errorf("%s: %d, %v", raw, got, err)
		}
	}
}

func TestRetryAfterCannotOverflow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, value := range []string{"9223372036854775807", "18446744073709551616"} {
		if got := retryAfter(value, now); got <= time.Minute {
			t.Errorf("%s overflowed: %v", value, got)
		}
	}
	for value, want := range map[string]time.Duration{"2": 2 * time.Second, "120": 2 * time.Minute, "-1": 0, "nonsense": 0, "18446744073709551616junk": 0, now.Add(5 * time.Second).Format(http.TimeFormat): 5 * time.Second} {
		if got := retryAfter(value, now); got != want {
			t.Errorf("%s: %v, want %v", value, got, want)
		}
	}
}
