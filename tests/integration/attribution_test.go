//go:build integration

package integration_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

func attributionCall(t *testing.T, h *accessHarness, model, key string, headers []string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for _, header := range headers {
		req.Header.Add(usage.AttributionHeader, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func attributionSeed(t *testing.T, f *repFixture, keyID string, observed time.Time, attribution string) {
	t.Helper()
	requestID := access.NewID()
	f.anchor(requestID, observed)
	f.exec(`INSERT INTO olp.attempt_usage_facts (attempt_id, event_id, request_id, request_started_at,
	        attempt_ordinal, api_key_id, provider_id, route_slug, upstream_model, operation, surface,
	        observed_at, charge_status, usage_observed, usage_complete, input_tokens, output_tokens,
	        unpriced, request_counted, provider_request_counted, model_request_counted,
	        target_request_counted, request_unpriced_counted, provider_unpriced_counted,
	        model_unpriced_counted, target_unpriced_counted, request_incomplete_counted,
	        provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted,
	        attribution)
	    VALUES ($1, $2, $3, $4, 1, $5, $6, 'alpha', 'm1', 'generation', 'openai',
	        $7, 'billable', true, true, 10, 5, false, true, true, true, true,
	        false, false, false, false, false, false, false, false, $8::jsonb)`,
		access.NewID(), access.NewID(), requestID, observed, keyID, f.P1, observed, attribution)
}

func TestAttributionReporting(t *testing.T) {
	f := repSetup(t)
	h := f.h
	owner := f.Owner
	now := time.Now().UTC()
	start := now.Add(-time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)

	project := h.want(owner, "POST", "/api/v1/projects",
		map[string]any{"name": "Labelled"}, map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectID := project["id"].(string)
	projectKey := h.want(owner, "POST", "/api/v1/api-keys",
		map[string]any{"name": "project key", "scopes": []string{"inference"}, "project_id": projectID},
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	projectKeyID := projectKey["id"].(string)

	attributionSeed(t, f, projectKeyID, now, `{"team":"core","env":"prod"}`)
	attributionSeed(t, f, projectKeyID, now, `{"team":"core","env":"stage"}`)
	attributionSeed(t, f, projectKeyID, now, `{"team":"edge"}`)
	attributionSeed(t, f, f.Key, now, `{"team":"core"}`)

	summary := func(query string) map[string]any {
		return h.want(owner, "GET", "/api/v1/usage/summary?start="+start+"&end="+end+query, nil, nil, 200)
	}

	filtered := summary("&attribution_key=team&attribution_value=core")
	if filtered["request_count"].(float64) != 3 {
		t.Fatalf("team=core summary = %v", filtered)
	}
	filtered = summary("&attribution_key=team&attribution_value=edge")
	if filtered["request_count"].(float64) != 1 {
		t.Fatalf("team=edge summary = %v", filtered)
	}

	filtered = summary("&attribution_key=env")
	if filtered["request_count"].(float64) != 2 {
		t.Fatalf("env summary = %v", filtered)
	}

	breakdown := h.want(owner, "GET",
		"/api/v1/usage/breakdown?start="+start+"&end="+end+"&dimension=attribution&attribution_key=team",
		nil, nil, 200)
	items := breakdown["items"].([]any)
	counts := map[string]float64{}
	for _, raw := range items {
		row := raw.(map[string]any)
		counts[row["dimension"].(string)] = row["request_count"].(float64)
	}
	if counts["core"] != 3 || counts["edge"] != 1 || len(counts) != 2 {
		t.Fatalf("team breakdown = %v", counts)
	}

	status, _, _ := h.request(owner, "GET",
		"/api/v1/usage/breakdown?start="+start+"&end="+end+"&dimension=attribution", nil, nil)
	if status != 400 {
		t.Fatalf("attribution breakdown without key = %d", status)
	}
	status, _, _ = h.request(owner, "GET",
		"/api/v1/usage/summary?start="+start+"&end="+end+"&attribution_value=core", nil, nil)
	if status != 400 {
		t.Fatalf("value without key = %d", status)
	}

	requestID := access.NewID()
	f.request(repRequest{ID: requestID, StartedAt: now, Route: "alpha",
		Operation: "generation", Surface: "openai", AttemptCount: 1})
	f.exec("UPDATE olp.requests SET attribution=$2::jsonb WHERE id=$1",
		requestID, `{"team":"core"}`)
	list := h.want(owner, "GET", "/api/v1/requests", nil, nil, 200)
	var row map[string]any
	for _, raw := range list["items"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["id"] == requestID {
			row = candidate
		}
	}
	if row == nil || row["attribution"].(map[string]any)["team"] != "core" {
		t.Fatalf("request labels = %v", row)
	}

	viewer := h.invite(owner, "viewer@example.com", "viewer")
	viewerID := h.want(owner, "GET", "/api/v1/users", nil, nil, 200)["items"].([]any)
	var memberID string
	for _, raw := range viewerID {
		u := raw.(map[string]any)
		if u["email"] == "viewer@example.com" {
			memberID = u["id"].(string)
		}
	}
	user := h.want(owner, "GET", "/api/v1/users/"+memberID, nil, nil, 200)
	h.want(owner, "PATCH", "/api/v1/users/"+memberID,
		map[string]any{"access_scope": "assigned"}, withMatch(user, nil), 200)
	h.want(owner, "PUT", "/api/v1/projects/"+projectID+"/members/"+memberID,
		map[string]any{"role": "viewer"}, projectEtag(h, owner, projectID), 200)
	h.want(viewer, "POST", "/api/v1/sessions",
		map[string]any{"email": "viewer@example.com", "password": accessPassword}, nil, 201)
	scoped := h.want(viewer, "GET",
		"/api/v1/usage/summary?start="+start+"&end="+end+"&attribution_key=team&attribution_value=core",
		nil, nil, 200)
	if scoped["request_count"].(float64) != 2 {
		t.Fatalf("scoped summary = %v (global key rows must be invisible)", scoped)
	}
}
