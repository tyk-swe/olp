package configuration

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/workload"
)

func TestWorkloadPublicationAllowlistsRoundTrip(t *testing.T) {
	project := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	toolset, agent := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608", "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"
	entry := WorkloadIssuerEntry{
		Config:   workload.Config{Name: "CI", Issuer: "https://issuer.example.test", JWKSURL: "https://issuer.example.test/keys", Audiences: []string{"olp"}, Algorithms: []string{"EdDSA"}, MaxLifetimeSeconds: 600, DisabledKeyIDs: []string{}},
		Mappings: []WorkloadMappingEntry{{Name: "worker", Match: map[string]string{}, Project: "Team", LimitTemplate: "worker", RouteGroups: []string{}, Scopes: []string{"tools", "agents"}, AllowedToolsets: []string{toolset}, AllowedAgents: []string{agent}}},
	}
	raw, err := json.Marshal(Document{APIVersion: APIVersion, WorkloadIssuers: []WorkloadIssuerEntry{entry}})
	if err != nil {
		t.Fatal(err)
	}
	var doc Document
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	config, err := doc.WorkloadIssuers[0].config(map[string]string{"team": project})
	if err != nil {
		t.Fatal(err)
	}
	mapping := config.Mappings[0]
	if mapping.ProjectID != project || !reflect.DeepEqual(mapping.AllowedToolsets, []string{toolset}) || !reflect.DeepEqual(mapping.AllowedAgents, []string{agent}) {
		t.Fatal("configuration serialization or project rebinding lost workload resource allowlists")
	}
}
