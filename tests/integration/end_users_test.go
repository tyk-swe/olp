//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestEndUserIdentityFromKeyPolicyThroughAccountingAndReports(t *testing.T) {
	h := newAccessHarness(t)
	fixture := newOpenAIFixture(t, "")
	owner, _, slug, _ := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"})
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{
		"name": "end users", "scopes": []string{"inference"}, "allowed_routes": []string{slug}, "end_user_source": "header",
	}, idem(uuid.NewString()), 201)
	path := "/api/v1/api-keys/" + key["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["end_user_source"] != "header" {
		t.Fatalf("key lost end-user policy: %v", detail)
	}
	const identifier = "private-customer-8432"
	digest := h.want(owner, "POST", path+"/end-user", map[string]any{"identifier": identifier}, nil, 200)["end_user_digest"].(string)
	h.want(owner, "POST", path+"/end-user", map[string]any{"identifier": "customer@example.com"}, nil, 422)
	h.refresh()
	finished := make(chan error, 1)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		finished <- err
		return err
	}}
	body, _ := json.Marshal(map[string]any{"model": slug, "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
	req, err := http.NewRequestWithContext(t.Context(), "POST", h.HTTP.URL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key["secret"].(string))
	req.Header.Set("X-OLP-End-User", identifier)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("gateway = %d %s", response.StatusCode, data)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request accounting did not finish")
	}
	window := "start=" + time.Now().UTC().Add(-time.Hour).Format(time.RFC3339) + "&end=" + time.Now().UTC().Add(time.Minute).Format(time.RFC3339)
	report := h.want(owner, "GET", "/api/v1/usage/breakdown?"+window+"&dimension=end_user", nil, nil, 200)
	items := report["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["dimension"] != digest {
		t.Fatalf("end-user report = %v", report)
	}
	for _, table := range []string{"requests", "attempt_usage_facts", "audit"} {
		var leaked bool
		if err := h.Pool.QueryRow(t.Context(), "SELECT EXISTS (SELECT 1 FROM olp."+table+" row WHERE row_to_json(row)::text LIKE $1)", "%"+identifier+"%").Scan(&leaked); err != nil || leaked {
			t.Fatalf("raw end-user identifier in %s: leaked=%v err=%v", table, leaked, err)
		}
	}
}
