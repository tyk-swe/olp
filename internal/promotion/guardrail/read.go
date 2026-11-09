package guardrail

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routes"
)

type named struct{ id, name string }

type slot struct {
	slug  string
	order int
}

type namer func(string) (string, error)

func readStored(ctx context.Context, q access.Queryer, projectName namer) (Section, error) {
	active, err := guardrails.ReadUnmanagedActiveRevisions(ctx, q)
	if err != nil {
		return Section{}, err
	}
	registry, err := guardrails.ReadRegistry(ctx, q)
	if err != nil {
		return Section{}, err
	}
	platform, err := readPlatform(ctx, q, registry, projectName, active)
	if err != nil {
		return Section{}, err
	}
	stored, err := promotion.TryMap(active, func(r guardrails.Revision) (Guardrail, error) { return fromRevision(projectName, r.Name, r) })
	if err != nil {
		return Section{}, err
	}
	definitions, err := promotion.TryMap(slices.Concat(stored, platform.Guardrails), func(g Guardrail) (Guardrail, error) {
		configuration, err := rewriteCredentials(g.Type, g.Configuration, credentialName(ctx, q))
		return Guardrail{Project: g.Project, Name: g.Name, Type: g.Type, Configuration: configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError}, err
	})
	return Section{Guardrails: definitions, Policies: platform.Policies, Attachments: platform.Attachments}, err
}

func fromRevision(projectName namer, name string, r guardrails.Revision) (Guardrail, error) {
	owner, err := projectName(r.ProjectID)
	return Guardrail{Project: owner, Name: name, Type: r.Type, Configuration: r.Configuration, TimeoutMS: r.TimeoutMS, OnError: r.OnError}, err
}

func readPlatform(ctx context.Context, q access.Queryer, registry *guardrails.Registry, projectName namer, activeRevisions []guardrails.Revision) (Section, error) {
	if registry == nil {
		return Section{}, nil
	}
	revisions, err := guardrails.ReadUnmanagedPolicyRevisions(ctx, q)
	if err != nil {
		return Section{}, err
	}
	policyNames, err := promotion.TryMap(revisions, func(r guardrails.NamedPolicyRevision) (named, error) { return policyName(registry, r) })
	if err != nil {
		return Section{}, err
	}
	policies := promotion.Associate(policyNames, func(n named) (string, string) { return n.id, n.name })
	ids := slices.Sorted(maps.Keys(policies))
	active := promotion.Associate(activeRevisions, func(r guardrails.Revision) (string, string) { return r.ID, r.Name })
	pinned := promotion.Associate(promotion.Filter(slices.Concat(promotion.Map(ids, func(id string) []string {
		return promotion.Map(registry.Policies[id].Entries, func(e guardrails.Entry) string { return e.RevisionID })
	})...), func(revision string) bool { _, ok := active[revision]; return !ok }), func(revision string) (string, string) {
		return revision, pinnedName(registry.Definitions[revision])
	})
	definitionName := func(revision string) string { return cmp.Or(active[revision], pinned[revision]) }
	reference := func(revision string) (PolicyRef, error) {
		owner, err := projectName(registry.Policies[revision].ProjectID)
		return PolicyRef{Project: owner, Name: policies[revision]}, err
	}
	attachments, err := promotion.TryMap(promotion.Filter(registry.Attachments, func(a guardrails.Attachment) bool { _, ok := policies[a.PolicyRevisionID]; return ok }), func(a guardrails.Attachment) (Attachment, error) {
		return readAttachment(ctx, q, projectName, reference, a)
	})
	if err != nil {
		return Section{}, err
	}
	staged, err := readDrafts(ctx, q, policies, reference)
	if err != nil {
		return Section{}, err
	}
	definitions, err := promotion.TryMap(slices.Sorted(maps.Keys(pinned)), func(revision string) (Guardrail, error) {
		return fromRevision(projectName, pinned[revision], registry.Definitions[revision])
	})
	if err != nil {
		return Section{}, err
	}
	declared, err := promotion.TryMap(ids, func(id string) (Policy, error) {
		p := registry.Policies[id]
		owner, err := projectName(p.ProjectID)
		return Policy{Project: owner, Name: policies[id], Entries: promotion.Map(p.Entries, func(e guardrails.Entry) Entry {
			return Entry{Guardrail: definitionName(e.RevisionID), Phase: e.Phase, Action: e.Action, Monitor: e.Monitor, Mandatory: e.Mandatory}
		})}, err
	})
	if err != nil {
		return Section{}, err
	}
	return Section{
		Guardrails:  definitions,
		Policies:    declared,
		Attachments: slices.Concat(promotion.Filter(attachments, func(a Attachment) bool { _, replaced := staged[routeSlot(a)]; return !replaced }), slices.Collect(maps.Values(staged))),
	}, nil
}

