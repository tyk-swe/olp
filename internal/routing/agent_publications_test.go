package routing

import (
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	grantconfig "github.com/tyk-swe/olp/internal/grants/config"
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

func TestAgentSnapshotCloneOwnsCredentialsAndTaskPolicy(t *testing.T) {
	credential, bound, continuations, deadline := "credential", 30, 8, 120
	definition := agentcontract.Draft{Name: "assistant", Agent: &agentcontract.Definition{CredentialID: &credential, OAuth: &grantconfig.OAuth{ClientID: "client", Scopes: []string{"read"}, EntraOBO: &grantconfig.EntraOBO{Issuer: "issuer"}}, RemoteWorkBoundSeconds: &bound, MaxTaskContinuations: &continuations, TaskDeadlineSeconds: &deadline}}
	original := &Snapshot{AgentPublications: []agentcontract.Revision{{Definition: definition}}}
	before, err := original.Digest()
	if err != nil {
		t.Fatal(err)
	}
	cloned := original.Clone()
	agent := cloned.AgentPublications[0].Definition.Agent
	*agent.CredentialID = "different"
	agent.OAuth.Scopes[0] = "write"
	agent.OAuth.EntraOBO.Issuer = "other"
	*agent.RemoteWorkBoundSeconds = 60
	*agent.MaxTaskContinuations = 16
	*agent.TaskDeadlineSeconds = 240
	after, err := original.Digest()
	if err != nil || before != after {
		t.Fatal("changing a cloned publication changed the serving snapshot", err)
	}
	changed, err := cloned.Digest()
	if err != nil || changed == before {
		t.Fatal("changed publication policy did not affect its own digest", err)
	}
	normalized := agentcontract.NormalizeDraft(definition)
	if !reflect.DeepEqual(normalized.Agent.OAuth, definition.Agent.OAuth) {
		t.Fatal("normalization changed OAuth policy")
	}
}
