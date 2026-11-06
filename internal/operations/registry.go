package operations

import (
	"encoding/json"
	"errors"
	"maps"
	"net/url"
	"regexp"
	"slices"

	"github.com/tyk-swe/olp/internal/oif"
)

const (
	Revision        = "1"
	RawVectorClient = "raw-vector-storage/1"
)

var label = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,95}$`)

func ValidLabel(value string) bool { return label.MatchString(value) }

// Usage is observed provider consumption, not a tokenization result or estimate.
// Each result's complete native categories remain in its immutable source.
type Usage struct {
	InputTokens, OutputTokens, TotalTokens *int64
	CachedInputTokens                      *int64
	CacheWriteInputTokens, ReasoningTokens *int64
	// MediaUnits carries a provider's native non-token billed quantity (for
	// example Cohere search_units or images) as exact decimal text.
	MediaUnits *string
}

type Text struct{ Pointer, Value string }

type Field struct {
	Schema   json.RawMessage
	Validate func(oif.Value) error
}

// Address is selected by trusted codec registration. RelativePath is appended
// by the hosting owner; FamilyPath keys select a connector family endpoint and
// are adapters, never operation IR.
type Address struct{ FamilyPath, RelativePath string }

type Transport string

const (
	TransportJSON     Transport = "json"
	TransportGetQuery Transport = "get-query"
	TransportPoll202  Transport = "poll-202"
)

func (t Transport) Normalized() Transport {
	if t == "" {
		return TransportJSON
	}
	return t
}

type PollState string

const (
	PollPending   PollState = "pending"
	PollSucceeded PollState = "succeeded"
	PollFailed    PollState = "failed"
)

type WireRequest struct {
	Query url.Values
	Body  []byte
}

type Dialect struct {
	Identity, Operation           oif.Identity
	Surface, Label, Documentation string
	Address                       Address
	Transport                     Transport
	RequestSchema, ResultSchema   json.RawMessage
	Defaults                      map[string]Field
	Request                       func(oif.Request) (oif.View, error)
	Query                         func(oif.Request) (url.Values, error)
	Result                        func(oif.Request, oif.Result) (oif.View, error)
	ValidateRoute                 func(oif.Request, string) error
	BindModel                     func(oif.Document, string) ([]oif.Change, error)
	BindResultModel               func(oif.Document, string) ([]oif.Change, error)
	Estimate                      func(oif.View) int64
	Usage                         func(oif.View) *Usage
	RequiredClient                func(oif.View) string
	InputText                     func(oif.Request) ([]Text, error)
	OutputText                    func(oif.Result) ([]Text, error)
	DocumentLocation              func(oif.Request) (string, bool)
	FetchedDocument               func(oif.Request) bool
	Probe                         func(string) []byte
	Evidence                      string
	Wire                          func(oif.View) (WireRequest, error)
	Poll                          func(oif.Document) (PollState, error)
	CancelPoll                    func(pollURL string) (method, url string, ok bool)
}

// Mapping qualifies one actual source/target pair. It owns both directions;
// merely retaining foreign fields cannot establish target interpretation.
type Mapping struct {
	Source, Target    oif.Identity
	Lower             func(oif.Request, oif.View) (oif.Document, []oif.Provenance, error)
	ValidateEffective func(oif.Request, oif.Request) error
	Project           func(oif.Request, oif.Result, oif.View, string) (oif.Document, error)
	Evidence          string
}

type Registry struct {
	dialects map[oif.Identity]Dialect
	mappings map[[2]oif.Identity]Mapping
}

func NewRegistry(dialects []Dialect, mappings []Mapping) (Registry, error) {
	if len(dialects) > 256 {
		return Registry{}, errors.New("duplicate or excessive dialect registration")
	}
	for _, d := range dialects {
		if err := validateDialect(d); err != nil {
			return Registry{}, err
		}
	}
	registered := maps.Collect(func(yield func(oif.Identity, Dialect) bool) {
		for _, d := range dialects {
			normalized := cloneDialect(d)
			normalized.Transport = d.Transport.Normalized()
			if !yield(d.Identity, normalized) {
				return
			}
		}
	})
	if len(registered) != len(dialects) {
		return Registry{}, errors.New("duplicate or excessive dialect registration")
	}
	if len(mappings) > 1024 {
		return Registry{}, errors.New("mapping requires registered matching operations and complete directions")
	}
	for _, m := range mappings {
		a, aok := registered[m.Source]
		b, bok := registered[m.Target]
		if !aok || !bok || a.Operation != b.Operation || m.Lower == nil || m.Project == nil || m.Evidence == "" {
			return Registry{}, errors.New("mapping requires registered matching operations and complete directions")
		}
	}
	registeredMappings := maps.Collect(func(yield func([2]oif.Identity, Mapping) bool) {
		for _, m := range mappings {
			if !yield([2]oif.Identity{m.Source, m.Target}, m) {
				return
			}
		}
	})
	if len(registeredMappings) != len(mappings) {
		return Registry{}, errors.New("duplicate mapping registration")
	}
	return Registry{dialects: registered, mappings: registeredMappings}, nil
}

func validateDialect(d Dialect) error {
	if !ValidLabel(d.Identity.ID) || d.Identity.Revision == "" || !ValidLabel(d.Operation.ID) || d.Operation.Revision == "" || d.Request == nil || d.Result == nil || d.Probe == nil || d.Evidence == "" || d.Label == "" {
		return errors.New("unary dialect requires bounded identities, codecs, probe and evidence")
	}
	if d.Address.FamilyPath == "" && d.Address.RelativePath == "" || d.Address.FamilyPath != "" && d.Address.RelativePath != "" {
		return errors.New("unary dialect requires one addressing contract")
	}
	switch d.Transport.Normalized() {
	case TransportJSON:
		if d.Query != nil || d.Wire != nil || d.Poll != nil || d.CancelPoll != nil {
			return errors.New("json unary dialect must not lower to query parameters or polling hooks")
		}
	case TransportGetQuery:
		if d.Query == nil || d.Wire != nil || d.Poll != nil || d.CancelPoll != nil {
			return errors.New("get-query unary dialect requires query lowering and no polling hooks")
		}
	case TransportPoll202:
		if d.Wire == nil || d.Poll == nil || d.Query != nil {
			return errors.New("poll-202 unary dialect requires a wire lowering and a poll reading")
		}
	default:
		return errors.New("unary dialect transport is not registered")
	}
	return nil
}

func (r Registry) Lookup(id, revision string) (Dialect, bool) {
	d, ok := r.dialects[oif.Identity{ID: id, Revision: revision}]
	return cloneDialect(d), ok
}

func (r Registry) Mapping(source, target oif.Identity) (Mapping, bool) {
	m, ok := r.mappings[[2]oif.Identity{source, target}]
	return m, ok
}

func (r Registry) Dialects() []Dialect {
	out := make([]Dialect, 0, len(r.dialects))
	for _, d := range r.dialects {
		out = append(out, cloneDialect(d))
	}
	slices.SortFunc(out, func(a, b Dialect) int {
		if a.Identity.ID < b.Identity.ID {
			return -1
		}
		if a.Identity.ID > b.Identity.ID {
			return 1
		}
		return 0
	})
	return out
}

func (r Registry) Supports(operation, surface, mode string) bool {
	if mode != "unary" {
		return false
	}
	for _, d := range r.dialects {
		if d.Operation.ID == operation && (surface == d.Surface || surface == "native") {
			return true
		}
	}
	return false
}

func cloneDialect(d Dialect) Dialect {
	d.RequestSchema = slices.Clone(d.RequestSchema)
	d.ResultSchema = slices.Clone(d.ResultSchema)
	fields := make(map[string]Field, len(d.Defaults))
	for name, field := range d.Defaults {
		field.Schema = slices.Clone(field.Schema)
		fields[name] = field
	}
	d.Defaults = fields
	return d
}

func (r Registry) HasOperation(id string) bool {
	for _, d := range r.dialects {
		if d.Operation.ID == id {
			return true
		}
	}
	return false
}

func (r Registry) SupportsTarget(target oif.Identity, surface, mode string) bool {
	if mode != "unary" {
		return false
	}
	d, ok := r.dialects[target]
	if !ok {
		return false
	}
	if surface == "native" || d.Surface == surface {
		return true
	}
	for pair := range r.mappings {
		if pair[1] == target && r.dialects[pair[0]].Surface == surface {
			return true
		}
	}
	return false
}
