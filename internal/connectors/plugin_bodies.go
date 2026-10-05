package connectors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A plugin profile's envelope and rewrites change the dialect's bodies around
// OLP's codecs: rewrites change the prepared dialect request, which OIF
// records, and the envelope wraps it on the way out and unwraps responses and
// stream events on the way in, so codecs and OIF see plain dialect bodies.

// memberName is how envelopes and rewrites name JSON object members.
var memberName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

const memberNameRule = "1–128 letters, digits, underscores, hyphens and dots"

// modelValue names the upstream model in envelope templates.
const modelValue = "model"

// hostingRewrite is the provenance of the changes a plugin profile's rewrites
// make to a prepared request.
const hostingRewrite oif.Origin = "hosting_rewrite"

// boundMembers are the request members OLP binds in the plugin dialects that
// carry them in the body: the model a target serves and the delivery, and its
// usage reporting, the caller chose. Rewrites leave them alone. Gemini
// addresses its model and delivery in the URL, so it binds none.
var boundMembers = map[string][]string{
	"openai-chat":        {"model", "stream", "stream_options"},
	"openai-responses":   {"model", "stream"},
	"anthropic-messages": {"model", "stream"},
}

// An envelope is a parsed abi.Envelope.
type envelope struct {
	request  string
	fields   map[string]template
	response string
}

// parseEnvelope reads an envelope whose field templates may reference the
// model and what known admits of the profile's options.
func parseEnvelope(declared *abi.Envelope, known func(name string) error) (*envelope, error) {
	if declared == nil {
		return nil, nil
	}
	if declared.Request == "" && declared.Response == "" {
		return nil, &ProfileError{Field: "hosting.envelope", Message: "Name the request member, the response member, or both."}
	}
	if declared.Request != "" && !memberName.MatchString(declared.Request) {
		return nil, &ProfileError{Field: "hosting.envelope.request", Message: "Name the member with " + memberNameRule + "."}
	}
	if declared.Response != "" && !memberName.MatchString(declared.Response) {
		return nil, &ProfileError{Field: "hosting.envelope.response", Message: "Name the member with " + memberNameRule + "."}
	}
	parsed := &envelope{request: declared.Request, fields: map[string]template{}, response: declared.Response}
	if len(declared.Fields) > 0 && declared.Request == "" {
		return nil, &ProfileError{Field: "hosting.envelope.fields", Message: "Declare fields only with the request member that carries the dialect's body."}
	}
	if len(declared.Fields) > 16 {
		return nil, &ProfileError{Field: "hosting.envelope.fields", Message: "Declare at most 16 fields."}
	}
	for _, name := range slices.Sorted(maps.Keys(declared.Fields)) {
		field := "hosting.envelope.fields." + name
		switch {
		case !memberName.MatchString(name):
			return nil, &ProfileError{Field: field, Message: "Name the field with " + memberNameRule + "."}
		case name == declared.Request:
			return nil, &ProfileError{Field: field, Message: "This member carries the dialect's body."}
		}
		value, err := parseValue(field, declared.Fields[name], envelopePlaceholders(known))
		if err != nil {
			return nil, err
		}
		parsed.fields[name] = value
	}
	return parsed, nil
}

// envelopePlaceholders admits what envelope fields may reference: the model
// and the options known admits, and never the credential, which stays out of
// bodies.
func envelopePlaceholders(known func(name string) error) func(name string) error {
	return func(name string) error {
		switch {
		case name == modelValue:
			return nil
		case strings.HasPrefix(name, optionPlaceholder):
			return known(name)
		}
		return fmt.Errorf("OLP has no placeholder {%s} in an envelope field; use {model} or {options.<name>}.", name)
	}
}

// wrap places a dialect request body in the envelope, with its fields
// rendered from values.
func (e *envelope) wrap(body []byte, values map[string]string) []byte {
	var wrapped bytes.Buffer
	wrapped.Grow(len(body) + 512)
	wrapped.WriteByte('{')
	for _, name := range slices.Sorted(maps.Keys(e.fields)) {
		writeMember(&wrapped, name, jsonString(e.fields[name].render(values)))
		wrapped.WriteByte(',')
	}
	writeMember(&wrapped, e.request, body)
	wrapped.WriteByte('}')
	return wrapped.Bytes()
}

