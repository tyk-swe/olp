package sink

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

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
	sum := sha256.Sum256([]byte(promotion.TupleKey(k.project, k.name)))
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
	raw := promotion.ValueOf(document.Sinks)
	if len(raw) > maxSinks {
		return nil, access.Invalid("sinks", "Declare at most 64 export sinks.")
	}
	entries := promotion.Map(raw, trim)
	first := promotion.FirstIndex(entries, func(e entry) key { return e.sink.key() })
	return promotion.TryMapIndex(entries, func(i int, e entry) (Sink, error) {
		return parseSink(e, first[e.sink.key()] != i, policy)
	})
}

func trim(raw contract.ConfigurationSink) entry {
	project, scoped := promotion.Present(raw.Project)
	_, signed := promotion.Present(raw.CredentialRef)
	return entry{
		sink: Sink{
			Owner:       ownerOf(strings.TrimSpace(project), scoped),
			Name:        strings.TrimSpace(raw.Name),
			Destination: raw.Destination,
			Streams:     promotion.Map(raw.Streams, func(stream contract.ConfigurationSinkStreams) string { return string(stream) }),
			Enabled:     raw.Enabled,
			Signed:      signed,
		},
		kind: string(raw.Type),
		ref:  promotion.Pointer(raw.CredentialRef),
	}
}

func parseSink(e entry, duplicate bool, policy *egress.Policy) (Sink, error) {
	if project, scoped := e.sink.Owner.(Project); scoped && !promotion.ValidLabel(project.Name) {
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
	return promotion.Map(current, func(s stored) Sink { return s.sink }), nil
}

func readStored(ctx context.Context, q access.Queryer) ([]stored, error) {
	active, err := sinks.ReadActiveSinks(ctx, q)
	if err != nil {
		return nil, err
	}
	return promotion.Map(active, func(s sinks.ActiveSink) stored {
		return stored{id: s.ID, credential: s.CredentialID, sink: Sink{
			Owner:       ownerOf(promotion.ValueOf(s.Project), s.Project != nil),
			Name:        s.Name,
			Destination: s.Destination,
			Streams:     slices.Sorted(slices.Values(s.Streams)),
			Enabled:     s.Enabled,
			Signed:      s.CredentialID != nil,
		}}
	}), nil
}

func index(current []stored) map[key]stored {
	return promotion.Index(current, func(s stored) key { return s.sink.key() })
}

func Render(state []Sink) *[]contract.ConfigurationSink {
	if len(state) == 0 {
		return nil
	}
	sorted := slices.SortedFunc(slices.Values(state), func(a, b Sink) int {
		return cmp.Or(strings.Compare(a.key().project, b.key().project), strings.Compare(a.key().name, b.key().name))
	})
	rendered := promotion.Map(sorted, render)
	return &rendered
}

func render(s Sink) contract.ConfigurationSink {
	return contract.ConfigurationSink{
		CredentialRef: present(string(s.ref()), s.Signed),
		Destination:   s.Destination,
		Enabled:       s.Enabled,
		Name:          s.Name,
		Project:       present(promotion.ValueOf(scope(s.Owner)), scope(s.Owner) != nil),
		Streams: promotion.Map(s.Streams, func(stream string) contract.ConfigurationSinkStreams {
			return contract.ConfigurationSinkStreams(stream)
		}),
		Type: contract.ConfigurationSinkTypeHttps,
	}
}

func present(value string, ok bool) nullable.Nullable[string] {
	if !ok {
		return nil
	}
	return nullable.NewNullableWithValue(value)
}
