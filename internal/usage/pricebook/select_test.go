package pricebook

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func TestAttemptRoutingReadsThePricingPin(t *testing.T) {
	t.Parallel()
	revision := "018f3a5c-0000-7000-8000-00000000000a"
	pinned := "018f3a5c-0000-7000-8000-00000000000b"
	policy := func(raw string) *json.RawMessage {
		message := json.RawMessage(raw)
		return &message
	}
	cases := []struct {
		name       string
		routing    *usage.Routing
		wantPinned bool
		wantVendor string
		wantErr    bool
	}{
		{name: "an attempt without routing has no pin"},
		{name: "routing alone pins nothing",
			routing: &usage.Routing{ProviderRevisionID: revision}},
		{name: "a recorded pricing revision pins the catalogue",
			routing:    &usage.Routing{ProviderRevisionID: revision, PricingRevisionID: &pinned},
			wantPinned: true},
		{name: "a policy may pin pricing without naming a revision",
			routing: &usage.Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":true,"vendor_id":"google"}`)},
			wantPinned: true, wantVendor: "google"},
		{name: "an unpinned policy carries only its vendor",
			routing: &usage.Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":false,"vendor_id":"openai"}`)},
			wantVendor: "openai"},
		{name: "a malformed policy is not guessed at",
			routing: &usage.Routing{ProviderRevisionID: revision, Policy: policy(`{"pricing_pinned":`)},
			wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pin, err := attemptRouting(&usage.Attempt{Routing: test.routing})
			if test.wantErr {
				if !errors.Is(err, usage.ErrInvalidEvent) {
					t.Fatalf("error = %v, want ErrInvalidEvent", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("attemptRouting: %v", err)
			}
			if pin.pinned != test.wantPinned {
				t.Fatalf("pinned = %v, want %v", pin.pinned, test.wantPinned)
			}
			vendor := ""
			if pin.vendorID != nil {
				vendor = *pin.vendorID
			}
			if vendor != test.wantVendor {
				t.Fatalf("vendor = %q, want %q", vendor, test.wantVendor)
			}
			if test.routing == nil {
				if pin.providerRevisionID != nil {
					t.Fatalf("provider revision = %v, want nil", *pin.providerRevisionID)
				}
				return
			}
			if pin.providerRevisionID == nil || *pin.providerRevisionID != revision {
				t.Fatalf("provider revision = %v, want %s", pin.providerRevisionID, revision)
			}
		})
	}
}

func TestMediaQuoteAndSettlementUseTheirOwnAmountLimits(t *testing.T) {
	t.Parallel()
	in := pricingInputs{
		priceDoc:    []byte(`{"unit_price":2}`),
		explicitDoc: []byte(`{"unit_price":3}`),
	}
	u := usage.AttemptUsage{
		Evidence:        usage.EvidenceObservedComplete,
		UsageQuantities: usage.UsageQuantities{MediaUnits: new("1000000000000")},
	}
	maximum := rates.MaxCost().String()
	for _, tc := range []struct {
		name               string
		forStorage         bool
		wantCost, wantBill string
		wantLines          bool
	}{
		{"quote", false, "2000000000000", "3000000000000", true},
		{"settlement", true, maximum, maximum, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := evaluatePricingInputs(in, u, time.Time{}, true, tc.forStorage)
			if err != nil || !p.Complete {
				t.Fatalf("complete = %v, error = %v", p.Complete, err)
			}
			for field, amount := range map[string]*string{"list cost": p.ListCost, "cost": p.EstimatedCost, "contracted cost": p.ContractedCost} {
				if amount == nil || *amount != tc.wantCost {
					t.Errorf("%s = %v, want %s", field, amount, tc.wantCost)
				}
			}
			if p.Charge == nil || *p.Charge != tc.wantBill {
				t.Errorf("charge = %v, want %s", p.Charge, tc.wantBill)
			}
			if (len(p.CostLines) > 0) != tc.wantLines || (len(p.ChargeLines) > 0) != tc.wantLines {
				t.Errorf("cost lines = %s, charge lines = %s, want lines = %v", p.CostLines, p.ChargeLines, tc.wantLines)
			}
		})
	}
}
