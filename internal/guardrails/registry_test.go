package guardrails

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"slices"
	"testing"
)

func TestAdditiveScopeOrderAndIsolation(t *testing.T) {
	r := &Registry{Definitions: map[string]Revision{}, Policies: map[string]PolicyRevision{}, ProjectOrganizations: map[string]string{"p": "o"}}
	for _, scope := range []guardrailpolicy.Scope{guardrailpolicy.KeyScope, guardrailpolicy.RouteScope, guardrailpolicy.ProjectScope, guardrailpolicy.OrganizationScope, guardrailpolicy.InstallationScope} {
		name := string(scope)
		d := Revision{ID: name, GuardrailID: name, ProjectID: "p", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: guardrailpolicy.FailClosed}
		r.Definitions[d.ID] = d
		r.Policies[name] = PolicyRevision{ID: name, PolicyID: name, ProjectID: "p", Entries: []Entry{{GuardrailID: name, RevisionID: name, Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block, Mandatory: true}}}
		ids := map[guardrailpolicy.Scope]string{"installation": "installation", "organization": "o", "project": "p", "route": "r", "api-key": "k"}
		r.Attachments = append(r.Attachments, Attachment{ID: name, Scope: scope, ScopeID: ids[scope], PolicyRevisionID: name})
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	entries, err := r.Entries(Boundary{ProjectID: "p", RouteID: "r", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for i, scope := range []guardrailpolicy.Scope{guardrailpolicy.InstallationScope, guardrailpolicy.OrganizationScope, guardrailpolicy.ProjectScope, guardrailpolicy.RouteScope, guardrailpolicy.KeyScope} {
		if entries[i].GuardrailID != string(scope) || !entries[i].Mandatory {
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
	attach := func(name string, scope guardrailpolicy.Scope, scopeID string) {
		r.Definitions[name] = Revision{ID: name, GuardrailID: name, ProjectID: "p", Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: guardrailpolicy.FailClosed}
		r.Policies[name] = PolicyRevision{ID: name, PolicyID: name, ProjectID: "p", Entries: []Entry{{GuardrailID: name, RevisionID: name, Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block, Mandatory: true}}}
		r.Attachments = append(r.Attachments, Attachment{ID: name, Scope: scope, ScopeID: scopeID, PolicyRevisionID: name})
	}
	attach("on-route", guardrailpolicy.RouteScope, "r")
	attach("on-project", guardrailpolicy.ProjectScope, "p")
	attach("on-toolset", guardrailpolicy.ToolsetScope, "ts-1")
	attach("on-key", guardrailpolicy.KeyScope, "k")
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	tools, err := r.Entries(Boundary{ProjectID: "p", ToolsetID: "ts-1", KeyID: "k"})
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
	routed, err := r.Entries(Boundary{ProjectID: "p", RouteID: "r", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range routed {
		if entry.GuardrailID == "on-toolset" {
			t.Fatal("a toolset policy reached a model route")
		}
	}
	other, err := r.Entries(Boundary{ProjectID: "p", ToolsetID: "ts-2", KeyID: "k"})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range other {
		if entry.GuardrailID == "on-toolset" {
			t.Fatal("a toolset policy reached a different toolset")
		}
	}
}

func TestStagedAttachmentsReplaceOnlyTheirSlot(t *testing.T) {
	kept := Attachment{ID: "kept", Scope: guardrailpolicy.RouteScope, ScopeID: "r", PolicyRevisionID: "a", Order: 1}
	replaced := Attachment{ID: "replaced", Scope: guardrailpolicy.RouteScope, ScopeID: "r", PolicyRevisionID: "a", Order: 0}
	other := Attachment{ID: "other", Scope: guardrailpolicy.RouteScope, ScopeID: "s", PolicyRevisionID: "a", Order: 0}
	published := &Registry{Attachments: []Attachment{replaced, kept, other}}
	staged := Attachment{ID: "b", Scope: guardrailpolicy.RouteScope, ScopeID: "r", PolicyRevisionID: "b", Order: 0}
	merged, err := published.WithStaged([]Attachment{staged})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(merged.Attachments, []Attachment{kept, other, staged}) {
		t.Fatalf("merged = %+v", merged.Attachments)
	}
	if !slices.Equal(published.Attachments, []Attachment{replaced, kept, other}) {
		t.Fatalf("published registry changed: %+v", published.Attachments)
	}
	if _, err := (*Registry)(nil).WithStaged([]Attachment{staged}); err == nil {
		t.Fatal("staged attachments without a published registry were accepted")
	}
}
