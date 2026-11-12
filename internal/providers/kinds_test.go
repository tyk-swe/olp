package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/vendors"
)

type listedVendor struct {
	ID             string   `json:"id"`
	Connector      string   `json:"connector"`
	Authentication []string `json:"authentication"`
}

func TestVendorListingPublishesEveryVendorWithItsAuthentication(t *testing.T) {
	t.Parallel()
	reply, err := (&Server{}).listVendors(httptest.NewRequest(http.MethodGet, "/api/v1/provider-vendors", nil), access.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	var listed []listedVendor
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatal(err)
	}
	contracts := vendors.All()
	if len(listed) != len(contracts) {
		t.Errorf("listed %d vendors, the vendors package publishes %d", len(listed), len(contracts))
	}
	for _, contract := range contracts {
		i := slices.IndexFunc(listed, func(v listedVendor) bool { return v.ID == contract.ID })
		if i < 0 {
			t.Errorf("vendor %s is not listed", contract.ID)
			continue
		}
		if listed[i].Connector != contract.Connector || len(listed[i].Authentication) == 0 {
			t.Errorf("vendor %s listed as %+v, want connector %s with its authentication modes", contract.ID, listed[i], contract.Connector)
		}
	}
}
