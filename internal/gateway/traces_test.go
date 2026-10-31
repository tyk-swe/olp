package gateway

import (
	"io"
	"net/http"

	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
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
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			status := &statuspb.Status{}
			if response.Header.Get("Content-Type") != "application/x-protobuf" || proto.Unmarshal(body, status) != nil || status.GetCode() == 0 || status.GetMessage() == "" {
				t.Fatalf("trace refusal was not an OTLP status: content-type=%q body=%q", response.Header.Get("Content-Type"), body)
			}
		})
	}
}
