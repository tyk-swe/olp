// Package operations links trusted operation-owned unary codecs. It does not
// know providers, transports, storage services or generation representations.
package operations

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/tyk-swe/olp/internal/oif"
)

const (
	Revision        = "1"
	RawVectorClient = "raw-vector-storage/1"
)

var Label = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,95}$`)

// Usage is observed provider consumption, not a tokenization result or estimate.
// Each result's complete native categories remain in its immutable source.
type Usage struct {
	InputTokens, OutputTokens, TotalTokens *int64
	CachedInputTokens                      *int64
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
// by the hosting owner; legacy path keys are adapters, never operation IR.
type Address struct{ LegacyPath, RelativePath string }

// Dialect is the small unary contract. Optional hooks are absent where that
// operation has no model-body identity, textual policy surface or usage record.
type Dialect struct {
	Identity, Operation           oif.Identity
	Surface, Label, Documentation string
	Address                       Address
	RequestSchema, ResultSchema   json.RawMessage
	Defaults                      map[string]Field
	Request                       func(oif.Request) (oif.View, error)
	Result                        func(oif.Request, oif.Result) (oif.View, error)
	ValidateRoute                 func(oif.Request, string) error
	BindModel                     func(oif.Document, string) ([]oif.Change, error)
	BindResultModel               func(oif.Document, string) ([]oif.Change, error)
	Estimate                      func(oif.View) int64
	Usage                         func(oif.View) *Usage
	RequiredClient                func(oif.View) string
	InputText                     func(oif.Request) ([]Text, error)
	OutputText                    func(oif.Result) ([]Text, error)
	Probe                         func(string) []byte
	Evidence                      string
}

// Mapping qualifies one actual source/target pair. It owns both directions;
// merely retaining foreign fields cannot establish target interpretation.
//
// Lower and Project co-construct each destination: they return the document
// and the declared changes that produce it. The planner re-applies the
// declaration and requires byte-identical construction, so a change missing
// from the declaration or a declared change whose value differs from the
// destination is a contract violation, not a silent drop. Projected declares
// every result change Project may apply, using /result-scoped fields, so a
// pre-dispatch receipt can advertise the projection contract; Decode rejects
// realized result changes the declaration does not cover.
type Mapping struct {
	Source, Target    oif.Identity
	Lower             func(oif.Request, oif.View) (oif.Document, []oif.Change, error)
	ValidateEffective func(oif.Request, oif.Request) error
	Project           func(oif.Request, oif.Result, oif.View, string) (oif.Document, []oif.Change, error)
	Projected         []oif.Disposition
	Evidence          string
}

type Registry struct {
	mu       sync.RWMutex
	dialects map[oif.Identity]Dialect
	mappings map[[2]oif.Identity]Mapping
}

func NewRegistry() *Registry {
	return &Registry{dialects: map[oif.Identity]Dialect{}, mappings: map[[2]oif.Identity]Mapping{}}
}

func (r *Registry) Register(d Dialect) error {
	if !Label.MatchString(d.Identity.ID) || d.Identity.Revision == "" || !Label.MatchString(d.Operation.ID) || d.Operation.Revision == "" || d.Request == nil || d.Result == nil || d.Probe == nil || d.Evidence == "" || d.Label == "" {
		return errors.New("unary dialect requires bounded identities, codecs, probe and evidence")
	}
	if d.Address.LegacyPath == "" && d.Address.RelativePath == "" || d.Address.LegacyPath != "" && d.Address.RelativePath != "" {
		return errors.New("unary dialect requires one addressing contract")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.dialects[d.Identity]; exists || len(r.dialects) >= 256 {
		return errors.New("duplicate or excessive dialect registration")
	}
	r.dialects[d.Identity] = cloneDialect(d)
	return nil
}

func (r *Registry) RegisterMapping(m Mapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, aok := r.dialects[m.Source]
	b, bok := r.dialects[m.Target]
	if !aok || !bok || a.Operation != b.Operation || m.Lower == nil || m.Project == nil || m.Evidence == "" || len(r.mappings) >= 1024 {
		return errors.New("mapping requires registered matching operations and complete directions")
	}
	if len(m.Projected) > 64 {
		return errors.New("mapping result declaration exceeds bounds")
	}
	for _, declared := range m.Projected {
		// Declared result fields are /result-scoped registered-schema names;
		// they describe what Project may do, never observed content.
		if !strings.HasPrefix(declared.Field, "/result/") || len(declared.Field) > 256 || declared.Disposition == "" || declared.Rule == "" {
			return errors.New("mapping result declaration requires bounded /result-scoped field, disposition and rule")
		}
	}
	key := [2]oif.Identity{m.Source, m.Target}
	if _, exists := r.mappings[key]; exists {
		return errors.New("duplicate mapping registration")
	}
	r.mappings[key] = m
	return nil
}

func (r *Registry) Lookup(id, revision string) (Dialect, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.dialects[oif.Identity{ID: id, Revision: revision}]
	return cloneDialect(d), ok
}

func (r *Registry) Mapping(source, target oif.Identity) (Mapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mappings[[2]oif.Identity{source, target}]
	return m, ok
}

func (r *Registry) Dialects() []Dialect {
	r.mu.RLock()
	defer r.mu.RUnlock()
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

func (r *Registry) Supports(operation, surface, mode string) bool {
	if mode != "unary" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
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

func (r *Registry) HasOperation(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.dialects {
		if d.Operation.ID == id {
			return true
		}
	}
	return false
}

func (r *Registry) SupportsTarget(target oif.Identity, surface, mode string) bool {
	if mode != "unary" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
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
