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
	h.want(owner, http.MethodDelete, "/api/v1/projects/"+project, nil, map[string]string{"If-Match": projectEtag(h, owner, project)["If-Match"], "Idempotency-Key": "delete-investigation-project"}, http.StatusNoContent)
	h.want(owner, http.MethodGet, "/api/v1/investigations/"+id, nil, nil, http.StatusNotFound)
}

func TestInvestigationsRejectInvalidFiltersAndReferences(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Investigation validation")
	cases := []struct {
		name     string
		filters  map[string]any
		evidence []any
	}{
		{"invalid time", map[string]any{"from": "yesterday"}, []any{}},
		{"inverted time", map[string]any{"from": "2026-06-01T00:00:00Z", "to": "2026-05-01T00:00:00Z"}, []any{}},
		{"inverted zero time", map[string]any{"from": "2026-01-01T00:00:00Z", "to": "0001-01-01T00:00:00Z"}, []any{}},
		{"negative duration", map[string]any{"min_duration_ms": -1}, []any{}},
		{"inverted duration", map[string]any{"min_duration_ms": 10, "max_duration_ms": 5}, []any{}},
		{"empty tool", map[string]any{"tool": " "}, []any{}},
		{"invalid run", map[string]any{"run_id": "not-a-uuid"}, []any{}},
		{"unknown evidence", map[string]any{}, []any{map[string]any{"kind": "payload", "id": "00000000-0000-0000-0000-000000000001"}}},
		{"invalid reference", map[string]any{}, []any{map[string]any{"kind": "run", "id": "invalid"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _ := h.request(owner, "POST", "/api/v1/investigations", map[string]any{"project_id": project, "filters": tc.filters, "evidence": tc.evidence, "summary": "Review"}, idem("investigation-"+tc.name))
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status %d, want %d; problem: %v", status, http.StatusUnprocessableEntity, body["detail"])
			}
		})
	}
}
