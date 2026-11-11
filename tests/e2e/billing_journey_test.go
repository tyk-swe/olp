//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestBillingImportJourney(t *testing.T) {
	t.Parallel()
	s := startService(t)
	owner := s.owner()
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "billing"}, idempotent(), http.StatusCreated)["id"].(string)
	provider := s.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name":          "openai-billing",
		"configuration": map[string]any{"kind": "openai", "auth_mode": "api_key"},
		"credential":    "sk-provider-key-0123456789",
		"project_id":    project,
	}, idempotent(), http.StatusCreated)
	providerID := provider["id"].(string)
	path := "/api/v1/providers/" + providerID + "/billing-import"
	const key = "sk-admin-billing-0123456789"
	stable := func(document map[string]any) string {
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	s.want(owner, "GET", path, nil, nil, http.StatusNotFound)
	for field, body := range map[string]map[string]any{
		"credential":        {"scope": map[string]any{}},
		"scope.project_ids": {"credential": key, "scope": map[string]any{"project_ids": []string{"not a project"}}},
		"scope":             {"credential": key, "scope": map[string]any{"bucket": "olp-cur"}},
	} {
		problem := s.want(owner, "PUT", path, body, idempotent(etag(provider)), http.StatusUnprocessableEntity)
		if _, ok := problem["errors"].(map[string]any)[field]; !ok {
			t.Fatalf("creating an import with %v blamed %v, want %s", body, problem["errors"], field)
		}
	}
	for _, credential := range []any{nil, "sk-short"} {
		s.want(owner, "PUT", path, map[string]any{"credential": credential}, idempotent(etag(provider)), http.StatusUnprocessableEntity)
	}

	created := s.want(owner, "PUT", path, map[string]any{"credential": key, "scope": map[string]any{"project_ids": []string{"proj_b", "proj_a"}}},
		idempotent(etag(provider)), http.StatusCreated)
	createdETag := created["etag"]
	status := created["status"].(map[string]any)
	delete(created, "etag")
	delete(status, "next_import_at")
	if got, want := stable(created), `{"credential_configured":true,"enabled":true,"provider_id":"`+providerID+`","scope":{"project_ids":["proj_b","proj_a"]},"source":"openai_costs","status":{"imported_from":null,"imported_through":null,"last_error_code":null,"last_import_at":null,"last_success_at":null,"source_revision":null}}`; got != want {
		t.Fatalf("created import:\n%s\nwant\n%s", got, want)
	}
	created["etag"] = createdETag

	paused := s.want(owner, "PUT", path, map[string]any{"enabled": false}, idempotent(etag(created)), http.StatusOK)
	if paused["enabled"] != false || paused["credential_configured"] != true || stable(paused["scope"].(map[string]any)) != `{"project_ids":["proj_b","proj_a"]}` {
		t.Fatalf("pausing kept %v, want the stored credential and scope", paused)
	}
	queued := s.want(owner, "POST", path+"/run", nil, idempotent(), http.StatusAccepted)
	if queued["etag"] == paused["etag"] {
		t.Fatalf("queueing a run kept etag %v", queued["etag"])
	}

	day := func(offset int) string { return time.Now().UTC().AddDate(0, 0, offset).Format(time.DateOnly) }
	for _, statement := range []string{
		`INSERT INTO olp.provider_billing_days(provider_id,day,source,scope,last_success_at) SELECT $1::uuid,d::date,'openai_costs','{}',now() FROM unnest(ARRAY[$2,$3]::date[]) d`,
		`INSERT INTO olp.provider_invoiced_costs(provider_id,day,line_item,currency,amount,source) VALUES
			($1::uuid,$2::date,'gpt-5 input','USD',2.25,'openai_costs'),($1::uuid,$2::date,'gpt-5 output','USD',0.25,'openai_costs'),($1::uuid,$3::date,'gpt-5 input','EUR',1,'openai_costs')`,
	} {
		if _, err := s.pool.Exec(t.Context(), statement, providerID, day(-4), day(-3)); err != nil {
			t.Fatal(err)
		}
	}
	window := "?" + url.Values{"start": {day(-5)}, "end": {day(0)}}.Encode()
	invoiced := s.want(owner, "GET", "/api/v1/providers/"+providerID+"/invoiced-costs"+window, nil, nil, http.StatusOK)
	for _, item := range invoiced["items"].([]any) {
		delete(item.(map[string]any), "fetched_at")
	}
	if got, want := stable(invoiced), `{"items":[{"amount":"2.25","currency":"USD","day":"`+day(-4)+`","line_item":"gpt-5 input","source":"openai_costs"},{"amount":"0.25","currency":"USD","day":"`+day(-4)+`","line_item":"gpt-5 output","source":"openai_costs"},{"amount":"1","currency":"EUR","day":"`+day(-3)+`","line_item":"gpt-5 input","source":"openai_costs"}]}`; got != want {
		t.Fatalf("invoiced costs:\n%s\nwant\n%s", got, want)
	}
	report := s.want(owner, "GET", "/api/v1/usage/reconciliation"+window, nil, nil, http.StatusOK)
	if got, want := stable(report), `{"currency":"USD","end":"`+day(0)+`","items":[{"attributed_cost":"0","capture_rate":"0","capture_rate_unavailable_reason":null,"day":"`+day(-4)+`","excluded_caller_attempts":0,"import":{"last_error_code":null,"last_success_at":null},"incomplete_attempts":0,"invoiced_cost":"2.5","invoiced_currency":"USD","priced_attempts":0,"provider_id":"`+providerID+`","provisional":false,"unpriced_attempts":0,"unpriced_share":null},{"attributed_cost":"0","capture_rate":null,"capture_rate_unavailable_reason":"currency_mismatch","day":"`+day(-3)+`","excluded_caller_attempts":0,"import":{"last_error_code":null,"last_success_at":null},"incomplete_attempts":0,"invoiced_cost":"1","invoiced_currency":"EUR","priced_attempts":0,"provider_id":"`+providerID+`","provisional":false,"unpriced_attempts":0,"unpriced_share":null}],"start":"`+day(-5)+`","totals":[{"attributed_cost":"0","attributed_cost_covered":"0","capture_rate":null,"capture_rate_unavailable_reason":"currency_mismatch","invoiced_cost":"3.5","provider_id":"`+providerID+`","unpriced_share":null}]}`; got != want {
		t.Fatalf("reconciliation:\n%s\nwant\n%s", got, want)
	}

	s.want(owner, "DELETE", path, nil, etag(queued), http.StatusNoContent)
	s.want(owner, "GET", path, nil, nil, http.StatusNotFound)
}
