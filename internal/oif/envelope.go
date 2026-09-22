package oif

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
)

type Identity struct{ ID, Revision string }
type Origin string

const (
	Caller            Origin = "caller"
	ProviderDefault   Origin = "provider_default"
	ModelDefault      Origin = "model_default"
	OperationDefault  Origin = "operation_default"
	IdentityBinding   Origin = "identity_binding"
	ResourceBinding   Origin = "resource_binding"
	TransportOption   Origin = "transport_option"
	QualifiedMapping  Origin = "qualified_mapping"
	ExplicitTransform Origin = "explicit_transform"
	LegacyMapping     Origin = "legacy_mapping"
)

type Presence uint8

const (
	Missing Presence = iota
	ExplicitNull
	Present
)

func (v Value) Presence() Presence {
	switch v.Kind() {
	case Absent:
		return Missing
	case Null:
		return ExplicitNull
	default:
		return Present
	}
}

// Execution dimensions compose without creating a chat-shaped operation enum.
type Execution struct {
	Delivery, Lifetime, Submission string
	Effects                        []string
}
type Requirement struct{ Pointer, Obligation, Description string }
type Asset struct {
	ID, Digest, MediaType string
	Size                  int64
}
type Resource struct{ ID, Kind, Relation string }
type Descriptor struct {
	Operation, Dialect, Profile, Client Identity
	Execution                           Execution
	Requirements                        []Requirement
	Assets                              []Asset
	Resources                           []Resource
}

func (d Descriptor) clone() Descriptor {
	d.Execution.Effects = append([]string(nil), d.Execution.Effects...)
	d.Requirements = append([]Requirement(nil), d.Requirements...)
	d.Assets = append([]Asset(nil), d.Assets...)
	d.Resources = append([]Resource(nil), d.Resources...)
	return d
}

// Change records an overlay, not a mutation of the source. Value is JSON;
// absence, explicit null and removal have distinct representations.
type Change struct {
	Pointer string
	Value   string
	Remove  bool
	Origin  Origin
	Reason  string
}
type Provenance struct {
	Pointer string
	Origin  Origin
	Reason  string
}

type Request struct {
	descriptor        Descriptor
	source, effective Document
	provenance        []Provenance
}

func NewRequest(descriptor Descriptor, source Document) (Request, error) {
	if !source.Valid() || descriptor.Operation.ID == "" || descriptor.Dialect.ID == "" {
		return Request{}, errors.New("request requires source, operation and dialect identities")
	}
	return Request{descriptor: descriptor.clone(), source: source, effective: source}, nil
}
func (r Request) Descriptor() Descriptor              { return r.descriptor.clone() }
func (r Request) Source() Document                    { return r.source }
func (r Request) Document() Document                  { return r.effective }
func (r Request) Provenance() []Provenance            { return append([]Provenance(nil), r.provenance...) }
func (r Request) WithDescriptor(d Descriptor) Request { r.descriptor = d.clone(); return r }
func (r Request) WithChanges(changes ...Change) (Request, error) {
	d, err := Apply(r.effective, changes)
	if err != nil {
		return Request{}, err
	}
	r.effective = d
	r.provenance = append([]Provenance(nil), r.provenance...)
	for _, c := range changes {
		r.provenance = append(r.provenance, Provenance{c.Pointer, c.Origin, c.Reason})
	}
	return r, nil
}

