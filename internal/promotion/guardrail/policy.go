package guardrail

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
	"github.com/tyk-swe/olp/internal/routing"
)

type Policy struct {
	Project string
	Name    string
	Entries []Entry
}

type Entry struct {
	Guardrail string
	Phase     string
	Action    string
	Monitor   bool
	Mandatory bool
}

type catalog struct {
	list    []Policy
	engines map[portable]*guardrails.Engine
}

func parsePolicies(declared []contract.ConfigurationGuardrailPolicyEntry, definitions []Guardrail) (catalog, error) {
	type compiled struct {
		key    portable
		engine *guardrails.Engine
	}
	byKey := promotion.Index(definitions, guardrailKey)
	list := promotion.Map(declared, parsePolicy)
	first := promotion.FirstIndex(list, policyKey)
	engines, err := promotion.TryMapIndex(list, func(i int, p Policy) (compiled, error) {
		key := policyKey(p)
		engine, err := compilePolicy(p, first[key] != i, byKey)
		return compiled{key, engine}, err
	})
	if err != nil {
		return catalog{}, err
	}
	return catalog{list: list, engines: promotion.Associate(engines, func(c compiled) (portable, *guardrails.Engine) { return c.key, c.engine })}, nil
}

func parsePolicy(p contract.ConfigurationGuardrailPolicyEntry) Policy {
	return Policy{Project: p.Project, Name: p.Name, Entries: promotion.Map(p.Entries, func(e rule) Entry {
		return Entry{Guardrail: e.Guardrail, Phase: string(e.Phase), Action: string(e.Action), Monitor: promotion.ValueOf(e.Monitor), Mandatory: promotion.ValueOf(e.Mandatory)}
	})}
}

func compilePolicy(p Policy, duplicate bool, definitions map[portable]Guardrail) (*guardrails.Engine, error) {
	if duplicate || !promotion.ValidLabel(p.Name) || len(p.Entries) > guardrails.MaxEntries {
		return nil, access.Invalid("guardrail_policies", "Use unique bounded policy names and ordered entries.")
	}
	type resolvedEntry struct {
		Entry
		definition Guardrail
	}
	resolved, err := promotion.TryMap(p.Entries, func(e Entry) (resolvedEntry, error) {
		g, ok := definitions[keyOf(p.Project, e.Guardrail)]
		if !ok {
			return resolvedEntry{}, access.Invalid("guardrail_policies", "Declare every referenced guardrail in the policy's project.")
		}
		return resolvedEntry{e, g}, nil
	})
	if err != nil {
		return nil, err
	}
	revisions := promotion.Associate(resolved, func(r resolvedEntry) (string, guardrails.Revision) {
		g := r.definition
		return g.Name, guardrails.Revision{ID: g.Name, GuardrailID: g.Name, Type: g.Type, Configuration: g.Configuration, TimeoutMS: g.TimeoutMS, OnError: g.OnError}
	})
	entries := promotion.Map(resolved, func(r resolvedEntry) guardrails.Entry {
		return guardrails.Entry{GuardrailID: r.definition.Name, RevisionID: r.definition.Name, Phase: r.Phase, Action: r.Action, Monitor: r.Monitor, Mandatory: r.Mandatory}
	})
	engine, err := guardrails.Compile(entries, revisions, nil)
	if err != nil {
		return nil, access.Invalid("guardrail_policies", "Invalid guardrail entries.")
	}
	return engine, nil
}

func checkRoutes(document contract.ConfigurationDocument, policies catalog, attachments []Attachment) error {
	organizations := promotion.Associate(document.Projects, func(p contract.ConfigurationProjectEntry) (string, string) {
		organization, _ := promotion.Present(p.Organization)
		return strings.ToLower(p.Name), organization
	})
	return promotion.Every(attachments, func(a Attachment) error {
		engine := policies.engines[keyOf(a.Policy.Project, a.Policy.Name)]
		return promotion.Every(document.Routes, func(route contract.ConfigurationRouteEntry) error {
			project, _ := promotion.Present(route.Project)
			if !applies(a, route.Slug, project, organizations[strings.ToLower(project)]) {
				return nil
			}
			return checkRoute(engine, route)
		})
	})
}

func applies(a Attachment, slug, project, organization string) bool {
	switch scope := a.Scope.(type) {
	case Installation:
		return true
	case Project:
		return strings.EqualFold(scope.Name, project)
	case Route:
		return scope.Slug == slug && strings.EqualFold(a.Policy.Project, project)
	case APIKey:
		return strings.EqualFold(scope.Project, project)
	case Organization:
		return organization != "" && strings.EqualFold(scope.Name, organization)
	}
	return false
}

func checkRoute(engine *guardrails.Engine, route contract.ConfigurationRouteEntry) error {
	if _, passthrough := promotion.Present(route.Passthrough); passthrough {
		return access.Fail(422, "passthrough_conflict", "A pass-through route cannot carry guardrails.")
	}
	raw, err := fidelityOf(route)
	if err != nil {
		return nil
	}
	fidelity, err := routing.DecodeFidelity(raw)
	if err != nil {
		return nil
	}
	if engine.ValidateRoute(fidelity.Strict(), int64(route.OverallTimeoutMs)) != nil {
		return access.Invalid("guardrail_policies", "Guardrails are incompatible with an attached route's fidelity or deadline.")
	}
	return nil
}

