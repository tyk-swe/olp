package process

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/management"
)

// The public mux composes the console, the prefix guards and, with inference,
// the gateway the way Run does. Composition must not conflict, and no gateway
// prefix the gateway leaves unanswered may reach the SPA.
func TestGatewayPrefixesNeverFallThroughToConsole(t *testing.T) {
	for _, inference := range []bool{true, false} {
		public := http.NewServeMux()
		management.Register(public)
		public.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "console") })
		guardPublicPrefixes(public, inference)
		paths := []string{"/native/anthropic/models/route", "/ws/unknown"}
		if inference {
			policy := egress.Policy{}
			gateway.New(nil, &policy, gateway.Config{MaxInFlight: 1}, slog.New(slog.DiscardHandler)).Register(public)
		} else {
			paths = append(paths, "/bedrock/model/route/converse", "/v1/models")
		}
		for _, path := range paths {
			w := httptest.NewRecorder()
			public.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("inference=%t GET %s reached the console: %d %q", inference, path, w.Code, w.Body.String())
			}
		}
	}
}
