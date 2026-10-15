package guardrails

import (
	"encoding/json"
	"testing"
)

func TestAdditiveScopeOrderAndIsolation(t *testing.T) {
	r := &Registry{Definitions: map[string]Revision{}, Policies: map[string]PolicyRevision{}, ProjectOrganizations: map[string]string{"p": "o"}}
	for _, scope := range []string{"api-key", "route", "project", "organization", "installation"} {
		d := Revision{ID: scope, GuardrailID: scope, ProjectID: "p", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: FailClosed}
		r.Definitions[d.ID] = d
		r.Policies[scope] = PolicyRevision{ID: scope, PolicyID: scope, ProjectID: "p", Entries: []Entry{{GuardrailID: scope, RevisionID: scope, Phase: Input, Action: Block, Mandatory: true}}}
		ids := map[string]string{"installation": "installation", "organization": "o", "project": "p", "route": "r", "api-key": "k"}
		r.Attachments = append(r.Attachments, Attachment{ID: scope, Scope: scope, ScopeID: ids[scope], PolicyRevisionID: scope})
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	entries, err := r.Entries(Boundary{OrganizationID: "o", ProjectID: "p", RouteID: "r", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for i, scope := range []string{"installation", "organization", "project", "route", "api-key"} {
		if entries[i].GuardrailID != scope || !entries[i].Mandatory {
			t.Fatal("scope order or inheritance changed")
		}
	}
	entries, err = r.Entries(Boundary{ProjectID: "other"})
	if err != nil || len(entries) != 1 || entries[0].GuardrailID != "installation" {
		t.Fatal("project boundary failed", entries, err)
	}
	clone := r.Clone()
	r.Policies["project"] = PolicyRevision{}
	if len(clone.Policies["project"].Entries) != 1 {
		t.Fatal("mutable release")
	}
}
