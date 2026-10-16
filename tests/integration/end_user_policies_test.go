//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/usage"
)

func endUserChat(ctx context.Context, h *accessHarness, secret, slug, identifier string) (int, error) {
	return endUserChatMode(ctx, h, secret, slug, identifier, false)
}

func endUserChatMode(ctx context.Context, h *accessHarness, secret, slug, identifier string, stream bool) (int, error) {
	body, err := json.Marshal(map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "user": identifier, "max_tokens": 16, "stream": stream})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", h.HTTP.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

// Complete overrides can remove a default budget, or replace a rate independently.

// Reconciliation and duplicate delivery must preserve the same balance.

func TestProjectEndUserPoliciesEnforceBothBoundaries(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "End users"}, idem("project"), 201)
	projectID := project["id"].(string)
	path := "/api/v1/projects/" + projectID + "/end-user-policy"
	initial := h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "PUT", path, map[string]any{}, etagHeader(initial), 422)
	h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 3}}}, etagHeader(initial), 200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Scoped provider", fixture.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"project_id": projectID, "slug": "end-user-project", "overall_timeout_ms": 10000, "max_attempts": 1,
		"fidelity": map[string]any{"mode": "transformed"},
		"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("draft"), 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", draftPath+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", draftPath+"/activate", nil, withMatch(validated, idem("activate")), 200)
	secrets := make([]string, 2)
	for i := range secrets {
		key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": fmt.Sprintf("key-%d", i), "project_id": projectID, "scopes": []string{"inference"}, "allowed_routes": []string{"end-user-project"}, "end_user_source": "native", "end_user_policy": map[string]any{"defaults": map[string]any{"requests_per_minute": 2}}}, idem(fmt.Sprintf("key-%d", i)), 201)
		secrets[i] = key["secret"].(string)
	}
	c, namespace, limiter := limSetup(t, "project-end-users")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	h.refresh()
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limLimiter(t, limClient(t), namespace), nil, slog.New(slog.DiscardHandler))
	replica.refresh()
	limSettleInMinute(t, c, 10*time.Second)
	for _, step := range []struct {
		key, status int
		user        string
	}{
		{0, 200, "shared"}, {0, 200, "shared"}, {0, 429, "shared"},
		// The key refusal refunds its project reservation, leaving one request.
		{1, 200, "shared"}, {1, 429, "shared"}, {1, 200, "other"},
	} {
		if status, err := endUserChat(t.Context(), replica, secrets[step.key], "end-user-project", step.user); status != step.status || err != nil {
			t.Fatalf("step %+v: %d %v", step, status, err)
		}
	}
	t.Run("cost is shared across keys", func(t *testing.T) {
		h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano),
			"prices":       []any{map[string]any{"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel, "operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000"}},
		}, idem("project-price"), 201)
		setBudget := func(amount string) {
			detail := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "PUT", path, map[string]any{"policy": map[string]any{"defaults": map[string]any{"daily_cost_limit": amount}}}, etagHeader(detail), 200)
			h.refresh()
			replica.refresh()
		}
		setBudget("1000")
		eventually(t, "project price publication", 15*time.Second, func() bool {
			h.refresh()
			replica.refresh()
			inputs := replica.Runtime.RoutingInputs()
			return inputs != nil && len(inputs.Prices) > 0
		})
		limiter := limLimiter(t, c, namespace)
		await := endUserAccounting(t, h, limiter)
		replica.Gateway.Sink = h.Gateway.Sink
		for _, step := range []struct{ key, status int }{{0, 503}, {1, 200}} {
			if status, err := endUserChat(t.Context(), replica, secrets[step.key], "end-user-project", "priced-user"); status != step.status || err != nil {
				t.Fatalf("shared budget admission: %d %v", status, err)
			}
			await()
		}
		var accrued, digest string
		if err := h.Pool.QueryRow(t.Context(), `SELECT w.accrued::text,a.end_user_digest FROM olp.end_user_cost_windows w JOIN olp.end_user_accounts a ON a.id=w.account_id WHERE a.project_id=$1 AND w.window_kind='day'`, projectID).Scan(&accrued, &digest); err != nil {
			t.Fatal(err)
		}
		if accrued != "2.000000000000" {
			t.Fatalf("project cost=%s, want two billed fixture tokens", accrued)
		}
		setBudget(accrued)
		if status, err := endUserChat(t.Context(), h, secrets[0], "end-user-project", "priced-user"); status != 429 || err != nil {
			t.Fatalf("spend on another key bypassed project budget: %d %v", status, err)
		}
		await()
		conn, err := h.Pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		snapshots, err := limits.ReconciliationSnapshots(t.Context(), conn.Conn(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, snapshot := range snapshots {
			if snapshot.CostOwnerID == limits.EndUserProjectID(projectID, digest) {
				found = true
				if snapshot.DailyAccrued != accrued {
					t.Fatalf("project reconciliation: %+v", snapshot)
				}
			}
		}
		if !found {
			t.Fatal("project identity missing from reconciliation")
		}
	})

}

func endUserAccounting(t *testing.T, h *accessHarness, limiter *limits.Limiter) func() {
	t.Helper()
	finished := make(chan error, 16)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		persisted, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		if err == nil {
			for _, snapshot := range persisted.CostSnapshots {
				if _, _, err = limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
					break
				}
			}
		}
		finished <- err
		return err
	}}
	return func() {
		t.Helper()
		select {
		case err := <-finished:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("accounting did not complete")
		}
	}
}
