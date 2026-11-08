package sink

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/sinks"
)

const maxSinks = 64

type Owner interface{ owner() }

type Installation struct{}

type Project struct{ Name string }

func (Installation) owner() {}
func (Project) owner()      {}

type Sink struct {
	Owner       Owner
	Name        string
	Destination string
	Streams     []string
	Enabled     bool
	Signed      bool
}

type key struct{ project, name string }

func (s Sink) key() key {
	switch owner := s.Owner.(type) {
	case Project:
		return key{project: strings.ToLower(owner.Name), name: strings.ToLower(s.Name)}
	default:
		return key{name: strings.ToLower(s.Name)}
	}
}

func (s Sink) ref() promotion.Ref {
	k := s.key()
	encoded, _ := json.Marshal([2]string{k.project, k.name})
	sum := sha256.Sum256(encoded)
	return promotion.Ref("sink/" + hex.EncodeToString(sum[:]))
}

func (s Sink) spec(projectID *string) sinks.Spec {
	return sinks.Spec{ProjectID: projectID, Name: s.Name, Type: string(contract.ConfigurationSinkTypeHttps), Destination: s.Destination, Streams: s.Streams, Enabled: s.Enabled}
}

type entry struct {
	sink Sink
	kind string
	ref  *string
}

func Parse(document contract.ConfigurationDocument, policy *egress.Policy) ([]Sink, error) {
	raw := valueOf(document.Sinks)
	if len(raw) > maxSinks {
		return nil, access.Invalid("sinks", "Declare at most 64 export sinks.")
	}
	entries := slices.Collect(mapped(raw, trim))
	first := firstIndex(entries, func(e entry) key { return e.sink.key() })
	return settle(slices.Collect(func(yield func(outcome[Sink]) bool) {
		for i, e := range entries {
			parsed, err := parseSink(e, first[e.sink.key()] != i, policy)
			if !yield(outcome[Sink]{parsed, err}) {
				return
			}
		}
	}))
}

func trim(raw contract.ConfigurationSink) entry {
	project, scoped := promotion.Present(raw.Project)
	ref, signed := promotion.Present(raw.CredentialRef)
	return entry{
		sink: Sink{
			Owner:       ownerOf(strings.TrimSpace(project), scoped),
			Name:        strings.TrimSpace(raw.Name),
			Destination: raw.Destination,
			Streams:     slices.Collect(mapped(raw.Streams, func(stream contract.ConfigurationSinkStreams) string { return string(stream) })),
			Enabled:     raw.Enabled,
			Signed:      signed,
		},
		kind: string(raw.Type),
		ref:  nonNil(ref, signed),
	}
}

func parseSink(e entry, duplicate bool, policy *egress.Policy) (Sink, error) {
	if project, scoped := e.sink.Owner.(Project); scoped && !label(project.Name) {
		return Sink{}, access.Invalid("sinks.project", "Use an owning project name.")
	}
	if duplicate {
		return Sink{}, access.Invalid("sinks", "Sink names must be unique inside each scope.")
	}
	spec, err := sinks.NormalizeSpec(sinks.Spec{ProjectID: scope(e.sink.Owner), Name: e.sink.Name, Type: e.kind, Destination: e.sink.Destination, Streams: e.sink.Streams, Enabled: e.sink.Enabled}, policy)
	if err != nil {
		return Sink{}, err
	}
	parsed := Sink{Owner: e.sink.Owner, Name: spec.Name, Destination: spec.Destination, Streams: slices.Sorted(slices.Values(spec.Streams)), Enabled: spec.Enabled, Signed: e.sink.Signed}
	if e.ref != nil && promotion.Ref(*e.ref) != parsed.ref() {
		return Sink{}, access.Invalid("sinks.credential_ref", "Use the deterministic exported signing-credential reference.")
	}
	return parsed, nil
}

func label(value string) bool {
	length := utf8.RuneCountInString(value)
	return utf8.ValidString(value) && length >= 1 && length <= 100 && !strings.ContainsFunc(value, unicode.IsControl)
}

