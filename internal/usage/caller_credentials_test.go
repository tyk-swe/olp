package usage

import "testing"

func TestCallerBudgetEvidenceCannotClaimStoredCredentialsOrSupplyOwners(t *testing.T) {
	for _, tc := range []struct {
		name, source           string
		exempt, stored, owners bool
		valid                  bool
	}{
		{name: "caller priced", source: "caller", valid: true},
		{name: "caller paid", source: "caller", exempt: true, valid: true},
		{name: "stored credential", source: "caller", stored: true},
		{name: "operator exemption", source: "operator", exempt: true},
		{name: "supply owner", source: "caller", exempt: true, owners: true},
		{name: "unknown source", source: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := metadataEvent()
			revision := "018f5800-0000-7000-8000-000000000001"
			r := &Routing{CredentialSource: tc.source, BudgetExempt: tc.exempt, ProviderRevisionID: revision}
			ev.Attempts[0].Routing = r
			if tc.stored {
				r.CredentialVersionID = &revision
			}
			if tc.owners {
				r.Budgets = []string{revision}
			}
			if _, err := Validate(ev); (err == nil) != tc.valid {
				t.Fatalf("validation %v, wanted valid=%v", err, tc.valid)
			}
			raw, err := Encode(ev)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := Decode(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("wire validation %v, wanted valid=%v", err, tc.valid)
			}
			if tc.valid && decoded.Attempts[0].Routing.BudgetExempt != tc.exempt {
				t.Fatal("lost immutable exemption")
			}
		})
	}
}