// Apply preserves untouched subtrees byte-for-byte. Overlapping changes and
// array insert/removal are rejected instead of manufacturing an ordering rule.
func Apply(source Document, changes []Change) (Document, error) {
	if !source.Valid() {
		return Document{}, errors.New("missing source")
	}
	if len(changes) == 0 {
		return source, nil
	}
	if len(changes) > source.Limits().MaxNodes {
		return Document{}, errors.New("overlay exceeds node limit")
	}
	replacementBytes := 0
	byPath := make(map[string]Change, len(changes))
	for _, c := range changes {
		if len(c.Value) > source.Limits().MaxBytes-replacementBytes {
			return Document{}, errors.New("overlay exceeds byte limit")
		}
		replacementBytes += len(c.Value)
		if c.Origin == "" || c.Reason == "" {
			return Document{}, errors.New("overlay requires provenance")
		}
		if _, ok := byPath[c.Pointer]; ok {
			return Document{}, errors.New("duplicate overlay path")
		}
		if c.Pointer != "" {
			i := strings.LastIndex(c.Pointer, "/")
			if i < 0 {
				return Document{}, errors.New("invalid overlay pointer")
			}
			parent, ok := source.Lookup(c.Pointer[:i])
			if !ok {
				return Document{}, errors.New("overlay parent does not exist")
			}
			_, exists := source.Lookup(c.Pointer)
			if parent.Kind() != Object && (parent.Kind() != Array || !exists || c.Remove) {
				return Document{}, errors.New("overlay would change array structure")
			}
			if c.Remove && !exists {
				return Document{}, errors.New("overlay removal does not exist")
			}
			if _, err := unescape(c.Pointer[i+1:]); err != nil {
				return Document{}, err
			}
		} else if c.Remove {
			return Document{}, errors.New("cannot remove document")
		}
		if !c.Remove {
			if _, err := ParseJSON([]byte(c.Value), source.Limits()); err != nil {
				return Document{}, err
			}
		}
		byPath[c.Pointer] = c
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for i := 1; i < len(paths); i++ {
		if strings.HasPrefix(paths[i], paths[i-1]+"/") {
			return Document{}, errors.New("overlapping overlay paths")
		}
	}
	var out bytes.Buffer
	var render func(Value, string)
	render = func(v Value, path string) {
		if c, ok := byPath[path]; ok {
			out.WriteString(c.Value)
			return
		}
		changed := false
		for _, p := range paths {
			if strings.HasPrefix(p, path+"/") {
				changed = true
				break
			}
		}
		if !changed {
			out.WriteString(v.Raw())
			return
		}
		if v.Kind() == Array {
			out.WriteByte('[')
			for i, child := range v.Elements() {
				if i > 0 {
					out.WriteByte(',')
				}
				render(child, Pointer(path, strconv.Itoa(i)))
			}
			out.WriteByte(']')
			return
		}
		out.WriteByte('{')
		first := true
		seen := map[string]bool{}
		write := func(name string, child Value) {
			p := Pointer(path, name)
			seen[p] = true
			if c, ok := byPath[p]; ok && c.Remove {
				return
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			key, _ := json.Marshal(name)
			out.Write(key)
			out.WriteByte(':')
			render(child, p)
		}
		for _, m := range v.Members() {
			write(m.Name, m.Value)
		}
		for _, p := range paths {
			if seen[p] || !strings.HasPrefix(p, path+"/") {
				continue
			}
			rest := strings.TrimPrefix(p, path+"/")
			if strings.Contains(rest, "/") {
				continue
			}
			name, _ := unescape(rest)
			write(name, Value{})
		}
		out.WriteByte('}')
	}
	render(source.Root(), "")
	return ParseJSON(out.Bytes(), source.Limits())
}
func unescape(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			i++
			if i >= len(s) || s[i] != '0' && s[i] != '1' {
				return "", errors.New("invalid JSON pointer escape")
			}
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~"), nil
}

type Prepared struct {
	request     Request
	destination Descriptor
	document    Document
	provenance  []Provenance
	identity    bool
}

// PrepareIdentity permits only changes whose meaning is defined by the
// identity contract. Defaults/mappings/transforms require separate admission.
func PrepareIdentity(request Request, destination Descriptor, changes []Change) (Prepared, error) {
	return prepareIdentity(request, destination, changes, nil)
}

// IdentityRule is declared by a trusted dialect binding, never by a request.
// Exact pointers prevent an origin label from blessing arbitrary semantic edits.
type IdentityRule struct {
	Pointer  string
	Origin   Origin
	Kind     Kind
	Validate func(Value, Value) bool
}

func prepareIdentity(request Request, destination Descriptor, changes []Change, rules []IdentityRule) (Prepared, error) {
	if request.Descriptor().Operation != destination.Operation || request.Descriptor().Dialect != destination.Dialect {
		return Prepared{}, errors.New("identity preparation requires identical operation and dialect revisions")
	}
	allowed := func(path string, origin Origin, before, after Value) bool {
		for _, rule := range rules {
			if rule.Pointer == path && rule.Origin == origin && rule.Kind == after.Kind() && (rule.Validate == nil || rule.Validate(before, after)) {
				return true
			}
		}
		return false
	}
	for _, p := range request.Provenance() {
		value, _ := request.Document().Lookup(p.Pointer)
		before, _ := request.Source().Lookup(p.Pointer)
		if !allowed(p.Pointer, p.Origin, before, value) {
			return Prepared{}, errors.New("request contains an overlay outside the dialect identity contract")
		}
	}
	for _, c := range changes {
		value, err := ParseJSON([]byte(c.Value), request.Document().Limits())
		if err != nil {
			return Prepared{}, err
		}
		before, _ := request.Document().Lookup(c.Pointer)
		if c.Remove || !allowed(c.Pointer, c.Origin, before, value.Root()) {
			return Prepared{}, errors.New("overlay is outside the dialect identity contract")
		}
	}
	d, err := Apply(request.Document(), changes)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{request: request, destination: destination.clone(), document: d, identity: true, provenance: request.Provenance()}
	for _, c := range changes {
		p.provenance = append(p.provenance, Provenance{c.Pointer, c.Origin, c.Reason})
	}
	return p, nil
}

// PrepareDestination records an admitted mapping or legacy adapter result. It
// cannot claim identity merely because source and destination happen to match.
func PrepareDestination(request Request, destination Descriptor, document Document, origin Origin, reason string) (Prepared, error) {
	if !document.Valid() || origin == "" || reason == "" {
		return Prepared{}, errors.New("destination requires source and provenance")
	}
	return Prepared{request: request, destination: destination.clone(), document: document, provenance: append(request.Provenance(), Provenance{"", origin, reason})}, nil
}
func (p Prepared) Request() Request                      { return p.request }
func (p Prepared) Descriptor() Descriptor                { return p.destination.clone() }
func (p Prepared) WithProfile(profile Identity) Prepared { p.destination.Profile = profile; return p }
func (p Prepared) Document() Document                    { return p.document }
func (p Prepared) Identity() bool                        { return p.identity }
func (p Prepared) Provenance() []Provenance              { return append([]Provenance(nil), p.provenance...) }

// WithProvenance records observations made at the actual lowering/default
// branch. It never changes the prepared body or upgrades its fidelity class.
func (p Prepared) WithProvenance(entries ...Provenance) Prepared {
	p.provenance = append(p.Provenance(), entries...)
	return p
}

type Outcome string

const (
	Complete   Outcome = "complete"
	Incomplete Outcome = "incomplete"
	Failed     Outcome = "failed"
	Pending    Outcome = "pending"
)

type Result struct {
	descriptor Descriptor
	source     Document
	outcome    Outcome
}

func NewResult(d Descriptor, source Document, outcome Outcome) (Result, error) {
	if !source.Valid() {
		return Result{}, errors.New("result requires source")
	}
	return Result{d.clone(), source, outcome}, nil
}
func (r Result) Descriptor() Descriptor              { return r.descriptor.clone() }
func (r Result) Source() Document                    { return r.source }
func (r Result) Outcome() Outcome                    { return r.outcome }
func (r Result) WithProfile(profile Identity) Result { r.descriptor.Profile = profile; return r }

// Event carries one source event. No history or unbounded accumulator lives in
// this envelope. Control is reserved for non-JSON terminal transport markers.
type Event struct {
	descriptor    Descriptor
	source        Document
	name, control string
	sequence      uint64
	framing       Framing
}

type Framing struct {
	ID          string
	HasID       bool
	RetryMillis uint64
	HasRetry    bool
}

func (e Event) WithFraming(f Framing) Event { e.framing = f; return e }
func (e Event) Framing() Framing            { return e.framing }

func NewEvent(d Descriptor, source Document, name string, sequence uint64) (Event, error) {
	if !source.Valid() {
		return Event{}, errors.New("event requires source")
	}
	return Event{descriptor: d.clone(), source: source, name: name, sequence: sequence}, nil
}
func ControlEvent(d Descriptor, name, control string, sequence uint64) Event {
	return Event{descriptor: d.clone(), name: name, control: control, sequence: sequence}
}
func (e Event) Descriptor() Descriptor             { return e.descriptor.clone() }
func (e Event) Source() Document                   { return e.source }
func (e Event) Name() string                       { return e.name }
func (e Event) Control() string                    { return e.control }
func (e Event) Sequence() uint64                   { return e.sequence }
func (e Event) WithProfile(profile Identity) Event { e.descriptor.Profile = profile; return e }