func fidelityOf(route contract.ConfigurationRouteEntry) ([]byte, error) {
	fidelity, ok := promotion.Present(route.Fidelity)
	if !ok {
		return nil, nil
	}
	return json.Marshal(fidelity)
}

func renderPolicy(p Policy) contract.ConfigurationGuardrailPolicyEntry {
	return contract.ConfigurationGuardrailPolicyEntry{Name: p.Name, Project: p.Project, Entries: promotion.Map(p.Entries, toRule)}
}

type rule = struct {
	Action    contract.ConfigurationGuardrailPolicyEntryEntriesAction `json:"action"`
	Guardrail string                                                  `json:"guardrail"`
	Mandatory *bool                                                   `json:"mandatory,omitempty"`
	Monitor   *bool                                                   `json:"monitor,omitempty"`
	Phase     contract.ConfigurationGuardrailPolicyEntryEntriesPhase  `json:"phase"`
}

func toRule(e Entry) rule {
	return rule{Action: contract.ConfigurationGuardrailPolicyEntryEntriesAction(e.Action), Guardrail: e.Guardrail, Mandatory: promotion.NonZero(e.Mandatory), Monitor: promotion.NonZero(e.Monitor), Phase: contract.ConfigurationGuardrailPolicyEntryEntriesPhase(e.Phase)}
}

func planPolicies(desired, current Section) ([]promotion.Item, map[portable]bool) {
	stored := promotion.Index(current.Policies, policyKey)
	before := promotion.Index(current.Guardrails, guardrailKey)
	after := promotion.Index(desired.Guardrails, guardrailKey)
	action := func(p Policy) contract.ConfigurationPlanItemAction {
		old, ok := stored[keyOf(p.Project, p.Name)]
		if !ok {
			return contract.ConfigurationPlanItemActionCreate
		}
		if !slices.Equal(old.Entries, p.Entries) || slices.ContainsFunc(p.Entries, func(e Entry) bool { return redefined(before, after, keyOf(p.Project, e.Guardrail)) }) {
			return contract.ConfigurationPlanItemActionRevision
		}
		return contract.ConfigurationPlanItemActionReuse
	}
	items := promotion.Map(desired.Policies, func(p Policy) promotion.Item {
		return item(contract.ConfigurationPlanItemKindGuardrailPolicy, p.Project+"/"+p.Name, action(p), "Pin the ordered entries to immutable definition revisions.")
	})
	changed := promotion.Associate(desired.Policies, func(p Policy) (portable, bool) {
		return policyKey(p), action(p) != contract.ConfigurationPlanItemActionReuse
	})
	return items, changed
}

func redefined(before, after map[portable]Guardrail, key portable) bool {
	old, ok := before[key]
	next := after[key]
	return !ok || old.Type != next.Type || !equal(old, next)
}

type storedPolicy struct {
	revision guardrails.PolicyRevision
	active   *string
}

func applyPolicy(ctx context.Context, tx pgx.Tx, p access.Principal, directory project.Directory, policy Policy) error {
	project, err := projectID(directory, policy.Project)
	if err != nil {
		return err
	}
	if err := p.Project(project, access.Change); err != nil {
		return err
	}
	entries, err := promotion.TryMap(policy.Entries, func(e Entry) (guardrails.Entry, error) { return pin(ctx, tx, project, e) })
	if err != nil {
		return err
	}
	current, err := optional(ctx, tx, func(row pgx.Row) (storedPolicy, error) {
		var s storedPolicy
		err := row.Scan(&s.revision.PolicyID, &s.revision.ID, &s.revision.Name, &s.revision.Revision, &s.revision.Entries, &s.active)
		return s, err
	}, "SELECT p.id::text,v.id::text,p.name,v.revision,v.entries,p.active_revision_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.latest_revision_id WHERE p.project_id IS NOT DISTINCT FROM $1::uuid AND lower(p.name)=lower($2) AND p.retired_at IS NULL", project, policy.Name)
	if err != nil {
		return err
	}
	if current != nil && slices.Equal(current.revision.Entries, entries) {
		if current.active != nil && *current.active == current.revision.ID {
			return nil
		}
		return guardrails.ActivatePolicy(ctx, tx, p.UserID(), current.revision.PolicyID, current.revision.ID)
	}
	saved, err := guardrails.SavePolicy(ctx, tx, p.UserID(), latest(current), guardrails.PolicyRevision{ProjectID: installationOr(project), Name: retainedName(current, policy.Name), Entries: entries})
	if err != nil {
		return err
	}
	return guardrails.ActivatePolicy(ctx, tx, p.UserID(), saved.PolicyID, saved.ID)
}

func pin(ctx context.Context, q access.Queryer, project *string, e Entry) (guardrails.Entry, error) {
	var id, revision string
	err := q.QueryRow(ctx, "SELECT id::text,active_revision_id::text FROM olp.guardrails WHERE project_id IS NOT DISTINCT FROM $1::uuid AND lower(name)=lower($2) AND retired_at IS NULL", project, e.Guardrail).Scan(&id, &revision)
	return guardrails.Entry{GuardrailID: id, RevisionID: revision, Phase: e.Phase, Action: e.Action, Monitor: e.Monitor, Mandatory: e.Mandatory}, err
}

func latest(current *storedPolicy) *guardrails.PolicyRevision {
	if current == nil {
		return nil
	}
	return &current.revision
}

func retainedName(current *storedPolicy, name string) string {
	if current == nil {
		return name
	}
	return current.revision.Name
}
