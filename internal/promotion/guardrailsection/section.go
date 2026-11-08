package guardrail

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
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

func Parse(document contract.ConfigurationDocument) (Section, error) {
	if err := refuseLegacy(document); err != nil {
		return Section{}, err
	}
	definitions, err := parseGuardrails(valueOf(document.Guardrails))
	if err != nil {
		return Section{}, err
	}
	declared, attached := valueOf(document.GuardrailPolicies), valueOf(document.GuardrailAttachments)
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
	definitions, err := collect(sorted(section.Guardrails, func(g Guardrail) []string {
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
		Guardrails:           present(definitions),
		GuardrailPolicies:    present(mapped(policies, renderPolicy)),
		GuardrailAttachments: present(mapped(attachments, renderAttachment)),
	}, nil
}

func merge(sections []Section) Section {
	return Section{
		Guardrails:  slices.Concat(mapped(sections, func(s Section) []Guardrail { return s.Guardrails })...),
		Policies:    slices.Concat(mapped(sections, func(s Section) []Policy { return s.Policies })...),
		Attachments: slices.Concat(mapped(sections, func(s Section) []Attachment { return s.Attachments })...),
	}
}

func projectID(ctx context.Context, q access.Queryer, name string) (*string, error) {
	if name == "" {
		return nil, nil
	}
	var id string
	if err := q.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", name).Scan(&id); err != nil {
		return nil, err
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

func valueOf[T any](pointer *[]T) []T {
	if pointer == nil {
		return nil
	}
	return *pointer
}

func present[T any](values []T) *[]T {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func collect[A, B any](values []A, convert func(A) (B, error)) ([]B, error) {
	converted := make([]B, len(values))
	for i, value := range values {
		next, err := convert(value)
		if err != nil {
			return nil, err
		}
		converted[i] = next
	}
	return converted, nil
}

func each[A any](values []A, apply func(A) error) error {
	for _, value := range values {
		if err := apply(value); err != nil {
			return err
		}
	}
	return nil
}

func mapped[A, B any](values []A, convert func(A) B) []B {
	return slices.Collect(func(yield func(B) bool) {
		for _, value := range values {
			if !yield(convert(value)) {
				return
			}
		}
	})
}

func kept[A any](values []A, keep func(A) bool) []A {
	return slices.Collect(func(yield func(A) bool) {
		for _, value := range values {
			if keep(value) && !yield(value) {
				return
			}
		}
	})
}

func indexed[A any, K comparable, V any](values []A, entry func(A) (K, V)) map[K]V {
	return maps.Collect(func(yield func(K, V) bool) {
		for _, value := range values {
			if !yield(entry(value)) {
				return
			}
		}
	})
}

func sorted[A any](values []A, key func(A) []string) []A {
	return slices.SortedStableFunc(slices.Values(values), func(a, b A) int { return slices.Compare(key(a), key(b)) })
}

func duplicated[A any](values []A, key func(A) portable) bool {
	return len(indexed(values, func(value A) (portable, bool) { return key(value), true })) != len(values)
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