func policyName(registry *guardrails.Registry, r guardrails.NamedPolicyRevision) (named, error) {
	if r.Active {
		return named{r.ID, r.Name}, nil
	}
	type portableRule struct {
		Type          guardrails.Kind
		Config        json.RawMessage
		Timeout       int64
		OnError       guardrails.OnError
		Phase, Action string
		Monitor       bool
		Mandatory     bool
	}
	content, err := json.Marshal(promotion.Map(registry.Policies[r.ID].Entries, func(e guardrails.Entry) portableRule {
		d := registry.Definitions[e.RevisionID]
		return portableRule{d.Type, d.Configuration, d.TimeoutMS, d.OnError, e.Phase, e.Action, e.Monitor, e.Mandatory}
	}))
	if err != nil {
		return named{}, err
	}
	sum := sha256.Sum256(content)
	return named{r.ID, bounded(r.Name + ".revision." + hex.EncodeToString(sum[:6]))}, nil
}

func pinnedName(d guardrails.Revision) string {
	sum := sha256.Sum256(slices.Concat([]byte(string(d.Type)+string(d.OnError)+strconv.FormatInt(d.TimeoutMS, 10)), d.Configuration))
	return bounded(d.Name + ".revision." + hex.EncodeToString(sum[:6]))
}

func projectNamer(directory project.Directory) namer {
	return func(id string) (string, error) {
		if id == "installation" {
			return "", nil
		}
		name, ok := directory.Name(id)
		if !ok {
			return "", fmt.Errorf("guardrail registry references unknown project %s", id)
		}
		return name, nil
	}
}

func readProjectNamer(ctx context.Context, q access.Queryer) (namer, error) {
	directory, err := project.ReadDirectory(ctx, q)
	return projectNamer(directory), err
}

func readAttachment(ctx context.Context, q access.Queryer, projectName namer, reference func(string) (PolicyRef, error), a guardrails.Attachment) (Attachment, error) {
	policy, err := reference(a.PolicyRevisionID)
	if err != nil {
		return Attachment{}, err
	}
	name, owner, err := scopeName(ctx, q, projectName, a)
	if err != nil {
		return Attachment{}, err
	}
	scope, valid := parseScope(contract.ConfigurationGuardrailAttachmentScope(a.Scope), name, owner)
	if !valid {
		return Attachment{}, fmt.Errorf("guardrail attachment has unknown scope %q", a.Scope)
	}
	return Attachment{Scope: scope, Policy: policy, Order: a.Order}, nil
}

func scopeName(ctx context.Context, q access.Queryer, projectName namer, a guardrails.Attachment) (string, string, error) {
	unowned := func(name string, err error) (string, string, error) { return name, "", err }
	switch contract.ConfigurationGuardrailAttachmentScope(a.Scope) {
	case contract.ConfigurationGuardrailAttachmentScopeProject:
		return unowned(projectName(a.ScopeID))
	case contract.ConfigurationGuardrailAttachmentScopeOrganization:
		return unowned(projects.OrganizationName(ctx, q, a.ScopeID))
	case contract.ConfigurationGuardrailAttachmentScopeRoute:
		return unowned(routes.RouteSlug(ctx, q, a.ScopeID))
	case contract.ConfigurationGuardrailAttachmentScopeToolset:
		return unowned(publications.ToolsetName(ctx, q, a.ScopeID))
	case contract.ConfigurationGuardrailAttachmentScopeApiKey:
		key, err := access.APIKeyName(ctx, q, a.ScopeID)
		if err != nil {
			return "", "", err
		}
		owner, err := projectName(key.ProjectID)
		return key.Name, owner, err
	}
	return "", "", nil
}

func readDrafts(ctx context.Context, q access.Queryer, policies map[string]string, reference func(string) (PolicyRef, error)) (map[slot]Attachment, error) {
	drafts, err := guardrails.ReadDraftAttachments(ctx, q)
	if err != nil {
		return nil, err
	}
	staged, err := promotion.TryMap(drafts, func(d guardrails.DraftAttachment) (Attachment, error) {
		if _, ok := policies[d.PolicyRevisionID]; !ok {
			return Attachment{}, access.Invalid("guardrails", "A staged policy is unavailable.")
		}
		policy, err := reference(d.PolicyRevisionID)
		return Attachment{Scope: Route{d.Slug}, Policy: policy, Order: d.Position}, err
	})
	return promotion.Associate(staged, func(a Attachment) (slot, Attachment) { return routeSlot(a), a }), err
}

func routeSlot(a Attachment) slot {
	route, ok := a.Scope.(Route)
	if !ok {
		return slot{order: -1}
	}
	return slot{route.Slug, a.Order}
}

func readLegacyRoutes(ctx context.Context, q access.Queryer, projectName namer) ([]Section, error) {
	legacy, err := routes.ReadLegacyContentPolicies(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.TryMap(legacy, func(r routes.LegacyContentPolicy) (Section, error) {
		owner, err := projectName(installationOr(r.ProjectID))
		if err != nil {
			return Section{}, err
		}
		c, err := legacyRoute(r.Slug, owner, r.ContentPolicy, int64(r.OverallTimeoutMS))
		if err != nil {
			return Section{}, err
		}
		return parseConverted(c)
	})
}
