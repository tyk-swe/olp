package gateway

import (
	"net/http"
	"testing"
)

func TestTraceIngestionRequiresAuthenticatedScope(t *testing.T) {
	h := newHarness(t, Config{})
	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{name: "missing credential", want: http.StatusUnauthorized},
		{name: "missing trace scope", key: fullKey, want: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := h.do(t.Context(), http.MethodPost, "/v1/traces", tc.key, []byte{}, map[string]string{"Content-Type": "application/x-protobuf"})
			defer response.Body.Close()
			if response.StatusCode != tc.want {
				t.Fatalf("trace request returned %d, want %d", response.StatusCode, tc.want)
			}
		})
	}
}
