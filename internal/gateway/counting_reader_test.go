package gateway

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestCountingReaderAcceptsExactLimitAndDetectsExtraByte(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int64
	}{
		{"below", "ab", 3}, {"exact", "abc", 3}, {"over", "abcd", 3}, {"empty", "", 0}, {"zero limit", "a", 0},
	} {
		for _, eofWithData := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "separate EOF", true: "EOF with data"}[eofWithData], func(t *testing.T) {
				var upstream io.Reader = strings.NewReader(tc.body)
				if eofWithData {
					upstream = iotest.DataErrReader(upstream)
				}
				r := &countingReader{r: upstream, limit: tc.limit}
				body, err := io.ReadAll(r)
				exceeded := int64(len(tc.body)) > tc.limit
				if errors.Is(err, errResponseTooLarge) != exceeded || r.exceeded != exceeded || (!exceeded && err != nil) {
					t.Fatalf("limit %d: exceeded=%t, err=%v", tc.limit, r.exceeded, err)
				}
				if string(body) != tc.body[:min(int64(len(tc.body)), tc.limit)] {
					t.Fatalf("returned bytes outside limit: %q", body)
				}
			})
		}
	}
	reader := &countingReader{r: io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(io.ErrUnexpectedEOF)), limit: 3}
	if _, err := io.ReadAll(reader); err != io.ErrUnexpectedEOF || reader.exceeded {
		t.Fatalf("boundary read error changed: %v, exceeded=%t", err, reader.exceeded)
	}
}

func TestUnaryResponseExactlyAtLimitSucceeds(t *testing.T) {
	const body = `{"id":"c","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	h := newHarness(t, Config{MaxInFlight: 1, MaxBodyBytes: 4096, MaxEventBytes: 4096, MaxResponseBytes: int64(len(body))})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
		w.(http.Flusher).Flush()
	})
	resp, result := h.chat(fullKey, nil)
	if resp.StatusCode != http.StatusOK || result["model"] != routeSlug {
		t.Fatalf("exact limit response: %d %v", resp.StatusCode, result)
	}
}
