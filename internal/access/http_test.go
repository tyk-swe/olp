package access

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
)

type managementRecorder struct {
	*httptest.ResponseRecorder
}

func (managementRecorder) SetReadDeadline(time.Time) error { return nil }

func TestManagementNativeJSONFitsPlaygroundEnvelope(t *testing.T) {
	t.Parallel()
	native := `{"text":"` + strings.Repeat("&", 3<<20) + `"}`
	s := &Server{}
	handler := s.serve(routeOptions{maxBody: 65536, timeout: time.Second}, func(*http.Request) (httpkit.Reply, error) {
		return httpkit.Reply{
			Status:              http.StatusOK,
			Body:                map[string]any{"response": json.RawMessage(native), "response_raw": native},
			DisableHTMLEscaping: true,
		}, nil
	})
	res := managementRecorder{httptest.NewRecorder()}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/playground", nil)
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected response: %d %s", res.Code, res.Header().Get("Content-Type"))
	}
	if size := res.Body.Len(); size > 32<<20 {
		t.Fatalf("management response exceeds the Playground envelope limit: %d bytes", size)
	}
	var response struct {
		Parsed json.RawMessage `json:"response"`
		Raw    string          `json:"response_raw"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if string(response.Parsed) != native || response.Raw != native {
		t.Fatal("management response changed native JSON")
	}
}
