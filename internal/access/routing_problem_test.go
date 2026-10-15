package access_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestRoutingValidationBecomesManagementProblem(t *testing.T) {
	response := httptest.NewRecorder()
	access.WriteProblem(response, fmt.Errorf("route validation: %w", &routing.ValidationError{Field: "strategy", Message: "Unknown routing strategy"}))
	var body struct {
		Status int                        `json:"status"`
		Detail string                     `json:"detail"`
		Errors map[string]json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 422 || body.Status != 422 || body.Detail != "Unknown routing strategy" || body.Errors["strategy"] == nil {
		t.Fatalf("routing error lost at management boundary: %d %s", response.Code, response.Body.String())
	}
}
