package resources

import (
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

// Every stored kind must be a member of the management contract, or the
// provider-resource list emits values outside its own schema.
func TestEveryStoredKindIsInTheManagementContract(t *testing.T) {
	for _, kind := range []string{
		KindFile, KindBatch, KindResponse, KindContinuation, KindStrictResponse,
		KindInteraction, KindStrictFile, KindStrictBatch,
	} {
		if !contract.ProviderResourceItemKind(kind).Valid() {
			t.Errorf("item kind %q is outside the contract", kind)
		}
		if !contract.ListProviderResourcesParamsKind(kind).Valid() {
			t.Errorf("kind filter %q is outside the contract", kind)
		}
	}
}

func TestListRejectsUnknownKindFilter(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/provider-resources?kind=bogus", nil)
	if _, err := (&Management{}).list(r, access.Principal{}); err == nil {
		t.Fatal("unknown kind filter must be rejected")
	}
}
