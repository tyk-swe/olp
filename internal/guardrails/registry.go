package guardrails

import (
	"encoding/json"
	"errors"
	"slices"
	"sort"
)

// Registry is serialized into the immutable runtime release. Policy entries
// point at exact definition revisions; attachments point at exact policies.
type Registry struct {
	MigratedRoutes       map[string]bool           `json:"migrated_routes,omitempty"`
	Definitions          map[string]Revision       `json:"definitions,omitempty"`
	Policies             map[string]PolicyRevision `json:"policies,omitempty"`
	Attachments          []Attachment              `json:"attachments,omitempty"`
	ProjectOrganizations map[string]string         `json:"project_organizations,omitempty"`
	WindowBytes          int                       `json:"window_bytes,omitempty"`
}

func (r *Registry) Validate() error {
	if r == nil {
		return nil
	}
	if len(r.Definitions) > 10000 || len(r.Policies) > 10000 || len(r.Attachments) > 10000 {
		return errors.New("guardrail registry exceeds publication bounds")
	}
	if r.WindowBytes != 0 && (r.WindowBytes < 1 || r.WindowBytes > MaxWindowBytes) {
		return errors.New("invalid guardrail stream window")
	}
	pluginModules := map[string]bool{}
	for id, d := range r.Definitions {
		if id != d.ID || !identifier.MatchString(id) || !identifier.MatchString(d.GuardrailID) || !identifier.MatchString(d.ProjectID) {
			return errors.New("invalid guardrail identity")
		}
		if err := ValidateDefinition(d); err != nil {
			return err
		}
		if d.Type == "plugin" {
			var c ExternalConfig
			_ = json.Unmarshal(d.Configuration, &c)
			pluginModules[c.PluginVersion] = true
			if len(pluginModules) > 16 {
				return errors.New("at most 16 guardrail plugin modules can be pinned")
			}
		}
	}
	for id, p := range r.Policies {
		if id != p.ID || !identifier.MatchString(id) || !identifier.MatchString(p.ProjectID) || len(p.Entries) > MaxEntries {
			return errors.New("invalid guardrail policy")
		}
		for _, e := range p.Entries {
			d, ok := r.Definitions[e.RevisionID]
			if !ok || d.ProjectID != p.ProjectID {
				return errors.New("guardrail policy crosses a project boundary")
			}
		}
		if _, err := Compile(p.Entries, r.Definitions, nil); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, a := range r.Attachments {
		if !identifier.MatchString(a.ID) || seen[a.ID] || a.Order < 0 {
			return errors.New("invalid guardrail attachment")
		}
		seen[a.ID] = true
		p, ok := r.Policies[a.PolicyRevisionID]
		if !ok {
			return errors.New("guardrail attachment has no policy revision")
		}
		switch a.Scope {
		case "installation":
			if a.ScopeID != "installation" {
				return errors.New("invalid installation attachment")
			}
		case "organization":
			if r.ProjectOrganizations[p.ProjectID] != a.ScopeID {
				return errors.New("organization attachment crosses a project boundary")
			}
		case "project":
			if a.ScopeID != p.ProjectID {
				return errors.New("project attachment crosses a project boundary")
			}
		case "route", "api-key":
			if !identifier.MatchString(a.ScopeID) {
				return errors.New("invalid attachment scope identity")
			}
		default:
			return errors.New("invalid guardrail attachment scope")
		}
	}
	return nil
}

func scopeRank(scope string) int {
	switch scope {
	case "installation":
		return 0
	case "organization":
		return 1
	case "project":
		return 2
	case "route":
		return 3
	case "api-key":
		return 4
	}
	return 5
}

// Entries only adds policies. There is deliberately no overriding or exclusion
// operator with which a narrower scope could remove inherited safeguards.
func (r *Registry) Entries(b Boundary) ([]Entry, error) {
	if r == nil {
		return nil, nil
	}
	attachments := []Attachment{}
	for _, a := range r.Attachments {
		matches := a.Scope == "installation" || (a.Scope == "organization" && a.ScopeID == b.OrganizationID) || (a.Scope == "project" && a.ScopeID == b.ProjectID) || (a.Scope == "route" && a.ScopeID == b.RouteID) || (a.Scope == "api-key" && a.ScopeID == b.KeyID)
		if matches {
			attachments = append(attachments, a)
		}
	}
	sort.SliceStable(attachments, func(i, j int) bool {
		x, y := attachments[i], attachments[j]
		if scopeRank(x.Scope) != scopeRank(y.Scope) {
			return scopeRank(x.Scope) < scopeRank(y.Scope)
		}
		xLegacy := x.Scope == "route" && r.MigratedRoutes[x.ScopeID] && r.Policies[x.PolicyRevisionID].PolicyID == x.ID
		yLegacy := y.Scope == "route" && r.MigratedRoutes[y.ScopeID] && r.Policies[y.PolicyRevisionID].PolicyID == y.ID
		if xLegacy != yLegacy {
			return xLegacy
		}
		if x.Order != y.Order {
			return x.Order < y.Order
		}
		return x.ID < y.ID
	})
	var entries []Entry
	for _, a := range attachments {
		p, ok := r.Policies[a.PolicyRevisionID]
		if !ok {
			return nil, errors.New("unresolved guardrail policy")
		}
		// Installation policies are deliberately global. Every narrower policy
		// must belong to the request project, including key/route attachments.
		if a.Scope != "installation" && a.Scope != "organization" && p.ProjectID != b.ProjectID {
			return nil, errors.New("guardrail attachment crosses request project")
		}
		entries = append(entries, p.Entries...)
		if len(entries) > MaxEntries {
			return nil, errors.New("effective guardrails exceed entry bound")
		}
	}
	return entries, nil
}
func (r *Registry) Compile(b Boundary, external Evaluator) (*Engine, error) {
	if r == nil {
		return nil, nil
	}
	entries, err := r.Entries(b)
	if err != nil {
		return nil, err
	}
	return Compile(entries, r.Definitions, external)
}
func (r *Registry) Window() int {
	if r != nil && r.WindowBytes > 0 {
		return r.WindowBytes
	}
	return DefaultWindowBytes
}

// Clone makes the publication independent of drafts retained by its caller.
func (r *Registry) Clone() *Registry {
	if r == nil {
		return nil
	}
	out := *r
	out.MigratedRoutes = make(map[string]bool, len(r.MigratedRoutes))
	for id, v := range r.MigratedRoutes {
		out.MigratedRoutes[id] = v
	}
	out.Definitions = make(map[string]Revision, len(r.Definitions))
	for id, d := range r.Definitions {
		d.Configuration = slices.Clone(d.Configuration)
		out.Definitions[id] = d
	}
	out.Policies = make(map[string]PolicyRevision, len(r.Policies))
	for id, p := range r.Policies {
		p.Entries = slices.Clone(p.Entries)
		out.Policies[id] = p
	}
	out.Attachments = slices.Clone(r.Attachments)
	out.ProjectOrganizations = make(map[string]string, len(r.ProjectOrganizations))
	for p, o := range r.ProjectOrganizations {
		out.ProjectOrganizations[p] = o
	}
	return &out
}
