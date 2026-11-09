package guardrails

import (
	"cmp"
	"errors"
	"slices"
	"strings"
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
		spec, err := ParseRevision(d)
		if err != nil {
			return err
		}
		if plugin, ok := spec.(PluginSpec); ok {
			pluginModules[plugin.Version] = true
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
		if _, err := Compile(p.Entries, r.Definitions); err != nil {
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
		case InstallationScope:
			if a.ScopeID != InstallationProject {
				return errors.New("invalid installation attachment")
			}
		case OrganizationScope:
			if r.ProjectOrganizations[p.ProjectID] != a.ScopeID {
				return errors.New("organization attachment crosses a project boundary")
			}
		case ProjectScope:
			if a.ScopeID != p.ProjectID {
				return errors.New("project attachment crosses a project boundary")
			}
		case RouteScope, ToolsetScope, KeyScope:
			if !identifier.MatchString(a.ScopeID) {
				return errors.New("invalid attachment scope identity")
			}
		default:
			return errors.New("invalid guardrail attachment scope")
		}
	}
	return nil
}

var scopeRanks = map[Scope]int{InstallationScope: 0, OrganizationScope: 1, ProjectScope: 2, RouteScope: 3, ToolsetScope: 3, KeyScope: 4}

func (b Boundary) matches(a Attachment, organization string) bool {
	switch a.Scope {
	case InstallationScope:
		return true
	case OrganizationScope:
		return a.ScopeID == organization
	case ProjectScope:
		return a.ScopeID == b.ProjectID
	case RouteScope:
		return a.ScopeID == b.RouteID
	case ToolsetScope:
		return a.ScopeID == b.ToolsetID
	case KeyScope:
		return a.ScopeID == b.KeyID
	}
	return false
}

// Entries only adds policies. There is deliberately no overriding or exclusion
// operator with which a narrower scope could remove inherited safeguards.
func (r *Registry) Entries(b Boundary) ([]Entry, error) {
	if r == nil {
		return nil, nil
	}
	organization := r.ProjectOrganizations[b.ProjectID]
	legacyFirst := func(a Attachment) int {
		if a.Scope == RouteScope && r.MigratedRoutes[a.ScopeID] && r.Policies[a.PolicyRevisionID].PolicyID == a.ID {
			return 0
		}
		return 1
	}
	attachments := slices.SortedStableFunc(slices.Values(slices.DeleteFunc(slices.Clone(r.Attachments), func(a Attachment) bool { return !b.matches(a, organization) })), func(x, y Attachment) int {
		return cmp.Or(cmp.Compare(scopeRanks[x.Scope], scopeRanks[y.Scope]), cmp.Compare(legacyFirst(x), legacyFirst(y)), cmp.Compare(x.Order, y.Order), strings.Compare(x.ID, y.ID))
	})
	var entries []Entry
	for _, a := range attachments {
		p, ok := r.Policies[a.PolicyRevisionID]
		if !ok {
			return nil, errors.New("unresolved guardrail policy")
		}
		// Installation policies are deliberately global. Every narrower policy
		// must belong to the request project, including key/route attachments.
		if a.Scope != InstallationScope && a.Scope != OrganizationScope && p.ProjectID != b.ProjectID {
			return nil, errors.New("guardrail attachment crosses request project")
		}
		entries = append(entries, p.Entries...)
		if len(entries) > MaxEntries {
			return nil, errors.New("effective guardrails exceed entry bound")
		}
	}
	return entries, nil
}

func (r *Registry) Engines(b Boundary) (map[string]*Engine, error) {
	if r == nil {
		return nil, nil
	}
	keys := slices.Collect(func(yield func(string) bool) {
		if !yield("") {
			return
		}
		for _, a := range r.Attachments {
			if a.Scope == KeyScope && r.Policies[a.PolicyRevisionID].ProjectID == b.ProjectID && !yield(a.ScopeID) {
				return
			}
		}
	})
	engines := make(map[string]*Engine, len(keys))
	for _, key := range keys {
		entries, err := r.Entries(Boundary{ProjectID: b.ProjectID, RouteID: b.RouteID, ToolsetID: b.ToolsetID, KeyID: key})
		if err != nil {
			return nil, err
		}
		engine, err := Compile(entries, r.Definitions)
		if err != nil {
			return nil, err
		}
		if engine != nil {
			engines[key] = engine
		}
	}
	return engines, nil
}

func (r *Registry) CompilePolicy(id string) (PolicyRevision, *Engine, error) {
	if r == nil {
		return PolicyRevision{}, nil, ErrSurface
	}
	p, ok := r.Policies[id]
	if !ok {
		return PolicyRevision{}, nil, ErrSurface
	}
	engine, err := Compile(p.Entries, r.Definitions)
	return p, engine, err
}
func (r *Registry) Window() int {
	if r != nil && r.WindowBytes > 0 {
		return r.WindowBytes
	}
	return DefaultWindowBytes
}

func (r *Registry) WithStaged(staged []Attachment) (*Registry, error) {
	if len(staged) == 0 {
		return r, nil
	}
	if r == nil {
		return nil, errors.New("draft guardrail policy is unavailable")
	}
	out := r.Clone()
	out.Attachments = slices.Concat(slices.DeleteFunc(out.Attachments, func(a Attachment) bool {
		return slices.ContainsFunc(staged, a.sameSlot)
	}), staged)
	return out, nil
}

func (a Attachment) sameSlot(b Attachment) bool {
	return a.Scope == b.Scope && a.ScopeID == b.ScopeID && a.Order == b.Order
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
