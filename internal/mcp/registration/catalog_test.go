package registration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
)

func catalogTool(name, description, schema string) routing.MCPTool {
	return routing.MCPTool{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func TestDiffCatalogsReportsAddedRemovedAndModifiedTools(t *testing.T) {
	saved := []routing.MCPTool{
		catalogTool("legacy", "Legacy lookup", `{"type":"object","properties":{}}`),
		catalogTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"}}}`),
	}
	discovered := []routing.MCPTool{
		catalogTool("lookup", "Look up a record", `{"type":"object","properties":{"id":{"type":"string"},"verbose":{"type":"boolean"}},"required":["id"]}`),
		catalogTool("search", "Search records", `{"type":"object","properties":{"q":{"type":"string"}}}`),
	}
	added, removed, modified := diffCatalogs(saved, discovered)
	if len(added) != 1 || added[0].Name != "search" || added[0].Description != "Search records" {
		t.Fatalf("added = %+v", added)
	}
	if len(removed) != 1 || removed[0].Name != "legacy" {
		t.Fatalf("removed = %+v", removed)
	}
	if len(modified) != 1 || modified[0].Name != "lookup" {
		t.Fatalf("modified = %+v", modified)
	}
	if !reflect.DeepEqual(modified[0].ChangedFields, []string{"input_schema"}) {
		t.Fatalf("changed fields = %v", modified[0].ChangedFields)
	}
	if modified[0].SavedDescription != "Look up a record" || modified[0].DiscoveredDescription != "Look up a record" {
		t.Fatalf("descriptions = %+v", modified[0])
	}
	if string(modified[0].SavedInputSchema) != `{"type":"object","properties":{"id":{"type":"string"}}}` ||
		!strings.Contains(string(modified[0].DiscoveredInputSchema), `"verbose"`) {
		t.Fatalf("schemas = %+v", modified[0])
	}
}

func TestDiffCatalogsNamesChangedFields(t *testing.T) {
	saved := []routing.MCPTool{catalogTool("lookup", "Before", `{"type":"object"}`)}
	for _, tc := range []struct {
		name        string
		description string
		schema      string
		want        []string
	}{
		{"description only", "After", `{"type":"object"}`, []string{"description"}},
		{"both", "After", `{"type":"object","properties":{}}`, []string{"description", "input_schema"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, modified := diffCatalogs(saved, []routing.MCPTool{catalogTool("lookup", tc.description, tc.schema)})
			if len(modified) != 1 || !reflect.DeepEqual(modified[0].ChangedFields, tc.want) {
				t.Fatalf("modified = %+v, want fields %v", modified, tc.want)
			}
		})
	}
}

func TestDiffCatalogsTreatsReorderedSchemaAsUnchanged(t *testing.T) {
	saved := []routing.MCPTool{catalogTool("lookup", "Look up", `{"type":"object","properties":{"id":{"type":"string","minLength":1}}}`)}
	discovered := []routing.MCPTool{catalogTool("lookup", "Look up", `{"properties":{"id":{"minLength":1,"type":"string"}},"type":"object"}`)}
	added, removed, modified := diffCatalogs(saved, discovered)
	if len(added) != 0 || len(removed) != 0 || len(modified) != 0 {
		t.Fatalf("reordered schema reported as a change: added=%v removed=%v modified=%v", added, removed, modified)
	}
	if added == nil || removed == nil || modified == nil {
		t.Fatal("diff lists must stay non-nil so they encode as empty arrays")
	}
}

func TestCatalogApprovalValidation(t *testing.T) {
	if err := (catalogApproval{ProtocolVersion: "2025-11-25", Digest: strings.Repeat("a", 64)}).validate(); err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
	for _, approval := range []catalogApproval{
		{Digest: strings.Repeat("a", 64)},
		{ProtocolVersion: "2025-11-25"},
		{ProtocolVersion: "2025-11-25", Digest: "short"},
		{ProtocolVersion: "2025-11-25", Digest: strings.Repeat("A", 64)},
	} {
		if err := approval.validate(); err == nil {
			t.Fatalf("approval accepted: %+v", approval)
		}
	}
}

func TestRetainedToolPricesDropRemovedTools(t *testing.T) {
	prices := map[string]string{"lookup": "1.5", "legacy": "2"}
	catalog := &upstream.Catalog{Tools: []routing.MCPTool{{Name: "lookup"}, {Name: "search"}}}
	retained := retainedToolPrices(prices, catalog)
	if !reflect.DeepEqual(retained, map[string]string{"lookup": "1.5"}) {
		t.Fatalf("retained = %v", retained)
	}
	if !reflect.DeepEqual(prices, map[string]string{"lookup": "1.5", "legacy": "2"}) {
		t.Fatalf("input prices changed: %v", prices)
	}
}
