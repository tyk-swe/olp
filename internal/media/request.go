package media

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/staging"
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
	route  string
	params Params
	extra  map[string]any
	origin Source
}

func (r *Request) Route() string         { return r.route }
func (r *Request) Params() Params        { return r.params }
func (r *Request) Extra() map[string]any { return r.extra }
func (r *Request) Source() Source        { return r.origin }
func (r *Request) Op() Operation         { return r.params.Operation() }
func (r *Request) Stream() bool          { return r.params.streaming() }

func (r *Request) Mode() string {
	switch {
	case r.Op() == OpVideoCreate:
		return "async"
	case r.Stream():
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

var policyTexts = []string{"prompt", "input", "instructions"}

func (r *Request) PolicyTexts() map[string]string {
	texts := map[string]string{}
	switch source := r.origin.(type) {
	case JSONSource:
		fields := source.Document.Fields()
		for _, name := range policyTexts {
			var text string
			if raw, ok := fields[name]; ok && json.Unmarshal(raw, &text) == nil {
				texts[name] = text
			}
		}
	case FormSource:
		for _, part := range source.Parts {
			if text, ok := part.Value.(staging.Text); ok && slices.Contains(policyTexts, part.Name) {
				texts[part.Name] = string(text)
			}
		}
	}
	return texts
}

func (r *Request) WithTexts(texts map[string]string) (*Request, *Error) {
	switch source := r.origin.(type) {
	case JSONSource:
		changes := make([]oif.Change, 0, len(texts))
		for _, name := range slices.Sorted(maps.Keys(texts)) {
			value, err := json.Marshal(texts[name])
			if err != nil {
				return nil, invalidMedia("The rewritten media field could not be encoded.")
			}
			changes = append(changes, oif.Change{Pointer: oif.Pointer("", name), Value: string(value), Origin: oif.ExplicitTransform, Reason: "content policy rewrite"})
		}
		document, err := oif.Apply(source.Document, changes)
		if err != nil {
			return nil, invalidMedia("The rewritten media body could not be encoded.")
		}
		return decode(r.Op(), JSONSource{Document: document})
	case FormSource:
		parts := slices.Clone(source.Parts)
		for i, part := range parts {
			if text, ok := texts[part.Name]; ok {
				parts[i].Value = staging.Text(text)
			}
		}
		return decode(r.Op(), FormSource{Parts: parts, Normalized: source.Normalized})
	}
	return nil, invalidMedia("The media request has no source.")
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

func decodeJSON(body []byte, params any) (string, JSONSource, map[string]any, *Error) {
	invalid := invalidMedia("The request body is not valid JSON.")
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return "", JSONSource{}, nil, invalid
	}
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", JSONSource{}, nil, invalid
	}
	var model struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &model) != nil || json.Unmarshal(body, params) != nil {
		return "", JSONSource{}, nil, invalid
	}
	known := jsonNames(params)
	extra := map[string]any{}
	for name, raw := range document.Fields() {
		if name == "model" || slices.Contains(known, name) {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			return "", JSONSource{}, nil, invalid
		}
		extra[name] = value
	}
	return model.Model, JSONSource{Document: document}, extra, nil
}

func jsonNames(params any) []string {
	t := reflect.TypeOf(params).Elem()
	names := make([]string, 0, t.NumField())
	for field := range t.Fields() {
		if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}

func formExtensions(form *staging.Form) (map[string]any, *Error) {
	extensions := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
	}
	extra := make(map[string]any, len(extensions))
	for name, value := range extensions {
		extra[name] = value
	}
	return extra, nil
}

func newRequest(route string, params Params, extra map[string]any, source Source) (*Request, *Error) {
	if !validRouteSlug(route) {
		return nil, invalidMedia("The model field must name a route.")
	}
	return &Request{route: route, params: params, extra: extra, origin: source}, nil
}

func formRequest(form *staging.Form, route string, params Params, extra map[string]any) (*Request, *Error) {
	parts, normalized := form.SourceFields()
	return newRequest(route, params, extra, FormSource{Parts: parts, Normalized: normalized})
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

func valueOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

func valueOf[T any](value *T) T {
	var zero T
	return valueOr(value, zero)
}

func (r *Request) spoolName(fallback string) string {
	if r == nil {
		return fallback
	}
	return string(r.Op()) + "-response"
}

func (r *Request) TranscriptFormat() string {
	if r == nil {
		return "json"
	}
	switch p := r.params.(type) {
	case Transcription:
		return valueOr(p.Format, "json")
	case Translation:
		return valueOr(p.Format, "json")
	}
	return "json"
}

func (r *Request) expectedImages() int64 {
	if r == nil {
		return 1
	}
	switch p := r.params.(type) {
	case ImageGeneration:
		return imageCount(p.Count)
	case ImageEdit:
		return imageCount(p.Count)
	case ImageVariation:
		return imageCount(p.Count)
	}
	return 1
}
