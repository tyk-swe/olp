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

func TestToolsetBoundaryRunsToolsetScopeAndSkipsRoutes(t *testing.T) {
	r := &Registry{Definitions: map[string]Revision{}, Policies: map[string]PolicyRevision{}, ProjectOrganizations: map[string]string{"p": "o"}}
	attach := func(name, scope, scopeID string) {
		r.Definitions[name] = Revision{ID: name, GuardrailID: name, ProjectID: "p", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: FailClosed}
		r.Policies[name] = PolicyRevision{ID: name, PolicyID: name, ProjectID: "p", Entries: []Entry{{GuardrailID: name, RevisionID: name, Phase: Input, Action: Block, Mandatory: true}}}
		r.Attachments = append(r.Attachments, Attachment{ID: name, Scope: scope, ScopeID: scopeID, PolicyRevisionID: name})
	}
	attach("on-route", "route", "r")
	attach("on-project", "project", "p")
	attach("on-toolset", "toolset", "ts-1")
	attach("on-key", "api-key", "k")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	tools, err := r.Entries(Boundary{OrganizationID: "o", ProjectID: "p", ToolsetID: "ts-1", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(tools))
	for i, entry := range tools {
		got[i] = entry.GuardrailID
	}
	if len(got) != 3 || got[0] != "on-project" || got[1] != "on-toolset" || got[2] != "on-key" {
		t.Fatalf("tool calls did not get project, toolset, then key policies without route policies: %v", got)
	}
	routed, err := r.Entries(Boundary{OrganizationID: "o", ProjectID: "p", RouteID: "r", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range routed {
		if entry.GuardrailID == "on-toolset" {
			t.Fatal("a toolset policy reached a model route")
		}
	}
	other, err := r.Entries(Boundary{OrganizationID: "o", ProjectID: "p", ToolsetID: "ts-2", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range other {
		if entry.GuardrailID == "on-toolset" {
			t.Fatal("a toolset policy reached a different toolset")
		}
	}
}