func ownerOf(project string, scoped bool) Owner {
	if !scoped {
		return Installation{}
	}
	return Project{Name: project}
}

func scope(owner Owner) *string {
	switch scoped := owner.(type) {
	case Project:
		return &scoped.Name
	default:
		return nil
	}
}

type stored struct {
	id         string
	credential *string
	sink       Sink
}

func Read(ctx context.Context, q access.Queryer) ([]Sink, error) {
	current, err := readStored(ctx, q)
	if err != nil {
		return nil, err
	}
	return slices.Collect(mapped(current, func(s stored) Sink { return s.sink })), nil
}

func readStored(ctx context.Context, q access.Queryer) ([]stored, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.credential_id::text,s.name,p.name,s.destination,s.streams,s.enabled FROM olp.managed_export_sinks s LEFT JOIN olp.projects p ON p.id=s.project_id WHERE s.retired_at IS NULL")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (stored, error) {
		var id, name, destination string
		var credential, project *string
		var streams []string
		var enabled bool
		scanErr := row.Scan(&id, &credential, &name, &project, &destination, &streams, &enabled)
		return stored{id: id, credential: credential, sink: Sink{
			Owner:       ownerOf(valueOf(project), project != nil),
			Name:        name,
			Destination: destination,
			Streams:     slices.Sorted(slices.Values(streams)),
			Enabled:     enabled,
			Signed:      credential != nil,
		}}, scanErr
	})
}

func index(current []stored) map[key]stored {
	return maps.Collect(func(yield func(key, stored) bool) {
		for _, s := range current {
			if !yield(s.sink.key(), s) {
				return
			}
		}
	})
}

func Render(state []Sink) *[]contract.ConfigurationSink {
	if len(state) == 0 {
		return nil
	}
	sorted := slices.SortedFunc(slices.Values(state), func(a, b Sink) int {
		return cmp.Or(strings.Compare(a.key().project, b.key().project), strings.Compare(a.key().name, b.key().name))
	})
	rendered := slices.Collect(mapped(sorted, render))
	return &rendered
}

func render(s Sink) contract.ConfigurationSink {
	return contract.ConfigurationSink{
		CredentialRef: present(string(s.ref()), s.Signed),
		Destination:   s.Destination,
		Enabled:       s.Enabled,
		Name:          s.Name,
		Project:       present(valueOf(scope(s.Owner)), scope(s.Owner) != nil),
		Streams: slices.Collect(mapped(s.Streams, func(stream string) contract.ConfigurationSinkStreams {
			return contract.ConfigurationSinkStreams(stream)
		})),
		Type: contract.ConfigurationSinkTypeHttps,
	}
}

func present(value string, ok bool) nullable.Nullable[string] {
	if !ok {
		return nil
	}
	return nullable.NewNullableWithValue(value)
}

type outcome[T any] struct {
	value T
	err   error
}

func settle[T any](outcomes []outcome[T]) ([]T, error) {
	if i := slices.IndexFunc(outcomes, func(o outcome[T]) bool { return o.err != nil }); i >= 0 {
		return nil, outcomes[i].err
	}
	return slices.Collect(mapped(outcomes, func(o outcome[T]) T { return o.value })), nil
}

func mapped[T, U any](values []T, convert func(T) U) func(func(U) bool) {
	return func(yield func(U) bool) {
		for _, value := range values {
			if !yield(convert(value)) {
				return
			}
		}
	}
}

func filtered[T any](values []T, keep func(T) bool) func(func(T) bool) {
	return func(yield func(T) bool) {
		for _, value := range values {
			if keep(value) && !yield(value) {
				return
			}
		}
	}
}

func firstIndex[T any, K comparable](values []T, key func(T) K) map[K]int {
	return maps.Collect(func(yield func(K, int) bool) {
		for i, value := range slices.Backward(values) {
			if !yield(key(value), i) {
				return
			}
		}
	})
}

func valueOf[T any](pointer *T) T {
	if pointer == nil {
		var zero T
		return zero
	}
	return *pointer
}

func nonNil[T any](value T, present bool) *T {
	if !present {
		return nil
	}
	return &value
}
