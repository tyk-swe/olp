package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	for _, raw := range []string{"", "null", "{}]", "{}}", "{} {}", "[]", `{"unexpected":true}`, `{"max_attempts":0}`} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set(routingHeader, raw)
		if _, err := routingPreferences(r); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"{}  ", `{"strategy":"weighted","max_attempts":2}`} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set(routingHeader, raw)
		if got, err := routingPreferences(r); err != nil || got == nil {
			t.Errorf("%s: %v, %v", raw, got, err)
		}
	}
}
