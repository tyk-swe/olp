package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestCodeEndUserPoliciesExcludeCostWithoutMutatingAuthority(t *testing.T) {
	digest, budget := strings.Repeat("a", 64), "1"
	policy := &access.EndUserPolicy{
		Defaults:  access.AdmissionLimits{DailyCostLimit: &budget},
		Overrides: map[string]access.AdmissionLimits{digest: {MonthlyCostLimit: &budget}},
	}
	authority := access.Authority{ID: "11111111-1111-4111-8111-111111111111", EndUserDigest: digest, Policy: access.KeyPolicy{EndUserPolicy: policy}}
	authority.Policy.RouteLimits = access.RouteLimits{"coding": {DailyCostLimit: &budget}}
	authority.InstallationBudget = &access.BudgetPolicy{DailyCostLimit: &budget}
	authority.ProjectBudget = &access.BudgetPolicy{MonthlyCostLimit: &budget}
	var admission *Admission
	lease, err := admission.ReserveCodeRate(t.Context(), authority, 1, time.Minute, "coding")
	if err != nil || lease != nil {
		t.Fatalf("subscription dollar budget: %v %v", lease, err)
	}
	if authority.Policy.RouteLimits["coding"].DailyCostLimit == nil || authority.InstallationBudget.DailyCostLimit == nil || authority.ProjectBudget.MonthlyCostLimit == nil || policy.Defaults.DailyCostLimit == nil || policy.Overrides[digest].MonthlyCostLimit == nil {
		t.Fatal("cached end-user policy was mutated")
	}
	policy.Blocked = []string{digest}
	if _, err = admission.ReserveCodeRate(t.Context(), authority, 1, time.Minute, "coding"); err == nil || err.Code != "end_user_blocked" {
		t.Fatalf("code end-user block was lost: %v", err)
	}
}

type endUserCodeLedger struct {
	*codeTestLedger
	authority access.Authority
}

func (l endUserCodeLedger) Admit(ctx context.Context, in resources.CodeAdmission) (resources.CodePermit, error) {
	permit, err := l.codeTestLedger.Admit(ctx, in)
	permit.Authority = l.authority
	return permit, err
}

func TestCodeNativeIdentitySurvivesBodyParsingAndFreshPolicy(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip"} {
		t.Run(encoding, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			source := "native"
			authority := h.rt.keys[fullKey]
			authority.Policy.EndUserSource = &source
			h.rt.keys[fullKey] = authority
			h.gateway.Runtime = endUserRuntime{h.rt}
			// A ledger policy newer than the runtime cache forces a second admission.
			authority.Policy.ResponseMetadata = true
			h.gateway.CodeLedger = endUserCodeLedger{ledger, authority}
			body := []byte(`{"model":"native-model","user":"code-user","input":"hi","stream":true}`)
			if encoding == "gzip" {
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				body = compressed.Bytes()
			}

			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, body) || r.Header.Get(endUserHeader) != "" || r.Header.Get("Content-Encoding") != encoding {
					t.Error("code forwarding changed bytes or leaked the local header")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_identity\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			})
			response := codeDo(t, server, body, http.Header{"Content-Encoding": []string{encoding}, "Content-Type": []string{"application/json"}, endUserHeader: []string{"not-the-source"}})
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("code identity: status %d", response.StatusCode)
			}
			if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 2 {
				t.Fatalf("code accounting: %+v", usage)
			}
			ledger.mu.Lock()
			digest := ledger.inputs[0].EndUserDigest
			ledger.mu.Unlock()
			if digest != (endUserRuntime{h.rt}).EndUserDigest(authority.ProjectID, "code-user") {
				t.Fatal("code admission lost the native digest")
			}
		})
	}
}

func TestCodeIdentityChangeDuringAdmissionRefusesBeforeDispatch(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	native, header := "native", "header"
	authority := h.rt.keys[fullKey]
	authority.Policy.EndUserSource = &native
	h.rt.keys[fullKey] = authority
	h.gateway.Runtime = endUserRuntime{h.rt}
	authority.Policy.EndUserSource = &header
	h.gateway.CodeLedger = endUserCodeLedger{ledger, authority}
	response := codeDo(t, server, []byte(`{"model":"native-model","user":"native-user","input":"hi","stream":true}`), http.Header{"Content-Type": {"application/json"}, endUserHeader: {"header-user"}})
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("code_end_user_changed")) {
		t.Fatalf("identity change: %d %s", response.StatusCode, body)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.marks) != 0 || len(ledger.aborts) != 1 || h.mock.count("a") != 0 {
		t.Fatal("changed identity dispatched or retained a prepared attempt")
	}
}

func TestSubscriptionWeeklyDollarExemptionsPreserveAuthority(t *testing.T) {
	cost := "1"
	group := "0d9d6a8b-0d2e-4a01-b1a0-5948b5a72c36"
	a := access.Authority{ProjectAttributionBudgets: access.AttributionBudgets{"team": {"core": {WeeklyCostLimit: &cost}}}, Attribution: map[string]string{"team": "core"}, ID: group, BudgetGroupID: &group, BudgetGroupWeeklyCostLimit: &cost, EndUserDigest: strings.Repeat("a", 64), InstallationBudget: &access.BudgetPolicy{WeeklyCostLimit: &cost}, ProjectBudget: &access.BudgetPolicy{WeeklyCostLimit: &cost}, Policy: access.KeyPolicy{WeeklyCostLimit: &cost, EndUserPolicy: &access.EndUserPolicy{Defaults: access.AdmissionLimits{WeeklyCostLimit: &cost}}, RouteLimits: access.RouteLimits{"coding": {WeeklyCostLimit: &cost}}}}
	var admission *Admission
	if lease, e := admission.ReserveCodeRate(t.Context(), a, 1, time.Minute, "coding"); e != nil || lease != nil {
		t.Fatalf("subscription required invoice admission: %v", e)
	}
	if a.ProjectAttributionBudgets["team"]["core"].WeeklyCostLimit == nil || a.Policy.WeeklyCostLimit == nil || a.BudgetGroupWeeklyCostLimit == nil || a.Policy.EndUserPolicy.Defaults.WeeklyCostLimit == nil || a.Policy.RouteLimits["coding"].WeeklyCostLimit == nil || a.InstallationBudget.WeeklyCostLimit == nil || a.ProjectBudget.WeeklyCostLimit == nil {
		t.Fatal("subscription admission mutated cached budgets")
	}
}
