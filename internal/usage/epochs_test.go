package usage

import (
	"encoding/json"
	"errors"
	"testing"
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
		routing    *Routing
		wantPinned bool
		wantVendor string
		wantErr    bool
	}{
		{name: "an attempt without routing has no pin"},
		{name: "routing alone pins nothing",
			routing: &Routing{ProviderRevisionID: revision}},
		{name: "a recorded pricing revision pins the catalogue",
			routing:    &Routing{ProviderRevisionID: revision, PricingRevisionID: &pinned},
			wantPinned: true},
		{name: "a policy may pin pricing without naming a revision",
			routing: &Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":true,"vendor_id":"google"}`)},
			wantPinned: true, wantVendor: "google"},
		{name: "an unpinned policy carries only its vendor",
			routing: &Routing{ProviderRevisionID: revision,
				Policy: policy(`{"pricing_pinned":false,"vendor_id":"openai"}`)},
			wantVendor: "openai"},
		{name: "a malformed policy is not guessed at",
			routing: &Routing{ProviderRevisionID: revision, Policy: policy(`{"pricing_pinned":`)},
			wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			pin, err := attemptRouting(&Attempt{Routing: test.routing})
			if test.wantErr {
				if !errors.Is(err, ErrInvalidEvent) {
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
