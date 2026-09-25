package generation

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/tyk-swe/olp/internal/oif"
)

// Label is the dialect/operation label grammar shared with the unary
// operation registry.
var Label = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,95}$`)

var maxContract = 128

type mappingKey struct {
	source, target oif.Identity
	contract       string
}

// Registry is the operation-owned generation registry. A dialect registration
// owns its codec, grammar, delivery and effects contracts; a mapping qualifies
// one source/target pair. Registration is bounded and immutable after
// publication — lookups never hand out a mutable interior.
type Registry struct {
	mu       sync.RWMutex
	dialects map[oif.Identity]Dialect
	labels   map[string]oif.Identity
	mappings map[mappingKey]Mapping
	oif      *oif.Registry
}

func NewRegistry() *Registry {
	return &Registry{
		dialects: map[oif.Identity]Dialect{},
		labels:   map[string]oif.Identity{},
		mappings: map[mappingKey]Mapping{},
	}
}

func (r *Registry) rebuild() error {
	bindings := []oif.Binding{}
	for _, d := range r.dialects {
		bindings = append(bindings, oif.Binding{Dialect: d.Identity, Operation: Contract(), IdentityRules: slices.Clone(d.IdentityRules)})
	}
	reg, err := oif.NewRegistry([]oif.Operation{{Identity: Contract()}}, bindings)
	if err != nil {
		return err
	}
	r.oif = reg
	return nil
}

// Register publishes one generation dialect. Identities are bounded, labels
// unique, and required capability hooks fail closed: a dialect without a codec
// for a delivery mode simply never claims it.
func (r *Registry) Register(d Dialect) error {
	if !Label.MatchString(d.Identity.ID) || d.Identity.Revision == "" || d.Operation != Contract() || d.Label == "" || !Label.MatchString(d.Label) || d.Evidence == "" {
		return errors.New("generation dialect requires bounded identity, label and evidence")
	}
	if d.Identity.ID != d.Label {
		return errors.New("generation dialect label must equal its identity ID")
	}
	if len(d.Surface) > 96 {
		return errors.New("generation dialect requires a bounded served surface")
	}
	if d.Lift == nil || d.IdentityChanges == nil || d.DecodeNative == nil || d.Effects == nil || d.Estimate == nil || d.Parameters == nil {
		return errors.New("generation dialect requires lift, identity, decode, effects and reservation hooks")
	}
	if d.ValidateEvent == nil && d.Streaming {
		return errors.New("streaming dialect requires its event admission guard")
	}
	if d.Streaming && d.StreamNative == nil {
		return errors.New("streaming dialect requires a native event grammar")
	}
	if !d.Streaming && (d.StreamNative != nil || d.ValidateEvent != nil) {
		return errors.New("unary dialect cannot claim a native event grammar")
	}
	if (d.Address.RelativePath == "") == (d.Address.LegacyPath == "") {
		return errors.New("generation dialect requires exactly one addressing contract")
	}
	if path := d.Address.RelativePath; path != "" {
		if strings.ContainsAny(path, "?#\\") || strings.HasPrefix(path, "/") || strings.Contains(path, "..") || len(path) > 512 {
			return errors.New("invalid registered generation path")
		}
	}
	for _, rule := range d.IdentityRules {
		switch rule.Origin {
		case oif.IdentityBinding, oif.ResourceBinding, oif.TransportOption:
		default:
			return errors.New("identity rule cannot authorize semantic transformation")
		}
		if len(rule.Pointer) > 256 {
			return errors.New("identity rule pointer exceeds bounds")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.dialects) >= 256 || len(r.mappings) >= 1024 {
		return errors.New("generation registry is full")
	}
	if _, ok := r.dialects[d.Identity]; ok {
		return errors.New("duplicate generation dialect registration")
	}
	if _, ok := r.labels[d.Label]; ok {
		return errors.New("duplicate generation dialect label")
	}
	r.dialects[d.Identity] = cloneDialect(d)
	r.labels[d.Label] = d.Identity
	return r.rebuild()
}

// RegisterMapping qualifies one actual source/target pair. Both dialects must
// already be registered; the contract version is part of the mapping identity
// so a negotiated continuation contract never claims the stateless slot. A
// mapping carrying a client contract must project results and, when the target
// dialect streams, events.
func (r *Registry) RegisterMapping(m Mapping) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.dialects[m.Source]; !ok {
		return errors.New("mapping source dialect is not registered")
	}
	if _, ok := r.dialects[m.Target]; !ok {
		return errors.New("mapping target dialect is not registered")
	}
	if m.Source == m.Target {
		return errors.New("identity mappings are not qualified mappings")
	}
	if m.Evidence == "" || m.Lower == nil || m.ProjectResult == nil {
		return errors.New("mapping requires evidence, lowering and projection")
	}
	if m.ClientContract != "" {
		if len(m.ClientContract) > maxContract || !Label.MatchString(m.ClientContract) {
			return errors.New("generation client contract requires a bounded version")
		}
		if target := r.dialects[m.Target]; target.Streaming && m.ProjectEvents == nil {
			return errors.New("client-contract mapping requires an event projection for a streaming target")
		}
	}
	key := mappingKey{m.Source, m.Target, m.ClientContract}
	if _, ok := r.mappings[key]; ok {
		return errors.New("duplicate generation mapping registration")
	}
	r.mappings[key] = m
	return nil
}

func (r *Registry) Dialect(identity oif.Identity) (Dialect, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.dialects[identity]
	return cloneDialect(d), ok
}

func (r *Registry) DialectLabel(label string) (Dialect, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	identity, ok := r.labels[label]
	if !ok {
		return Dialect{}, false
	}
	d, ok := r.dialects[identity]
	return cloneDialect(d), ok
}

func (r *Registry) Dialects() []Dialect {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Dialect, 0, len(r.dialects))
	for _, d := range r.dialects {
		out = append(out, cloneDialect(d))
	}
	slices.SortFunc(out, func(a, b Dialect) int { return strings.Compare(a.Identity.ID, b.Identity.ID) })
	return out
}

// Mapping returns the qualified contract for one pair; the empty contract is
// the stateless mapping.
func (r *Registry) Mapping(source, target oif.Identity, contract string) (Mapping, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mappings[mappingKey{source, target, contract}]
	return m, ok
}

// Mappings lists the qualified targets a source dialect admits, sorted for
// stable evidence and capability enumeration.
func (r *Registry) Mappings(source oif.Identity) []Mapping {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Mapping{}
	for key, m := range r.mappings {
		if key.source == source {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Mapping) int {
		if c := strings.Compare(a.Target.ID, b.Target.ID); c != 0 {
			return c
		}
		return strings.Compare(a.ClientContract, b.ClientContract)
	})
	return out
}

// KnownContract reports whether a client contract version is registered on
// any source/target pair.
func (r *Registry) KnownContract(version string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key := range r.mappings {
		if key.contract == version {
			return true
		}
	}
	return false
}

// SourceContract reports whether one source dialect admits a client contract
// version on any registered target.
func (r *Registry) SourceContract(source oif.Identity, version string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key := range r.mappings {
		if key.source == source && key.contract == version {
			return true
		}
	}
	return false
}

// SupportsTarget reports whether a registered target dialect can serve the
// requested capability tuple. A native surface is the dialect itself; any
// other surface requires a registered mapping — stateless or contracted —
// from a dialect on that surface. A client-contract mapping still serves its
// source surface: strict binding gates each request on the negotiated
// contract, but the surface capability is real.
func (r *Registry) SupportsTarget(target oif.Identity, surface, mode string) bool {
	d, ok := r.Dialect(target)
	if !ok {
		return false
	}
	if mode != "unary" && mode != "streaming" {
		return false
	}
	if mode == "streaming" && !d.Streaming {
		return false
	}
	if surface == "native" || surface == d.Surface {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for key := range r.mappings {
		if key.target != target {
			continue
		}
		if source, ok := r.dialects[key.source]; ok && source.Surface == surface {
			return true
		}
	}
	return false
}

// SupportsSurface reports whether any registered dialect can serve the
// surface/mode tuple — as its own surface, on the generic native surface, or
// through a qualified mapping. Capability declarations use it so a new
// dialect's surfaces are declarable without a management-side enumeration.
func (r *Registry) SupportsSurface(surface, mode string) bool {
	if mode != "unary" && mode != "streaming" {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, d := range r.dialects {
		if mode == "streaming" && !d.Streaming {
			continue
		}
		if surface == "native" || surface == d.Surface {
			return true
		}
		for key := range r.mappings {
			if key.target != d.Identity {
				continue
			}
			if source, ok := r.dialects[key.source]; ok && source.Surface == surface {
				return true
			}
		}
	}
	return false
}

// PrepareIdentity runs the aggregated identity contract: only overlays the
// destination dialect registered as identity/resource/transport bindings are
// permitted.
func (r *Registry) PrepareIdentity(request oif.Request, destination oif.Descriptor, changes []oif.Change) (oif.Prepared, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.oif == nil {
		return oif.Prepared{}, errors.New("unregistered generation contract")
	}
	return r.oif.PrepareIdentity(request, destination, changes)
}

func cloneDialect(d Dialect) Dialect {
	d.IdentityRules = slices.Clone(d.IdentityRules)
	return d
}
