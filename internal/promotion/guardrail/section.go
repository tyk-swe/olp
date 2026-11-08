package guardrail

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/promotion/project"
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
			current, err := readStored(ctx, q)
			if err != nil {
				return nil, err
			}
			policies, changed := planPolicies(desired, current)
			return slices.Concat(definitions, policies, planAttachments(desired.Attachments, current.Attachments, changed)), nil
		},
	}
}

func Read(ctx context.Context, q access.Queryer) (Section, error) {
	stored, err := readStored(ctx, q)
	if err != nil {
		return Section{}, err
	}
	legacy, err := readLegacyRoutes(ctx, q)
	if err != nil {
		return Section{}, err
	}
	return merge(append([]Section{stored}, legacy...)), nil
}

func Render(section Section) (Fragment, error) {
	definitions, err := promotion.TryMap(sorted(section.Guardrails, func(g Guardrail) []string {
		return []string{strings.ToLower(g.Project), strings.ToLower(g.Name), g.Project, g.Name}
	}), renderGuardrail)
	if err != nil {
		return Fragment{}, err
	}
	policies := sorted(section.Policies, func(p Policy) []string {
		return []string{strings.ToLower(p.Project), strings.ToLower(p.Name), p.Project, p.Name}
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

func projectID(directory project.Directory, name string) (*string, error) {
	if name == "" {
		return nil, nil
	}
	id, ok := directory.ID(name)
	if !ok {
		return nil, fmt.Errorf("project %q is not present at the destination", name)
	}
	return &id, nil
}

func installationOr(id *string) string {
	if id == nil {
		return "installation"
	}
	return *id
}

func optional[T any](ctx context.Context, q access.Queryer, scan func(pgx.Row) (T, error), sql string, args ...any) (*T, error) {
	value, err := scan(q.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func sorted[A any](values []A, key func(A) []string) []A {
	return slices.SortedStableFunc(slices.Values(values), func(a, b A) int { return slices.Compare(key(a), key(b)) })
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
