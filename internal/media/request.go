package media

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

type Operation string

const (
	OpImageGeneration Operation = "image_generation"
	OpImageEdit       Operation = "image_edit"
	OpImageVariation  Operation = "image_variation"
	OpSpeech          Operation = "speech"
	OpTranscription   Operation = "transcription"
	OpTranslation     Operation = "translation"
	OpVideoCreate     Operation = "video_create"
	OpVideoGet        Operation = "video_get"
	OpVideoContent    Operation = "video_content"
	OpVideoDelete     Operation = "video_delete"
)

func (op Operation) SupportsStreaming() bool {
	return op == OpImageGeneration || op == OpImageEdit || op == OpSpeech || op == OpTranscription
}

type Params interface {
	Operation() Operation
	uploads() []staging.Part
	streaming() bool
}

type Source interface{ source() }

type JSONSource struct{ Document oif.Document }

type FormSource struct {
	Parts      []staging.Field
	Normalized bool
}

func (JSONSource) source() {}
func (FormSource) source() {}

type Request struct {
	route     string
	params    Params
	jsonExtra map[string]json.RawMessage
	formExtra map[string]string
	origin    Source
}

func (r *Request) Route() string  { return r.route }
func (r *Request) Params() Params { return r.params }
func (r *Request) Source() Source { return r.origin }
func (r *Request) Op() Operation  { return r.params.Operation() }
func (r *Request) Stream() bool   { return r.params.streaming() }
func (r *Request) hasExtra() bool { return len(r.jsonExtra) > 0 || len(r.formExtra) > 0 }

func (r *Request) Mode() string {
	switch {
	case r.Op() == OpVideoCreate:
		return "async"
	case r.Op().SupportsStreaming() && r.Stream():
		return "streaming"
	}
	return "unary"
}

func (r *Request) Uploads() []staging.Handle {
	parts := r.params.uploads()
	handles := make([]staging.Handle, len(parts))
	for i, part := range parts {
		handles[i] = part.Handle
	}
	return handles
}

func (r *Request) SeedMediaUnits() *string {
	if !r.Stream() {
		return nil
	}
	switch p := r.params.(type) {
	case ImageGeneration:
		return new(strconv.FormatInt(imageCount(p.Count), 10))
	case ImageEdit:
		return new(strconv.FormatInt(imageCount(p.Count), 10))
	}
	return nil
}

var unparameterized = []string{"model", "stream", "stream_format", "image", "file"}

func (r *Request) ParameterNames() []string {
	names := map[string]struct{}{}
	add := func(name string) {
		name = strings.TrimSuffix(name, "[]")
		if base, _, indexed := strings.Cut(name, "["); indexed {
			name = base
		}
		if slices.Contains(unparameterized, name) || strings.HasPrefix(name, "__olp_") {
			return
		}
		names[name] = struct{}{}
	}
	switch source := r.origin.(type) {
	case JSONSource:
		for name, raw := range source.Document.Fields() {
			if !isJSON(raw, "null") {
				add(name)
			}
		}
	case FormSource:
		for _, part := range source.Parts {
			add(part.Name)
		}
	}
	return slices.Sorted(maps.Keys(names))
}

func (r *Request) suppliedNames() []string {
	switch source := r.origin.(type) {
	case JSONSource:
		return slices.Collect(maps.Keys(source.Document.Fields()))
	case FormSource:
		names := make([]string, len(source.Parts))
		for i, part := range source.Parts {
			names[i] = strings.TrimSuffix(part.Name, "[]")
		}
		return names
	}
	return nil
}

func decode(op Operation, source Source) (*Request, *Error) {
	switch source := source.(type) {
	case JSONSource:
		body := source.Document.Bytes()
		switch op {
		case OpImageGeneration:
			return DecodeImageGeneration(body)
		case OpSpeech:
			return DecodeSpeech(body)
		}
	case FormSource:
		form := staging.FormOf(source.Parts, source.Normalized)
		switch op {
		case OpImageEdit:
			return DecodeImageEdit(form)
		case OpImageVariation:
			return DecodeImageVariation(form)
		case OpTranscription:
			return DecodeTranscription(form)
		case OpTranslation:
			return DecodeTranslation(form)
		case OpVideoCreate:
			return DecodeVideoCreate(form)
		}
	}
	return nil, invalidMedia("The media operation is not supported.")
}

func decodeJSON[P Params](body []byte, known ...string) (string, P, JSONSource, map[string]json.RawMessage, *Error) {
	var zero P
	invalid := invalidMedia("The request body is not valid JSON.")
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return "", zero, JSONSource{}, nil, invalid
	}
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", zero, JSONSource{}, nil, invalid
	}
	var model struct {
		Model string `json:"model"`
	}
	var params P
	if json.Unmarshal(body, &model) != nil || json.Unmarshal(body, &params) != nil {
		return "", zero, JSONSource{}, nil, invalid
	}
	extra := diffRawFields(document.Fields(), append([]string{"model"}, known...)...)
	return model.Model, params, JSONSource{Document: document}, extra, nil
}

func formExtensions(form *staging.Form) (map[string]string, *Error) {
	extensions := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
	}
	return extensions, nil
}

func newJSONRequest(route string, params Params, extra map[string]json.RawMessage, source JSONSource) (*Request, *Error) {
	if !validRouteSlug(route) {
		return nil, invalidMedia("The model field must name a route.")
	}
	return &Request{route: route, params: params, jsonExtra: extra, origin: source}, nil
}

func formRequest(form *staging.Form, route string, params Params, extra map[string]string) (*Request, *Error) {
	if !validRouteSlug(route) {
		return nil, invalidMedia("The model field must name a route.")
	}
	parts, normalized := form.SourceFields()
	return &Request{route: route, params: params, formExtra: extra, origin: FormSource{Parts: parts, Normalized: normalized}}, nil
}

func validRouteSlug(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return value[0] >= 'a' && value[0] <= 'z' || value[0] >= '0' && value[0] <= '9'
}

func imageCount(count *int64) int64 {
	if count == nil {
		return 1
	}
	return *count
}

func (r *Request) TranscriptFormat() string {
	if r == nil {
		return "json"
	}
	switch p := r.params.(type) {
	case Transcription:
		return ptr.Or(p.Format, "json")
	case Translation:
		return ptr.Or(p.Format, "json")
	}
	return "json"
}
