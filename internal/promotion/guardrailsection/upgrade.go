package guardrail

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type converted struct {
	guardrails  []contract.ConfigurationGuardrail
	policies    []contract.ConfigurationGuardrailPolicyEntry
	attachments []contract.ConfigurationGuardrailAttachment
}

func Upgrade(document contract.ConfigurationDocument) (contract.ConfigurationDocument, error) {
	if document.ApiVersion != contract.OpenllmproxyDevconfigv1 {
		return document, nil
	}
	if len(valueOf(document.GuardrailPolicies)) > 0 || len(valueOf(document.GuardrailAttachments)) > 0 {
		return contract.ConfigurationDocument{}, access.Invalid("guardrails", "Version 1 cannot mix new guardrail policies with legacy content policies.")
	}
	if slices.ContainsFunc(valueOf(document.Guardrails), func(g contract.ConfigurationGuardrail) bool { return g.Configuration != nil }) {
		return contract.ConfigurationDocument{}, access.Invalid("guardrails", "Version 1 cannot contain typed guardrail configuration.")
	}
	definitions, err := collect(valueOf(document.Guardrails), upgradeGuardrail)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	routes, err := collect(document.Routes, upgradeRoute)
	if err != nil {
		return contract.ConfigurationDocument{}, err
	}
	upgraded := document
	upgraded.ApiVersion = contract.OpenllmproxyDevconfigv2
	upgraded.Routes = mapped(document.Routes, withoutContentPolicy)
	upgraded.Guardrails = present(slices.Concat(append([][]contract.ConfigurationGuardrail{definitions}, mapped(routes, func(c converted) []contract.ConfigurationGuardrail { return c.guardrails })...)...))
	upgraded.GuardrailPolicies = present(slices.Concat(mapped(routes, func(c converted) []contract.ConfigurationGuardrailPolicyEntry { return c.policies })...))
	upgraded.GuardrailAttachments = present(slices.Concat(mapped(routes, func(c converted) []contract.ConfigurationGuardrailAttachment { return c.attachments })...))
	return upgraded, nil
}

func refuseLegacy(document contract.ConfigurationDocument) error {
	if slices.ContainsFunc(document.Routes, func(r contract.ConfigurationRouteEntry) bool {
		_, set := promotion.Present(r.ContentPolicy)
		return set
	}) {
		return access.Invalid("content_policy", "Use version 2 guardrail definitions, policies and attachments.")
	}
	if slices.ContainsFunc(valueOf(document.Guardrails), func(g contract.ConfigurationGuardrail) bool { return g.Policy != nil }) {
		return access.Invalid("guardrails", "Use typed configuration in version 2.")
	}
	return nil
}

func upgradeGuardrail(g contract.ConfigurationGuardrail) (contract.ConfigurationGuardrail, error) {
	if g.Policy == nil {
		return g, nil
	}
	rules := mapped(g.Policy.Rules, func(r contract.ContentPolicyRule) contentpolicy.Rule {
		return contentpolicy.Rule{ID: r.Id, Phase: string(r.Phase), Pattern: r.Pattern, Action: string(r.Action), Replacement: dereference(r.Replacement)}
	})
	if contentpolicy.Validate(&contentpolicy.Policy{Rules: rules}) != nil {
		return contract.ConfigurationGuardrail{}, access.Invalid("guardrails", "Use valid bounded RE2 block or redact rules.")
	}
	return regexGuardrail(g.Project, g.Name, g.Type, rules, 1000), nil
}

func upgradeRoute(route contract.ConfigurationRouteEntry) (converted, error) {
	policy, set := promotion.Present(route.ContentPolicy)
	if !set {
		return converted{}, nil
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		return converted{}, access.Invalid("content_policy", "Invalid legacy content policy.")
	}
	project, _ := promotion.Present(route.Project)
	return legacyRoute(route.Slug, project, raw, int64(route.OverallTimeoutMs))
}

func withoutContentPolicy(route contract.ConfigurationRouteEntry) contract.ConfigurationRouteEntry {
	stripped := route
	stripped.ContentPolicy = nil
	return stripped
}

func legacyRoute(slug, project string, raw json.RawMessage, overallMS int64) (converted, error) {
	policy, err := contentpolicy.Decode(raw)
	if err != nil {
		return converted{}, access.Invalid("content_policy", "Invalid legacy content policy.")
	}
	if len(policy.Rules) == 0 {
		return converted{}, nil
	}
	name := bounded("route." + slug + ".content")
	definition := func(r contentpolicy.Rule) string { return bounded(name + "." + r.ID) }
	return converted{
		guardrails: mapped(policy.Rules, func(r contentpolicy.Rule) contract.ConfigurationGuardrail {
			return regexGuardrail(project, definition(r), contract.ConfigurationGuardrailTypeBuiltinRegex, []contentpolicy.Rule{r}, min(1000, overallMS))
		}),
		policies: []contract.ConfigurationGuardrailPolicyEntry{{Name: name, Project: project, Entries: mapped(policy.Rules, func(r contentpolicy.Rule) rule {
			return rule{Action: contract.ConfigurationGuardrailPolicyEntryEntriesAction(r.Action), Guardrail: definition(r), Mandatory: when(true), Phase: contract.ConfigurationGuardrailPolicyEntryEntriesPhase(r.Phase)}
		})}},
		attachments: []contract.ConfigurationGuardrailAttachment{{Scope: contract.ConfigurationGuardrailAttachmentScopeRoute, Name: filled(slug), Project: filled(project), Policy: name, PolicyProject: project}},
	}, nil
}

func regexGuardrail(project, name string, kind contract.ConfigurationGuardrailType, rules []contentpolicy.Rule, timeoutMS int64) contract.ConfigurationGuardrail {
	configuration := map[string]any{"rules": mapped(rules, func(r contentpolicy.Rule) map[string]any {
		return map[string]any{"id": r.ID, "pattern": r.Pattern, "replacement": r.Replacement}
	})}
	onError := contract.ConfigurationGuardrailOnError(guardrails.FailClosed)
	return contract.ConfigurationGuardrail{Configuration: &configuration, Name: name, OnError: &onError, Project: project, TimeoutMs: &timeoutMS, Type: kind}
}

func parseConverted(c converted) (Section, error) {
	definitions, err := collect(c.guardrails, parseGuardrail)
	if err != nil {
		return Section{}, err
	}
	attachments := mapped(c.attachments, func(a contract.ConfigurationGuardrailAttachment) Attachment {
		return Attachment{Scope: Route{dereference(a.Name)}, Policy: PolicyRef{a.PolicyProject, a.Policy}, Order: a.Order}
	})
	return Section{Guardrails: definitions, Policies: mapped(c.policies, parsePolicy), Attachments: attachments}, nil
}

func bounded(name string) string {
	if len(name) <= 85 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:64] + "-" + hex.EncodeToString(sum[:8])
}
