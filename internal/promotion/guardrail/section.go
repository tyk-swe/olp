package guardrail

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
)

type Section struct {
	Guardrails  []Guardrail
	Policies    []Policy
	Attachments []Attachment
}

type Fragment struct {
	Guardrails           *[]contract.ConfigurationGuardrail
	GuardrailPolicies    *[]contract.ConfigurationGuardrailPolicyEntry
	GuardrailAttachments *[]contract.ConfigurationGuardrailAttachment
}

type portable struct{ project, name string }

func keyOf(project, name string) portable {
	return portable{strings.ToLower(project), strings.ToLower(name)}
}

func guardrailKey(g Guardrail) portable { return keyOf(g.Project, g.Name) }

func policyKey(p Policy) portable { return keyOf(p.Project, p.Name) }

func Parse(document contract.ConfigurationDocument) (Section, error) {
	if err := refuseLegacy(document); err != nil {
		return Section{}, err
	}
	definitions, err := parseGuardrails(promotion.ValueOf(document.Guardrails))
	if err != nil {
		return Section{}, err
	}
	declared, attached := promotion.ValueOf(document.GuardrailPolicies), promotion.ValueOf(document.GuardrailAttachments)
	if len(declared) > 1000 || len(attached) > 10000 {
		return Section{}, access.Invalid("guardrail_policies", "Too many policies or attachments.")
	}
	policies, err := parsePolicies(declared, definitions)
	if err != nil {
		return Section{}, err
	}
	attachments, err := parseAttachments(attached, policies)
	if err != nil {
		return Section{}, err
	}
	if err := checkRoutes(document, policies, attachments); err != nil {
		return Section{}, err
	}
	return Section{Guardrails: definitions, Policies: policies.list, Attachments: attachments}, nil
}

func Planner(desired Section, names promotion.Names) promotion.Planner {
	return promotion.Planner{
		Claims: claims(desired.Guardrails),
		Plan: func(ctx context.Context, q access.Queryer) ([]promotion.Item, error) {
			definitions, err := planGuardrails(ctx, q, desired.Guardrails, names)
			if err != nil {
				return nil, err
			}
			if len(desired.Policies) == 0 && len(desired.Attachments) == 0 {
				return definitions, nil
			}
			projectName, err := readProjectNamer(ctx, q)
			if err != nil {
				return nil, err
			}
			current, err := readStored(ctx, q, projectName)
			if err != nil {
				return nil, err
			}
			policies, changed := planPolicies(desired, current)
			return slices.Concat(definitions, policies, planAttachments(desired.Attachments, current.Attachments, changed)), nil
		},
	}
}

func Read(ctx context.Context, q access.Queryer) (Section, error) {
	projectName, err := readProjectNamer(ctx, q)
	if err != nil {
		return Section{}, err
	}
	stored, err := readStored(ctx, q, projectName)
	if err != nil {
		return Section{}, err
	}
	legacy, err := readLegacyRoutes(ctx, q, projectName)
	if err != nil {
		return Section{}, err
	}
	return merge(append([]Section{stored}, legacy...)), nil
}

func Render(section Section) (Fragment, error) {
	definitions, err := promotion.TryMap(slices.SortedStableFunc(slices.Values(section.Guardrails), func(a, b Guardrail) int {
		return compareOwned(a.Project, a.Name, b.Project, b.Name)
	}), renderGuardrail)
	if err != nil {
		return Fragment{}, err
	}
	policies := slices.SortedStableFunc(slices.Values(section.Policies), func(a, b Policy) int {
		return compareOwned(a.Project, a.Name, b.Project, b.Name)
	})
	attachments := slices.SortedFunc(slices.Values(section.Attachments), compareAttachments)
	return Fragment{
		Guardrails:           promotion.Optional(definitions),
		GuardrailPolicies:    promotion.Optional(promotion.Map(policies, renderPolicy)),
		GuardrailAttachments: promotion.Optional(promotion.Map(attachments, renderAttachment)),
	}, nil
}

func merge(sections []Section) Section {
	return Section{
		Guardrails:  slices.Concat(promotion.Map(sections, func(s Section) []Guardrail { return s.Guardrails })...),
		Policies:    slices.Concat(promotion.Map(sections, func(s Section) []Policy { return s.Policies })...),
		Attachments: slices.Concat(promotion.Map(sections, func(s Section) []Attachment { return s.Attachments })...),
	}
}

func projectID(directory promotion.Directory, name string) (*string, error) {
	if name == "" {
		return nil, nil
	}
	id, ok := directory.ID(name)
	if !ok {
		return nil, fmt.Errorf("project %q is not present at the destination", name)
	}
	return &id, nil
}

func compareOwned(aProject, aName, bProject, bName string) int {
	return cmp.Or(
		cmp.Compare(strings.ToLower(aProject), strings.ToLower(bProject)),
		cmp.Compare(strings.ToLower(aName), strings.ToLower(bName)),
		cmp.Compare(aProject, bProject),
		cmp.Compare(aName, bName),
	)
}

func compareAttachments(a, b Attachment) int {
	x, y := describe(a.Scope), describe(b.Scope)
	return cmp.Or(
		cmp.Compare(x.kind, y.kind),
		cmp.Compare(strings.ToLower(x.name), strings.ToLower(y.name)),
		cmp.Compare(strings.ToLower(x.project), strings.ToLower(y.project)),
		cmp.Compare(a.Order, b.Order),
		cmp.Compare(strings.ToLower(a.Policy.Project), strings.ToLower(b.Policy.Project)),
		cmp.Compare(strings.ToLower(a.Policy.Name), strings.ToLower(b.Policy.Name)),
		cmp.Compare(x.name+x.project+a.Policy.Project+a.Policy.Name, y.name+y.project+b.Policy.Project+b.Policy.Name),
	)
}
