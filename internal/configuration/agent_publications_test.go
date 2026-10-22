package configuration

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/prompts"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func TestAgentConfigurationRoundTripAndOwnership(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	credential := id
	d := agentcontract.Draft{ProjectID: id, Name: "worker", Agent: &agentcontract.Definition{CredentialID: &credential, ToolsetRevisions: []string{id}}}
	exported := exportPublicationDraft(d)
	if exported.ProjectID != "" || exported.Agent.CredentialID != nil || d.Agent.CredentialID == nil {
		t.Fatal("export leaked destination identity or mutated source")
	}
	exported.Agent.ToolsetRevisions[0] = "changed"
	if d.Agent.ToolsetRevisions[0] != id {
		t.Fatal("configuration shares immutable source memory")
	}
	prompt := agentcontract.Draft{Name: "current name", Prompt: &prompts.Definition{Template: "{{name}}", Variables: []string{"name"}}}
	historical := prompt
	historical.Name = "historical name"
	e := PublicationEntry{Project: "Team", Kind: "prompt", Draft: prompt, Published: &historical, Labels: map[string]agentcontract.Draft{"production": historical}}
	doc := Document{APIVersion: APIVersion, AgentPublications: []PublicationEntry{e}}
	if err := (&Server{}).validateAgentPublications(&doc); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip Document
	if err = json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.AgentPublications, roundTrip.AgentPublications) {
		t.Fatal("configuration lost draft, publication or label separation")
	}
	copy := e
	copy.Project = "team"
	copy.Draft.Name = "CURRENT NAME"
	if publicationCredentialRef(e) != publicationCredentialRef(copy) {
		t.Fatal("credential identity differs by case")
	}
	doc.AgentPublications[0].Labels["unknown"] = prompt
	if (&Server{}).validateAgentPublications(&doc) == nil {
		t.Fatal("accepted an unknown prompt label")
	}
}

func TestAgentCredentialReuseCannotCrossDestinations(t *testing.T) {
	id := "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"
	old := agentcontract.Draft{Agent: &agentcontract.Definition{Endpoint: "https://one.example.test", CredentialID: &id}}
	same := agentcontract.Draft{Agent: &agentcontract.Definition{Endpoint: old.Agent.Endpoint}}
	other := agentcontract.Draft{Agent: &agentcontract.Definition{Endpoint: "https://two.example.test"}}
	if !reusableAgentCredential(&old, &same) || reusableAgentCredential(&old, &other) || reusableAgentCredential(nil, &same) {
		t.Fatal("credential reuse crossed its destination boundary")
	}
	e := PublicationEntry{Project: "team", Kind: "agent", Draft: old, Published: &other}
	base := publicationCredentialRef(e)
	e.CredentialRef = &base
	e.PublishedCredentialRef = &base
	if (&Server{}).validateAgentPublications(&Document{AgentPublications: []PublicationEntry{e}}) == nil {
		t.Fatal("one credential reference was accepted for different endpoints")
	}
}

func TestAgentConfigurationIdentitySeparatesNames(t *testing.T) {
	left := PublicationEntry{Project: "a", Kind: "prompt", Draft: agentcontract.Draft{
		Name: "b/prompt/c", Prompt: &prompts.Definition{Template: "Hello", Variables: []string{}},
	}}
	right := PublicationEntry{Project: "a/prompt/b", Kind: "prompt", Draft: agentcontract.Draft{
		Name: "c", Prompt: &prompts.Definition{Template: "Hello", Variables: []string{}},
	}}
	if publicationKey(left) == publicationKey(right) || publicationCredentialRef(left) == publicationCredentialRef(right) {
		t.Fatal("distinct owning project/name identities collide")
	}
	doc := &Document{AgentPublications: []PublicationEntry{left, right}}
	if err := (&Server{}).validateAgentPublications(doc); err != nil {
		t.Fatalf("distinct slash-containing identities refused: %v", err)
	}
	doc.AgentPublications = append(doc.AgentPublications, left)
	if (&Server{}).validateAgentPublications(doc) == nil {
		t.Fatal("duplicate publication accepted")
	}
}

func TestAgentCredentialBindings(t *testing.T) {
	draft, published := "agent/draft", "agent/draft/published"
	doc := &Document{AgentPublications: []PublicationEntry{{CredentialRef: &draft, PublishedCredentialRef: &published}}}
	for _, ref := range []string{draft, published} {
		t.Run(ref, func(t *testing.T) {
			if err := validateBindings(doc, plainBindings(map[string]string{ref: "bearer-token"})); err != nil {
				t.Fatalf("declared agent credential refused: %v", err)
			}
			for _, value := range []string{"", "bad\rheader", "bad\nheader", "bad\x00header", strings.Repeat("a", 4097)} {
				if validateBindings(doc, plainBindings(map[string]string{ref: value})) == nil {
					t.Fatal("invalid static bearer material accepted")
				}
			}
			external := &secretstore.Reference{Store: "gcp", SecretID: "projects/team/secrets/agent", Version: "1"}
			if validateBindings(doc, bindingSet{ref: {reference: external}}) == nil {
				t.Fatal("external agent credential accepted instead of sealed binding")
			}
		})
	}
	if err := validateBindings(doc, plainBindings(map[string]string{draft: "draft-token", published: "published-token"})); err != nil {
		t.Fatal(err)
	}
	if validateBindings(doc, plainBindings(map[string]string{"agent/undeclared": "token"})) == nil {
		t.Fatal("unreferenced binding accepted")
	}
}
