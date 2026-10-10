package configuration

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestMCPArtifactReferencesAreScopedAndRequireSealedBearerBindings(t *testing.T) {
	a := MCPServerEntry{Project: "A/B", Name: "C", Transport: "streamable_http", Endpoint: "https://mcp.example.com", CatalogDigest: strings.Repeat("a", 64)}
	b := a
	b.Project, b.Name = "A", "B/C"
	if mcpReference(a) == mcpReference(b) {
		t.Fatal("logical identities collide")
	}
	ref := mcpReference(a)
	a.CredentialRef = &ref
	doc := &Document{MCPServers: []MCPServerEntry{a}}
	s := &Server{Egress: &egress.Policy{}}
	if err := s.validateMCPServers(doc); err != nil {
		t.Fatal(err)
	}
	if err := validateBindings(doc, plainBindings(map[string]string{ref: "destination-bearer"})); err != nil {
		t.Fatal(err)
	}
	if err := validateBindings(doc, plainBindings(map[string]string{ref: "private\ninvalid"})); err == nil {
		t.Fatal("header-control material accepted")
	}
	a.CatalogDigest = "not-a-digest"
	doc.MCPServers[0] = a
	if s.validateMCPServers(doc) == nil {
		t.Fatal("uncertified identity accepted")
	}
}
