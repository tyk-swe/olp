package configuration

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
)

func mcpConfigServer() *Server {
	return &Server{Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}}
}

func mcpConfigEntry() MCPServerEntry {
	return MCPServerEntry{
		Name: "Priced tools", Project: "Team", Transport: "streamable_http",
		Endpoint: "http://127.0.0.1:8080/mcp", Enabled: true,
		CatalogDigest: strings.Repeat("a", 64),
	}
}

func TestMCPServerPricingRoundTripsConfiguration(t *testing.T) {
	price, rate, concurrency := "0.25", int64(90), int64(3)
	definition := &mcpservers.Definition{
		Name: "Priced tools", Transport: "streamable_http", Endpoint: "http://127.0.0.1:8080/mcp",
		Enabled: true, Catalog: &mcpservers.Catalog{Digest: strings.Repeat("a", 64)},
		UnitPrice: &price, ToolPrices: map[string]string{"lookup": "0.5"},
		CallsPerMinute: rate, MaxConcurrency: concurrency, PerKeyCallsPerMinute: 45, PerKeyMaxConcurrency: 2,
	}
	exported := mcpEntry(definition, "Team")
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential") && !strings.Contains(string(raw), "credential_ref") {
		t.Fatalf("export carries credential material: %s", raw)
	}
	var imported MCPServerEntry
	if err = json.Unmarshal(raw, &imported); err != nil {
		t.Fatal(err)
	}
	doc := &Document{MCPServers: []MCPServerEntry{imported}}
	if err = mcpConfigServer().validateMCPServers(doc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.MCPServers[0], exported) {
		t.Fatalf("pricing did not round trip: imported=%+v exported=%+v", doc.MCPServers[0], exported)
	}
	applied := mcpDefinitionFromEntry(doc.MCPServers[0], definition.Catalog)
	if applied.UnitPrice == nil || *applied.UnitPrice != price || applied.ToolPrices["lookup"] != "0.5" ||
		applied.CallsPerMinute != rate || applied.MaxConcurrency != concurrency ||
		applied.PerKeyCallsPerMinute != 45 || applied.PerKeyMaxConcurrency != 2 {
		t.Fatalf("applied definition lost pricing: %+v", applied)
	}
}

func TestMCPServerConfigurationDefaultsAndRefusals(t *testing.T) {
	doc := &Document{MCPServers: []MCPServerEntry{mcpConfigEntry()}}
	if err := mcpConfigServer().validateMCPServers(doc); err != nil {
		t.Fatal(err)
	}
	entry := doc.MCPServers[0]
	if entry.UnitPrice != nil || entry.ToolPrices == nil || len(entry.ToolPrices) != 0 {
		t.Fatalf("unpriced entry not normalized: %+v", entry)
	}
	if entry.CallsPerMinute == nil || *entry.CallsPerMinute != routing.DefaultToolCallsPerMinute ||
		entry.MaxConcurrency == nil || *entry.MaxConcurrency != routing.DefaultToolMaxConcurrency ||
		entry.PerKeyCallsPerMinute == nil || *entry.PerKeyCallsPerMinute != routing.DefaultToolCallsPerMinute ||
		entry.PerKeyMaxConcurrency == nil || *entry.PerKeyMaxConcurrency != routing.DefaultToolMaxConcurrency {
		t.Fatalf("default limits did not apply: %+v", entry)
	}
	negative := "-1"
	over := routing.MaxToolCallsPerMinute + 1
	for name, mutate := range map[string]func(*MCPServerEntry){
		"negative price": func(e *MCPServerEntry) { e.UnitPrice = &negative },
		"malformed tool price": func(e *MCPServerEntry) {
			e.ToolPrices = map[string]string{"lookup": "1e3"}
		},
		"rate over ceiling": func(e *MCPServerEntry) { e.CallsPerMinute = &over },
	} {
		e := mcpConfigEntry()
		mutate(&e)
		if err := mcpConfigServer().validateMCPServers(&Document{MCPServers: []MCPServerEntry{e}}); err == nil {
			t.Fatalf("configuration accepted %s", name)
		}
	}
}
