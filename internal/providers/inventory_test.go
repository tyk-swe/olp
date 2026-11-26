package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

type listedVendor struct {
	ID             string   `json:"id"`
	Connector      string   `json:"connector"`
	Authentication []string `json:"authentication"`
}

func TestVendorListingPublishesEveryVendorWithItsAuthentication(t *testing.T) {
	t.Parallel()
	reply, err := (&inventoryService{}).listVendors(httptest.NewRequest(http.MethodGet, "/api/v1/provider-vendors", nil), authz.Principal{})
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

func TestInventoryTypedRepliesPreserveContractShapes(t *testing.T) {
	t.Parallel()

	t.Run("kinds", func(t *testing.T) {
		t.Parallel()
		reply, err := (&inventoryService{}).kinds(httptest.NewRequest(http.MethodGet, "/api/v1/provider-kinds", nil), authz.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(reply.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Items []connection.ListedKind `json:"items"`
		}
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Items) != len(connection.Kinds()) {
			t.Fatalf("got %d kinds, want %d", len(body.Items), len(connection.Kinds()))
		}
	})

	t.Run("kindCapabilities", func(t *testing.T) {
		t.Parallel()
		kind := connection.Kinds()[0].Name
		req := httptest.NewRequest(http.MethodGet, "/api/v1/provider-kinds/"+string(kind)+"/capabilities", nil)
		req.SetPathValue("provider_kind", string(kind))
		reply, err := (&inventoryService{}).kindCapabilities(req, authz.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(reply.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		if string(body["provider_kind"]) != `"`+string(kind)+`"` {
			t.Fatalf("provider_kind = %s, want %q", string(body["provider_kind"]), kind)
		}
		if len(body["capabilities"]) == 0 || body["capabilities"][0] != '[' {
			t.Fatalf("capabilities = %s, want JSON array", string(body["capabilities"]))
		}
	})

	t.Run("operationDialects", func(t *testing.T) {
		t.Parallel()
		reply, err := (&inventoryService{}).operationDialects(httptest.NewRequest(http.MethodGet, "/api/v1/operation-dialects", nil), authz.Principal{})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(reply.Body)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Items []operationDialectReply `json:"items"`
		}
		if err := json.Unmarshal(encoded, &body); err != nil {
			t.Fatal(err)
		}
		want := operationregistry.Catalog().Dialects()
		if len(body.Items) != len(want) {
			t.Fatalf("got %d dialects, want %d", len(body.Items), len(want))
		}
		for i, item := range body.Items {
			if item.ID != want[i].Identity.ID || item.Mode != "unary" {
				t.Fatalf("dialect[%d] = %+v, want id %q mode unary", i, item, want[i].Identity.ID)
			}
		}
	})
}
