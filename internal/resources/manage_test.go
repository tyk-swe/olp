package resources

import (
	"testing"

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
