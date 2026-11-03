//go:build integration

package integration_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
)

func TestLimitTemplatesPropagateToKeysUsersAndGroups(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Templates"}, idem("template-project"), 201)
	id := project["id"].(string)
	t.Run("effective limits agree with reporting", func(t *testing.T) {
		intPointer := func(value int64) *int64 { return &value }
		amountPointer := func(value string) *string { return &value }
		cases := []struct {
			name     string
			local    access.AdmissionLimits
			template access.AdmissionLimits
		}{
			{name: "unlimited"},
			{name: "local", local: access.AdmissionLimits{
				RequestsPerMinute: intPointer(3), TokensPerMinute: intPointer(4), MaxConcurrency: intPointer(5),
				DailyCostLimit: amountPointer("6.25"), WeeklyCostLimit: amountPointer("7.5"), MonthlyCostLimit: amountPointer("8"), CostBasis: "charge",
			}},
			{name: "template", template: access.AdmissionLimits{
				RequestsPerMinute: intPointer(3), TokensPerMinute: intPointer(4), MaxConcurrency: intPointer(5),
				DailyCostLimit: amountPointer("6.25"), WeeklyCostLimit: amountPointer("7.5"), MonthlyCostLimit: amountPointer("8"), CostBasis: "charge",
			}},
			{name: "intersect", local: access.AdmissionLimits{
				RequestsPerMinute: intPointer(2), TokensPerMinute: intPointer(5), MaxConcurrency: intPointer(7),
				DailyCostLimit: amountPointer("2.5"), WeeklyCostLimit: amountPointer("6"), MonthlyCostLimit: amountPointer("9.01"), CostBasis: "cost",
			}, template: access.AdmissionLimits{
				RequestsPerMinute: intPointer(3), TokensPerMinute: intPointer(4), MaxConcurrency: intPointer(7),
				DailyCostLimit: amountPointer("3"), WeeklyCostLimit: amountPointer("4.25"), MonthlyCostLimit: amountPointer("9.010"), CostBasis: "charge",
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				name := "template"
				authority := access.Authority{ProjectID: &id, Policy: access.KeyPolicy{
					LimitTemplate: &name, RequestsPerMinute: tc.local.RequestsPerMinute, TokensPerMinute: tc.local.TokensPerMinute,
					MaxConcurrency: tc.local.MaxConcurrency, DailyCostLimit: tc.local.DailyCostLimit,
					WeeklyCostLimit: tc.local.WeeklyCostLimit, MonthlyCostLimit: tc.local.MonthlyCostLimit, CostBasis: tc.local.CostBasis,
				}}
				if err := authority.BindLimitTemplates(access.LimitTemplates{name: tc.template}); err != nil {
					t.Fatal(err)
				}
				localJSON, err := json.Marshal(tc.local)
				if err != nil {
					t.Fatal(err)
				}
				templateJSON, err := json.Marshal(tc.template)
				if err != nil {
					t.Fatal(err)
				}
				var result []byte
				if err := h.Pool.QueryRow(t.Context(), "SELECT olp.effective_limits($1::jsonb, $2::jsonb)", string(localJSON), string(templateJSON)).Scan(&result); err != nil {
					t.Fatal(err)
				}
				var reported access.AdmissionLimits
				if err := json.Unmarshal(result, &reported); err != nil {
					t.Fatal(err)
				}
				effective := authority.Policy.AdmissionLimits()
				for _, field := range []struct {
					name               string
					admitted, reported *int64
				}{
					{"requests", effective.RequestsPerMinute, reported.RequestsPerMinute},
					{"tokens", effective.TokensPerMinute, reported.TokensPerMinute},
					{"concurrency", effective.MaxConcurrency, reported.MaxConcurrency},
				} {
					if (field.admitted == nil) != (field.reported == nil) {
						t.Errorf("%s limit is present in only one path", field.name)
						continue
					}
					if field.admitted != nil && *field.admitted != *field.reported {
						t.Errorf("%s admission = %d, reporting = %d", field.name, *field.admitted, *field.reported)
					}
				}
				for _, field := range []struct {
					name               string
					admitted, reported *string
				}{
					{"daily", effective.DailyCostLimit, reported.DailyCostLimit},
					{"weekly", effective.WeeklyCostLimit, reported.WeeklyCostLimit},
					{"monthly", effective.MonthlyCostLimit, reported.MonthlyCostLimit},
				} {
					if (field.admitted == nil) != (field.reported == nil) {
						t.Errorf("%s limit is present in only one path", field.name)
						continue
					}
					if field.admitted == nil {
						continue
					}
					admittedAmount, admittedErr := decimal.NewFromString(*field.admitted)
					reportedAmount, reportedErr := decimal.NewFromString(*field.reported)
					if admittedErr != nil || reportedErr != nil || !admittedAmount.Equal(reportedAmount) {
						t.Errorf("%s admission = %q, reporting = %q", field.name, *field.admitted, *field.reported)
					}
				}
				if limits.Basis(effective.CostBasis).Pick("cost", "charge") != limits.Basis(reported.CostBasis).Pick("cost", "charge") {
					t.Errorf("basis admission = %q, reporting = %q", effective.CostBasis, reported.CostBasis)
				}
			})
		}
	})
	path := "/api/v1/projects/" + id + "/limit-templates"
	templates := map[string]any{"key": map[string]any{"requests_per_minute": 2}, "customer": map[string]any{"requests_per_minute": 1}, "group": map[string]any{"requests_per_minute": 2}}
	put := func(want int) {
		t.Helper()
		old := h.want(owner, "GET", path, nil, nil, 200)
		h.want(owner, "PUT", path, map[string]any{"templates": templates}, etagHeader(old), want)
	}
	put(200)
	fixture := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Template provider", fixture.URL, &id, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	slug := "template-chat"
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": id, "slug": slug, "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("template-route"), 201)
	dp := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", dp+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", dp+"/activate", nil, withMatch(validated, idem("activate-template-route")), 200)
	client, _, limiter := limSetup(t, "templates")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = h.Gateway.Admission
	key := func(name string, extra map[string]any) string {
		t.Helper()
		in := map[string]any{"name": name, "project_id": id, "scopes": []string{"inference"}, "allowed_routes": []string{slug}}
		for k, v := range extra {
			in[k] = v
		}
		made := h.want(owner, "POST", "/api/v1/api-keys", in, idem(name), 201)
		h.refresh()
		replica.refresh()
		return made["secret"].(string)
	}
	call := func(server *accessHarness, secret, user string, want int) {
		t.Helper()
		status, err := endUserChat(t.Context(), server, secret, slug, user)
		if err != nil || status != want {
			t.Fatalf("status=%d want=%d err=%v", status, want, err)
		}
	}
	a := key("key-a", map[string]any{"limit_template": "key", "requests_per_minute": 100})
	b := key("key-b", map[string]any{"limit_template": "key"})
	limSettleInMinute(t, client, 10*time.Second)
	call(h, a, "", 200)
	call(replica, a, "", 200)
	call(h, a, "", 429)
	call(h, b, "", 200)
	templates["key"] = map[string]any{"requests_per_minute": 3}
	put(200)
	h.refresh()
	replica.refresh()
	call(replica, a, "", 200)
	call(h, a, "", 429)
	// The inventory reports the template's rate for a key that leaves it unset.
	listedB := false
	for _, item := range h.want(owner, "GET", "/api/v1/api-keys", nil, nil, 200)["items"].([]any) {
		listed := item.(map[string]any)
		if listed["name"] != "key-b" {
			continue
		}
		if listed["requests_per_minute"] != nil || listed["effective_limits"].(map[string]any)["requests_per_minute"] != float64(3) {
			t.Fatalf("templated key reported rate %v, effective %v", listed["requests_per_minute"], listed["effective_limits"])
		}
		listedB = true
	}
	if !listedB {
		t.Fatal("key-b is missing from the inventory")
	}
	identified := key("identified", map[string]any{"end_user_source": "native", "end_user_policy": map[string]any{"limit_template": "customer"}})
	call(h, identified, "alice", 200)
	call(replica, identified, "alice", 429)
	call(h, identified, "bob", 200)
	group := h.want(owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Template group", "project_id": id, "limit_template": "group"}, idem("template-group"), 201)
	c := key("group-c", map[string]any{"budget_group_id": group["id"]})
	d := key("group-d", map[string]any{"budget_group_id": group["id"]})
	call(h, c, "", 200)
	call(replica, d, "", 200)
	call(h, c, "", 429)
	templates["group"] = map[string]any{"requests_per_minute": 3, "daily_cost_limit": "4"}
	put(200)
	h.refresh()
	replica.refresh()
	detail := h.want(owner, "GET", "/api/v1/budget-groups/"+group["id"].(string), nil, nil, 200)
	if detail["budget"].(map[string]any)["daily"].(map[string]any)["limit"] != "4" {
		t.Fatalf("effective group reporting=%v", detail)
	}
	delete(templates, "key")
	put(409)
	// Revoked keys no longer hold their template, and the authority still loads without it.
	for _, item := range h.want(owner, "GET", "/api/v1/api-keys", nil, nil, 200)["items"].([]any) {
		listed := item.(map[string]any)
		if listed["limit_template"] != "key" {
			continue
		}
		keyPath := "/api/v1/api-keys/" + listed["id"].(string)
		h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(h.want(owner, "GET", keyPath, nil, nil, 200), idem(uuid.NewString())), 200)
	}
	put(200)
	h.refresh()
	// Unknown and cross-project references cannot be saved.
	h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Bad template", "scopes": []string{"inference"}, "allowed_routes": []string{}, "limit_template": "key"}, idem("unassigned-template"), 422)
}
