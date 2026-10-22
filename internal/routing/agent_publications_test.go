package routing

import (
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/prompts"
)

func TestAgentSnapshotCloneAndDigest(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	d := agentcontract.Draft{ProjectID: id, Name: "greeting", Prompt: &prompts.Definition{Template: "Hello {{name}}", Variables: []string{"name"}}}
	s := &Snapshot{AgentPublications: []agentcontract.Revision{{ID: id, PublicationID: id, ProjectID: id, Kind: "prompt", Digest: agentcontract.DraftDigest(d), Definition: d, Current: true, Labels: []string{"production"}}}}
	want, err := s.Digest()
	if err != nil {
		t.Fatal(err)
	}
	copy := s.Clone()
	copy.AgentPublications[0].Definition.Prompt.Variables[0] = "other"
	copy.AgentPublications[0].Labels[0] = "staging"
	if s.AgentPublications[0].Definition.Prompt.Variables[0] != "name" || s.AgentPublications[0].Labels[0] != "production" {
		t.Fatal("snapshot clone shares mutable publications")
	}
	got, err := copy.Digest()
	if err != nil || got == want {
		t.Fatal("publication mutation did not change serving digest", err)
	}
}
