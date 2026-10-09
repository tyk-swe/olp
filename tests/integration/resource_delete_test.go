//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestConditionalBudgetAndNotificationDeletionRetainsDependencies(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	projectID := createProject(h, owner, "Managed resources")
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Managed budget", "project_id": projectID, "monthly_cost_limit": "10"}, idem(uuid.NewString()), http.StatusCreated)
	destination := h.want(owner, "POST", "/api/v1/notifications/destinations", map[string]any{"name": "Managed destination", "project_id": projectID, "url": "https://notifications.example.com/budget", "secret": "private-fixture-signature", "enabled": false}, idem(uuid.NewString()), http.StatusCreated)
	rule := h.want(owner, "POST", "/api/v1/notifications/rules", map[string]any{"name": "Managed rule", "event": "budget.threshold", "project_id": projectID, "subject_kind": "budget_group", "subject_id": group["id"], "window_kind": "month", "threshold_percent": 80, "destination_id": destination["id"], "enabled": false}, idem(uuid.NewString()), http.StatusCreated)
	groupPath := "/api/v1/budget-groups/" + group["id"].(string)
	destinationPath := "/api/v1/notifications/destinations/" + destination["id"].(string)
	rulePath := "/api/v1/notifications/rules/" + rule["id"].(string)
	headers := func(path string) map[string]string {
		observed := h.want(owner, "GET", path, nil, nil, 200)
		result := etagHeader(observed)
		result["Idempotency-Key"] = uuid.NewString()
		return result
	}
	h.want(owner, "DELETE", groupPath, nil, headers(groupPath), 409)
	h.want(owner, "DELETE", destinationPath, nil, headers(destinationPath), 409)
	h.want(owner, "DELETE", rulePath, nil, idem(uuid.NewString()), 428)
	h.want(owner, "DELETE", rulePath, nil, map[string]string{"If-Match": "\"" + uuid.NewString() + "\"", "Idempotency-Key": uuid.NewString()}, 412)
	deleteHeaders := headers(rulePath)
	h.want(owner, "DELETE", rulePath, nil, deleteHeaders, 204)
	h.want(owner, "DELETE", rulePath, nil, deleteHeaders, 204)
	h.want(owner, "GET", rulePath, nil, nil, 404)
	var secretID string
	if err := h.Pool.QueryRow(t.Context(), "SELECT secret_id::text FROM olp.notification_destinations WHERE id=$1", destination["id"]).Scan(&secretID); err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", destinationPath, nil, headers(destinationPath), 204)
	var present bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.secrets WHERE id=$1)", secretID).Scan(&present); err != nil || present {
		t.Fatalf("removed destination signing material retained: %v", err)
	}
	h.want(owner, "DELETE", groupPath, nil, headers(groupPath), 204)
	h.want(owner, "GET", groupPath, nil, nil, 404)
	retained := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Retained budget", "project_id": projectID, "monthly_cost_limit": "10"}, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Retained key", "project_id": projectID, "budget_group_id": retained["id"]}, idem(uuid.NewString()), 201)
	h.want(owner, "DELETE", "/api/v1/budget-groups/"+retained["id"].(string), nil, headers("/api/v1/budget-groups/"+retained["id"].(string)), 409)
	var audited int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit WHERE action IN ('budget_group.delete','notification_destination.delete','notification_rule.delete')").Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("audit count=%d: %v", audited, err)
	}
}
