package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

func TestAcceptCatalogSuggestionsRejectsDuplicateJSONKeys(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body       string
		wantStatus int
		wantCode   string
	}{
		"duplicate digest": {
			body:       `{"catalog_sha256":"first","catalog_sha256":"second","upstream_models":["model-a"]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_json",
		},
		"duplicate models field": {
			body:       `{"catalog_sha256":"digest","upstream_models":["model-a"],"upstream_models":["model-b"]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_json",
		},
		"unique keys pass decoding": {
			body:       `{"catalog_sha256":"digest","upstream_models":["model-a"]}`,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   "reference_catalog_unavailable",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/providers/018f1f77-9823-7b8a-b847-3658e91e4d01/catalog-suggestions/accept", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			_, err := (&modelService{}).acceptCatalogSuggestions(req, authz.Principal{})
			problem, ok := httpkit.ProblemFor(err)
			if !ok {
				t.Fatalf("unexpected error %v", err)
			}
			if problem.Status != tc.wantStatus || problem.Code != tc.wantCode {
				t.Fatalf("got status %d code %q, want status %d code %q", problem.Status, problem.Code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestCatalogMetadataHelpersPropagateMalformedJSON(t *testing.T) {
	t.Parallel()

	t.Run("canonicalModel", func(t *testing.T) {
		t.Parallel()
		for _, empty := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("   "), json.RawMessage("{}"), json.RawMessage("null")} {
			got, err := canonicalModel(empty)
			if err != nil || got != "" {
				t.Fatalf("canonicalModel(%q) = (%q, %v), want (\"\", nil)", string(empty), got, err)
			}
		}
		got, err := canonicalModel(json.RawMessage(`{"canonical_model":"family/model"}`))
		if err != nil || got != "family/model" {
			t.Fatalf("canonicalModel valid = (%q, %v), want (\"family/model\", nil)", got, err)
		}
		for _, malformed := range []json.RawMessage{json.RawMessage(`{broken`), json.RawMessage(`{"canonical_model":123}`)} {
			if _, err := canonicalModel(malformed); err == nil {
				t.Fatalf("canonicalModel(%q) succeeded, want error", string(malformed))
			}
		}
	})

	t.Run("privacyEvidence", func(t *testing.T) {
		t.Parallel()
		for _, empty := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("   "), json.RawMessage("{}"), json.RawMessage("null")} {
			got, err := privacyEvidence(empty)
			if err != nil || got {
				t.Fatalf("privacyEvidence(%q) = (%v, %v), want (false, nil)", string(empty), got, err)
			}
		}
		for _, declared := range []json.RawMessage{json.RawMessage(`{"data_collection":false}`), json.RawMessage(`{"zero_data_retention":false}`)} {
			got, err := privacyEvidence(declared)
			if err != nil || !got {
				t.Fatalf("privacyEvidence(%q) = (%v, %v), want (true, nil)", string(declared), got, err)
			}
		}
		for _, malformed := range []json.RawMessage{json.RawMessage(`{broken`), json.RawMessage(`{"data_collection":"deny"}`), json.RawMessage(`{"zero_data_retention":"yes"}`)} {
			if _, err := privacyEvidence(malformed); err == nil {
				t.Fatalf("privacyEvidence(%q) succeeded, want error", string(malformed))
			}
		}
	})

	t.Run("suggestionsFor propagates malformed metadata", func(t *testing.T) {
		t.Parallel()
		signed, err := catalog.Embedded()
		if err != nil {
			t.Fatal(err)
		}
		vendorID := signed.Catalog.Vendors[0].ID
		cfg := connection.Configuration{
			Options: connection.Options{
				VendorID: &vendorID,
				Models:   map[string]json.RawMessage{"unmatched-model": json.RawMessage(`{broken`)},
			},
		}
		if _, err := suggestionsFor(signed, cfg, []storedModel{{ID: "m-1", UpstreamModel: "unmatched-model"}}); err == nil {
			t.Fatal("suggestionsFor succeeded with malformed metadata, want error")
		}
	})
}
