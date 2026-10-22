package agentcontract

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/prompts"
)

const contractID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

func TestPublicationContracts(t *testing.T) {
	tool := Tool{ServerID: contractID, RevisionID: contractID, CatalogDigest: strings.Repeat("a", 64), Name: "lookup"}
	tool.Alias = ToolAlias(tool.ServerID, tool.Name)
	d := Draft{ProjectID: contractID, Name: "research", Toolset: &Toolset{Tools: []Tool{tool}}}
	if err := d.Validate("toolset"); err != nil {
		t.Fatal(err)
	}
	if len(tool.Alias) != 67 || tool.Alias == ToolAlias(tool.ServerID, "other") {
		t.Fatal("unstable tool alias")
	}
	d.Toolset.Tools = append(d.Toolset.Tools, tool)
	if d.Validate("toolset") == nil {
		t.Fatal("accepted alias collision")
	}
	d.Toolset.Tools = d.Toolset.Tools[:1]
	d.Toolset.Tools[0].CatalogDigest = strings.Repeat("A", 64)
	if d.Validate("toolset") == nil {
		t.Fatal("accepted noncanonical catalog digest")
	}
	for _, raw := range []string{`{"project_id":"` + contractID + `","name":"x","execute":true}`, `{} {}`, `{"project_id":"` + contractID + `","name":"x","prompt":{"template":"hello","variables":[]},"toolset":{"tools":[]}}`} {
		parsed, err := DecodeDraft([]byte(raw))
		if err == nil && parsed.Validate("prompt") == nil {
			t.Fatalf("accepted unsupported draft %s", raw)
		}
	}
}

func TestPublicationRevisionIsolation(t *testing.T) {
	d := Draft{ProjectID: contractID, Name: "greeting", Prompt: &prompts.Definition{Template: "{{name}}", Variables: []string{"name"}}}
	v := Revision{ID: contractID, PublicationID: contractID, ProjectID: contractID, Kind: "prompt", Digest: DraftDigest(d), Definition: d, Current: true, Labels: []string{"production"}}
	if err := ValidateRevisions([]Revision{v}); err != nil {
		t.Fatal(err)
	}
	clone := CloneRevision(v)
	clone.Definition.Prompt.Variables[0] = "changed"
	clone.Labels[0] = "staging"
	if v.Definition.Prompt.Variables[0] != "name" || v.Labels[0] != "production" {
		t.Fatal("clone shares mutable revision data")
	}
	if ValidateRevisions([]Revision{v, v}) == nil {
		t.Fatal("accepted duplicate published identity")
	}
	v.Digest = strings.Repeat("0", 64)
	if ValidateRevisions([]Revision{v}) == nil {
		t.Fatal("accepted corrupted published definition")
	}
}
