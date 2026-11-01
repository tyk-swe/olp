package wire

import (
	"bytes"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/oif"
)

func IsResponsesItemReference(kind string, hasID bool) bool {
	return kind == "item_reference" || kind == "" && hasID
}

// Request retains an immutable source and the envelope understood by routing.
// A transformation returns a new request or an error and never damages its input.
type Request struct {
	Family            Format
	Route             string
	Stream            bool
	IncludeUsage      bool
	source            oif.Request
	validatedDocument oif.Document
	validatedFormat   Format
	validatedCodec    string
}

func NewRequest(format Format, route string, stream bool, doc oif.Document) (*Request, error) {
	source, err := oif.NewRequest(Descriptor(format, stream), doc)
	if err != nil {
		return nil, err
	}
	return &Request{Family: format, Route: route, Stream: stream, source: source}, nil
}

func FromFields(format Format, route string, stream bool, fields map[string]json.RawMessage) (*Request, error) {
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	doc, err := oif.ParseJSON(data, oif.Limits{})
	if err != nil {
		return nil, err
	}
	return NewRequest(format, route, stream, doc)
}

func (r *Request) Field(name string) json.RawMessage {
	value, _ := r.source.Document().Root().Lookup(name)
	return value.Bytes()
}
func (r *Request) OIF() oif.Request                     { return r.source }
func (r *Request) Document() map[string]json.RawMessage { return r.source.Document().Fields() }

func (r *Request) WithField(name string, value json.RawMessage) (*Request, error) {
	source, err := r.source.WithChanges(oif.Change{Pointer: oif.Pointer("", name), Value: string(value), Origin: oif.ResourceBinding, Reason: "gateway resource reference"})
	if err != nil {
		return nil, err
	}
	out := *r
	out.source = source
	return &out, nil
}

// WithResourceBindings overlays exact, dialect-owned reference positions. The
// caller must establish live resource authority before invoking it; identity
// preparation independently checks the dialect's declared pointer patterns.
func (r *Request) WithResourceBindings(changes ...oif.Change) (*Request, error) {
	for i := range changes {
		changes[i].Origin = oif.ResourceBinding
	}
	source, err := r.source.WithChanges(changes...)
	if err != nil {
		return nil, err
	}
	out := *r
	out.source = source
	return &out, nil
}

func (r *Request) WithFields(fields map[string]json.RawMessage, origin oif.Origin) (*Request, error) {
	changes := []oif.Change{}
	for name, value := range fields {
		if !bytes.Equal(r.Field(name), value) {
			changes = append(changes, oif.Change{Pointer: oif.Pointer("", name), Value: string(value), Origin: origin, Reason: "explicit input policy"})
		}
	}
	for _, m := range r.source.Document().Root().Members() {
		if _, ok := fields[m.Name]; !ok {
			changes = append(changes, oif.Change{Pointer: oif.Pointer("", m.Name), Remove: true, Origin: origin, Reason: "explicit input policy"})
		}
	}
	source, err := r.source.WithChanges(changes...)
	if err != nil {
		return nil, err
	}
	out := *r
	out.source = source
	return &out, nil
}

// WithValidation records codec validation of this exact document and format.
// Derived documents cannot reuse it; codecs still validate applied defaults.
func (r *Request) WithValidation(codec string) *Request {
	out := *r
	out.source = out.source.WithDescriptor(Descriptor(out.Family, out.Stream))
	out.validatedDocument = r.source.Document()
	out.validatedFormat = r.Family
	out.validatedCodec = codec
	return &out
}
func (r *Request) ValidatedBy(codec string) bool {
	return r.validatedCodec == codec && r.validatedFormat == r.Family && r.validatedDocument == r.source.Document()
}
