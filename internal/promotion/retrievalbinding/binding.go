package retrievalbinding

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/retrieval"
)

const (
	maxBindings  = 512
	maxKeyGrants = 2000
)

type Binding struct {
	Project        string
	Name           string
	Route          string
	TargetProvider string
	Backend        string
	Resource       string
	Fields         retrieval.Fields
	KeyGrants      []string
	Enabled        bool
}

type Desired struct {
	Bindings []Binding
	stages   map[string]stage
}

func Parse(document contract.ConfigurationDocument, names promotion.Names) (Desired, error) {
	entries := valueOf(document.RetrievalBindings)
	if len(entries) > maxBindings {
		return Desired{}, access.Invalid("retrieval_bindings", "Declare at most 512 retrieval bindings.")
	}
	bindings := make([]Binding, 0, len(entries))
	for _, entry := range entries {
		binding, err := parseEntry(entry, bindings)
		if err != nil {
			return Desired{}, err
		}
		bindings = append(bindings, binding)
	}
	sorted := slices.SortedFunc(slices.Values(bindings), func(a, b Binding) int { return cmp.Compare(key(a), key(b)) })
	return Desired{
		Bindings: sorted,
		stages: maps.Collect(func(yield func(string, stage) bool) {
			for _, binding := range sorted {
				if !yield(key(binding), stageOf(document, names, binding)) {
					return
				}
			}
		}),
	}, nil
}

func parseEntry(entry contract.ConfigurationRetrievalBinding, seen []Binding) (Binding, error) {
	binding := Binding{
		Project:        strings.TrimSpace(entry.Project),
		Name:           strings.TrimSpace(entry.Name),
		Route:          entry.Route,
		TargetProvider: entry.TargetProvider,
		Backend:        string(entry.Backend),
		Resource:       entry.BackendResource,
		Fields:         fieldsOf(entry.Fields),
		KeyGrants:      append([]string{}, slices.Compact(slices.Sorted(slices.Values(entry.KeyGrants)))...),
		Enabled:        entry.Enabled,
	}
	duplicate := slices.ContainsFunc(seen, func(other Binding) bool { return key(other) == key(binding) })
	if !promotion.GuardrailLabel(binding.Project) || !promotion.GuardrailLabel(binding.Name) || !promotion.GuardrailLabel(binding.TargetProvider) || binding.Route == "" || len(binding.Route) > 128 || duplicate || !retrieval.ResourceValid(binding.Backend, binding.Resource) {
		return Binding{}, invalidIdentity()
	}
	if err := retrieval.ValidateFields(binding.Backend, binding.Fields); err != nil {
		return Binding{}, access.Invalid("retrieval_bindings.fields", err.Error())
	}
	if len(entry.KeyGrants) > maxKeyGrants {
		return Binding{}, access.Invalid("retrieval_bindings.key_grants", "Use at most 2000 explicit key names.")
	}
	if !slices.ContainsFunc(binding.KeyGrants, func(name string) bool { return !promotion.GuardrailLabel(name) }) {
		return binding, nil
	}
	return Binding{}, access.Invalid("retrieval_bindings.key_grants", "Use nonempty exact project-local API-key names.")
}

func Render(bindings []Binding) *[]contract.ConfigurationRetrievalBinding {
	rendered := slices.Collect(func(yield func(contract.ConfigurationRetrievalBinding) bool) {
		for _, binding := range slices.SortedFunc(slices.Values(bindings), func(a, b Binding) int { return cmp.Compare(key(a), key(b)) }) {
			if !yield(renderBinding(binding)) {
				return
			}
		}
	})
	if len(rendered) == 0 {
		return nil
	}
	return &rendered
}

func renderBinding(binding Binding) contract.ConfigurationRetrievalBinding {
	return contract.ConfigurationRetrievalBinding{
		Project:         binding.Project,
		Name:            binding.Name,
		Route:           binding.Route,
		TargetProvider:  binding.TargetProvider,
		Backend:         contract.ConfigurationRetrievalBindingBackend(binding.Backend),
		BackendResource: binding.Resource,
		Fields: contract.RetrievalBindingFields{
			Text:     optional(binding.Fields.Text),
			Id:       optional(binding.Fields.ID),
			Title:    optional(binding.Fields.Title),
			Url:      optional(binding.Fields.URL),
			Search:   optionalSlice(binding.Fields.Search),
			Metadata: optionalMap(binding.Fields.Metadata),
		},
		KeyGrants: append([]string{}, slices.Sorted(slices.Values(binding.KeyGrants))...),
		Enabled:   binding.Enabled,
	}
}

func fieldsOf(fields contract.RetrievalBindingFields) retrieval.Fields {
	return retrieval.Fields{
		Text:     valueOf(fields.Text),
		ID:       valueOf(fields.Id),
		Title:    valueOf(fields.Title),
		URL:      valueOf(fields.Url),
		Search:   valueOf(fields.Search),
		Metadata: valueOf(fields.Metadata),
	}
}

func key(binding Binding) string {
	return strings.ToLower(binding.Project) + "\x00" + strings.ToLower(binding.Name)
}

func invalidIdentity() error {
	return access.Invalid("retrieval_bindings", "Use unique project/name pairs with an explicit route, provider and native backend resource.")
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalSlice(values []string) *[]string {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func optionalMap(values map[string]string) *map[string]string {
	if len(values) == 0 {
		return nil
	}
	return &values
}

func valueOf[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}