func writeMember(b *bytes.Buffer, name string, value []byte) {
	b.Write(jsonString(name))
	b.WriteByte(':')
	b.Write(value)
}

func jsonString(s string) []byte {
	encoded, _ := json.Marshal(s)
	return encoded
}

// unwrap returns the dialect body that an upstream body carries: the response
// member of a JSON object that has it, and otherwise the body as it is.
func (e *envelope) unwrap(body []byte) []byte {
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return body
	}
	carried, found := document.Root().Lookup(e.response)
	if !found {
		return body
	}
	return carried.Bytes()
}

// envelopeStream unwraps each event of an upstream's server-sent events.
// Events are bounded by the event limit, as the dialect's codec bounds them,
// and a frame never grows: it is re-encoded in its shortest form, keeping
// control frames and their IDs separate from subsequent data frames.
type envelopeStream struct {
	envelope *envelope
	events   *sse.Decoder
	pending  []byte
}

func (s *envelopeStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		event, err := s.events.NextFrame()
		if err != nil {
			return 0, err
		}
		if !event.Control {
			event.Data = string(s.envelope.unwrap([]byte(event.Data)))
		}
		s.pending = event.Encode()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// envelope returns the connector's plugin envelope, or nil.
func (c Config) envelope() *envelope {
	if c.Plugin == nil {
		return nil
	}
	return c.Plugin.hosting.envelope
}

// envelopeValues are the values envelope templates place in a request for
// model: the model and the provider's options. The credential is empty, as
// no envelope template places it.
func (c Config) envelopeValues(model string) map[string]string {
	values := templateValues("", c.PluginOptions, nil)
	values[modelValue] = c.Model(model)
	return values
}

// WrapRequest returns the body OLP sends for a dialect request body to model:
// the body in the envelope a plugin profile declares, the body adapted to a
// hosting's API, such as SageMaker's or watsonx's, or the body as it is.
// Apply signs what it returns.
func (c Config) WrapRequest(body []byte, model string) []byte {
	switch c.Kind {
	case KindSageMaker:
		return sagemakerBody(body)
	case KindWatsonx:
		return c.watsonxBody(body)
	}
	e := c.envelope()
	if e == nil || e.request == "" {
		return body
	}
	return e.wrap(body, c.envelopeValues(model))
}

// UnwrapResponse returns the dialect response that an upstream's successful
// unary response carries: the member a plugin profile's envelope names, or the
// body as it is, such as an upstream error.
func (c Config) UnwrapResponse(body []byte) []byte {
	if c.Kind == KindWatsonx {
		return watsonxResult(body, "chat.completion")
	}
	e := c.envelope()
	if e == nil || e.response == "" {
		return body
	}
	return e.unwrap(body)
}

// unwrapStream unwraps the events of a plugin profile's upstream stream.
func (c Config) unwrapStream(reader io.Reader, maxEventBytes int) io.Reader {
	e := c.envelope()
	if e == nil || e.response == "" {
		return reader
	}
	return &envelopeStream{envelope: e, events: sse.NewDecoder(reader, maxEventBytes)}
}

// maxRewriteDepth bounds how deeply a rewrite value nests, so that with its
// path it stays well within the depth OLP reads requests to.
const maxRewriteDepth = 32

// A rewrite is a parsed abi.Rewrite.
type rewrite struct {
	op    string
	path  []string
	value []byte
}

func parseRewrites(declared []abi.Rewrite, dialect string) ([]rewrite, error) {
	if len(declared) > 16 {
		return nil, &ProfileError{Field: "hosting.rewrites", Message: "Declare at most 16 rewrites."}
	}
	parsed := []rewrite{}
	for i, r := range declared {
		field := fmt.Sprintf("hosting.rewrites[%d]", i)
		switch r.Op {
		case abi.RewriteSet, abi.RewriteDefault:
			// OLP places the value in requests as it reads them.
			if _, err := oif.ParseJSON(r.Value, oif.Limits{MaxDepth: maxRewriteDepth}); len(r.Value) == 0 || err != nil {
				return nil, &ProfileError{Field: field + ".value", Message: fmt.Sprintf("Give the JSON value to %s the member to, at most %d levels deep and naming each member once.", r.Op, maxRewriteDepth)}
			}
		case abi.RewriteDelete:
			if len(r.Value) > 0 {
				return nil, &ProfileError{Field: field + ".value", Message: "Deleting a member takes no value."}
			}
		default:
			return nil, &ProfileError{Field: field + ".op", Message: "Rewrite with set, default or delete."}
		}
		names := strings.Split(strings.TrimPrefix(r.Path, "/"), "/")
		if !strings.HasPrefix(r.Path, "/") || len(names) > 8 || slices.ContainsFunc(names, func(name string) bool { return !memberName.MatchString(name) }) {
			return nil, &ProfileError{Field: field + ".path", Message: "Point at an object member with 1–8 names of " + memberNameRule + ", such as /generationConfig/seed."}
		}
		if slices.Contains(boundMembers[dialect], names[0]) {
			return nil, &ProfileError{Field: field + ".path", Message: "OLP binds /" + names[0] + " for every request of this dialect."}
		}
		for _, prior := range parsed {
			if overlaps(prior.path, names) {
				return nil, &ProfileError{Field: field + ".path", Message: "Rewrite each member once, and nothing inside a member another rewrite changes."}
			}
		}
		parsed = append(parsed, rewrite{op: r.Op, path: names, value: r.Value})
	}
	return parsed, nil
}

// overlaps reports whether one path is the other or inside it.
func overlaps(a, b []string) bool {
	n := min(len(a), len(b))
	return slices.Equal(a[:n], b[:n])
}

func (r rewrite) pointer() string { return "/" + strings.Join(r.path, "/") }

// change returns the overlay the rewrite makes to a request, or false when the
// request already satisfies it: a default of a member the request has, or a
// deletion of one it lacks. Setting a member creates the objects above it; an
// ancestor that is not an object makes the request one the rewrite cannot
// place.
func (r rewrite) change(request oif.Document) (oif.Change, bool, error) {
	change := oif.Change{Origin: hostingRewrite, Reason: "declared " + r.op + " of the plugin profile's hosting adaptation"}
	parent := ""
	for i, name := range r.path {
		container, _ := request.Lookup(parent)
		if container.Kind() != oif.Object {
			if r.op == abi.RewriteDelete {
				return oif.Change{}, false, nil
			}
			return oif.Change{}, false, fmt.Errorf("the plugin profile's rewrite of %s needs the request's %s to be an object", r.pointer(), parent)
		}
		change.Pointer = oif.Pointer(parent, name)
		_, found := container.Lookup(name)
		switch {
		case found && i < len(r.path)-1:
			parent = change.Pointer
			continue
		case !found && r.op == abi.RewriteDelete, found && r.op == abi.RewriteDefault:
			return oif.Change{}, false, nil
		case found && r.op == abi.RewriteDelete:
			change.Remove = true
		default:
			change.Value = string(r.value)
			for _, missing := range slices.Backward(r.path[i+1:]) {
				change.Value = "{" + string(jsonString(missing)) + ":" + change.Value + "}"
			}
		}
		return change, true, nil
	}
	return oif.Change{}, false, nil
}

// Rewrite applies a plugin profile's declared rewrites to a prepared dialect
// request, in order, and records each change in its provenance. Only the
// declared rewrites apply.
func (c Config) Rewrite(prepared oif.Prepared) (oif.Prepared, error) {
	if c.Plugin == nil || len(c.Plugin.hosting.rewrites) == 0 {
		return prepared, nil
	}
	document := prepared.Document()
	var applied []oif.Provenance
	for _, r := range c.Plugin.hosting.rewrites {
		change, changed, err := r.change(document)
		if err != nil {
			return oif.Prepared{}, err
		}
		if !changed {
			continue
		}
		if document, err = oif.Apply(document, []oif.Change{change}); err != nil {
			return oif.Prepared{}, err
		}
		applied = append(applied, oif.Provenance{Pointer: change.Pointer, Origin: change.Origin, Reason: change.Reason})
	}
	if len(applied) == 0 {
		return prepared, nil
	}
	rewritten, err := oif.PrepareDestination(prepared.Request(), prepared.Descriptor(), document, hostingRewrite, "declared rewrites of plugin profile "+c.ProfileID)
	if err != nil {
		return oif.Prepared{}, err
	}
	return rewritten.WithProvenance(prepared.Provenance()...).WithProvenance(applied...), nil
}
