package contract_test

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRouteNamesAgreeWithManagementContract(t *testing.T) {
	raw, err := os.ReadFile("../../../openapi/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Pattern string `json:"pattern"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct{ schema, property string }{
		{"CodeRouteWrite", "slug"},
		{"ConfigurationCodeRoute", "slug"},
		{"RouteFallback", "route"},
		{"RouteSelector", "route"},
		{"WorkloadMapping", "limit_template"},
		{"ConfigurationWorkloadMapping", "limit_template"},
	} {
		t.Run(field.schema+"/"+field.property, func(t *testing.T) {
			pattern := document.Components.Schemas[field.schema].Properties[field.property].Pattern
			if pattern == "" {
				t.Fatal("contract does not constrain the identifier")
			}
			expression, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []string{"route", "route.name", "route-2", "_route", "route/name", "", strings.Repeat("a", 100), strings.Repeat("a", 101)} {
				if expression.MatchString(value) != routeidentity.ValidSlug(value) {
					t.Errorf("contract and route validation disagree for %q", value)
				}
			}
		})
	}
}

func openAPIEnum(t *testing.T, schema string) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../openapi/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `json:"enum"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	values := document.Components.Schemas[schema].Enum
	if len(values) == 0 {
		t.Fatalf("OpenAPI schema %s declares no enum", schema)
	}
	return values
}

// Generated structs express fields, but encoding/json permits unknown fields
// by default. Management handlers must explicitly enforce additionalProperties:false.
func TestRequestOriginsAgreeAcrossUsageContractAndOpenAPI(t *testing.T) {
	usageOrigins := []string{
		usage.OriginCaller,
		usage.OriginShadow,
		usage.OriginClassifier,
		usage.OriginCacheEmbedding,
		usage.OriginProbe,
		usage.OriginPlayground,
		usage.OriginBatchSettlement,
		usage.OriginRetainedSettlement,
	}
	declared := openAPIEnum(t, "RequestOrigin")
	if !slices.Equal(slices.Sorted(slices.Values(usageOrigins)), slices.Sorted(slices.Values(declared))) {
		t.Fatalf("usage origins %v disagree with the OpenAPI RequestOrigin enum %v", usageOrigins, declared)
	}
	for _, origin := range declared {
		value := contract.RequestOrigin(origin)
		if !value.Valid() {
			t.Fatalf("OpenAPI origin %q is missing from the generated contract", origin)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded contract.RequestOrigin
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != value {
			t.Fatalf("origin %q does not round trip through JSON: %q, %v", origin, decoded, err)
		}
	}
	if contract.RequestOrigin("not_an_origin").Valid() {
		t.Fatal("an unknown origin reports valid")
	}
}
