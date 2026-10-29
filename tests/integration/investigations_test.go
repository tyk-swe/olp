//go:build integration

package integration_test

import (
	"net/http"
	"reflect"
	"testing"
)

func TestInvestigationsSaveTypedFiltersAndSummary(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Investigation project")
	filters := map[string]any{
		"from":            "2026-01-01T00:00:00Z",
		"to":              "2026-01-02T00:00:00Z",
		"min_duration_ms": float64(100),
		"max_duration_ms": float64(1000),
		"error":           true,
		"tool":            "lookup",
	}
	input := map[string]any{"project_id": project, "filters": filters, "evidence": []any{}, "summary": "The tool timed out"}
	created := h.want(owner, http.MethodPost, "/api/v1/investigations", input, idem("investigation"), http.StatusCreated)
	id := mcpExecString(t, created, "id")
	read := h.want(owner, http.MethodGet, "/api/v1/investigations/"+id, nil, nil, http.StatusOK)
	if read["project_id"] != project || read["summary"] != input["summary"] || !reflect.DeepEqual(read["filters"], filters) || len(mcpExecItems(t, read, "evidence")) != 0 {
		t.Fatalf("saved investigation differs from the submitted metadata: %v", read)
	}
	listed := h.want(owner, http.MethodGet, "/api/v1/investigations?project_id="+project, nil, nil, http.StatusOK)
	if items := mcpExecItems(t, listed, "items"); len(items) != 1 || mcpExecString(t, mcpExecItem(t, items, 0, listed), "id") != id {
		t.Fatalf("saved investigation is not listed: %v", listed)
	}
}
