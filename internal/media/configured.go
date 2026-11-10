package media

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/mediacontract"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/staging"
)

func EncodeConfigured(request *Request, cfg connectors.Config, model string) (*UpstreamCall, *Request, *Error) {
	c, vendor := codecFor(cfg, request.Op())
	upstreamModel := cfg.Model(model)
	if vendor {
		if defaults, _, err := cfg.DefaultsFor(string(request.Op()), model); cfg.ProfileID != "" && (err != nil || len(defaults) > 0) {
			return nil, nil, invalidMedia("Native media defaults do not apply to the vendor's translated media API.")
		}
	}
	if vendor || cfg.ProfileID == "" {
		call, failure := c.encode(request, cfg, upstreamModel)
		return call, request, failure
	}
	defaults, failure := profileDefaults(request.Op(), cfg, model)
	if failure != nil {
		return nil, nil, failure
	}
	cloud := request.Op() == OpImageGeneration && (cfg.Kind == "vertex_ai" || cfg.Kind == "bedrock")
	effective, failure := withDefaults(request, defaults, cloud)
	if failure != nil {
		return nil, nil, failure
	}
	if effective.Mode() != request.Mode() {
		return nil, nil, configuredFailure("stream_format", "cannot change the caller's delivery mode")
	}
	source, native := effective.Source().(JSONSource)
	if cloud {
		if failure := cloudRepresentable(source.Document); failure != nil {
			return nil, nil, failure
		}
	}
	call, failure := c.encode(effective, cfg, upstreamModel)
	if failure != nil || cloud || !native {
		return call, effective, failure
	}
	body, failure := boundModel(source.Document, upstreamModel)
	if failure != nil {
		return nil, nil, failure
	}
	call.JSON = body
	return call, effective, nil
}

func EncodeStrictConfigured(request *Request, cfg connectors.Config, model string) (*UpstreamCall, *Request, *Error) {
	call, effective, failure := EncodeConfigured(request, cfg, model)
	if failure != nil || len(call.Fields) == 0 {
		return call, effective, failure
	}
	source, form := effective.Source().(FormSource)
	if !form || source.Normalized {
		return nil, nil, configuredFailure("multipart", "has a caller source that cannot be preserved exactly")
	}
	call.Fields = exactParts(source.Parts, cfg.Model(model))
	return call, effective, nil
}

func profileDefaults(op Operation, cfg connectors.Config, model string) (map[string]json.RawMessage, *Error) {
	if err := cfg.ValidateProfile(); err != nil {
		return nil, invalidMedia(err.Error())
	}
	profile, err := cfg.Profile()
	if err != nil {
		return nil, invalidMedia(err.Error())
	}
	if !slices.Contains(profile.Operations, string(op)) {
		return nil, invalidMedia("The selected profile does not own this media operation.")
	}
	defaults, _, err := cfg.DefaultsFor(string(op), model)
	if err != nil {
		return nil, invalidMedia(err.Error())
	}
	return defaults, nil
}

func withDefaults(r *Request, defaults map[string]json.RawMessage, cloud bool) (*Request, *Error) {
	present := r.suppliedNames()
	absent := slices.DeleteFunc(slices.Sorted(maps.Keys(defaults)), func(name string) bool { return slices.Contains(present, name) })
	if len(absent) == 0 {
		return r, nil
	}
	_, multipart := r.origin.(FormSource)
	for _, name := range absent {
		if failure := placeable(r.Op(), name, defaults[name], present, cloud, multipart); failure != nil {
			return nil, failure
		}
	}
	switch source := r.origin.(type) {
	case JSONSource:
		changes := make([]oif.Change, len(absent))
		for i, name := range absent {
			changes[i] = oif.Change{Pointer: oif.Pointer("", name), Value: string(defaults[name]), Origin: oif.ProviderDefault, Reason: "declared absent-only media default"}
		}
		document, err := oif.Apply(source.Document, changes)
		if err != nil {
			return nil, invalidMedia("The configured media body could not be encoded.")
		}
		return decode(r.Op(), JSONSource{Document: document})
	case FormSource:
		parts := slices.Clone(source.Parts)
		for _, name := range absent {
			added, failure := multipartDefault(name, defaults[name])
			if failure != nil {
				return nil, failure
			}
			parts = append(parts, added...)
		}
		return decode(r.Op(), FormSource{Parts: parts, Normalized: source.Normalized})
	}
	return nil, invalidMedia("The media request has no source.")
}

func placeable(op Operation, name string, raw json.RawMessage, present []string, cloud, multipart bool) *Error {
	collides := slices.ContainsFunc(present, func(supplied string) bool {
		return strings.HasPrefix(supplied, name+"[") || strings.HasPrefix(name, supplied+"[")
	})
	switch {
	case collides:
		return configuredFailure(name, "collides with a caller multipart member")
	case name == "stream_format" && op != OpSpeech:
		return configuredFailure(name, "has no qualified delivery control for this operation")
	case cloud && name != "n" && name != "size" && name != "response_format":
		return configuredFailure(name, "has no qualified position in the native image wrapper")
	case multipart && isJSON(raw, "null"):
		return configuredFailure(name, "native null has no qualified multipart representation")
	}
	return nil
}

func cloudRepresentable(document oif.Document) *Error {
	fields := document.Fields()
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		raw := fields[name]
		switch {
		case name == "model", name == "prompt":
		case name == "stream" && isJSON(raw, "false"):
		case (name == "n" || name == "size" || name == "response_format") && !isJSON(raw, "null"):
		default:
			return configuredFailure(name, "has no qualified representation in the native image wrapper")
		}
	}
	return nil
}

func boundModel(document oif.Document, model string) ([]byte, *Error) {
	value, err := json.Marshal(model)
	if err != nil {
		return nil, invalidMedia("The configured media body could not be encoded.")
	}
	bound, err := oif.Apply(document, []oif.Change{{Pointer: "/model", Value: string(value), Origin: oif.IdentityBinding, Reason: "selected provider model"}})
	if err != nil {
		return nil, invalidMedia("The configured media body could not be encoded.")
	}
	return bound.Bytes(), nil
}

func exactParts(parts []staging.Field, model string) []staging.Field {
	exact := slices.Clone(parts)
	for i, part := range exact {
		if _, text := part.Value.(staging.Text); text && part.Name == "model" {
			exact[i].Value = staging.Text(model)
		}
	}
	return exact
}

func multipartDefault(name string, raw json.RawMessage) ([]staging.Field, *Error) {
	if mediacontract.RepeatedMultipartDefault(name) {
		var values []string
		if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
			return nil, configuredFailure(name, "requires a nonempty array of strings in multipart")
		}
		fields := make([]staging.Field, len(values))
		for i, value := range values {
			fields[i] = staging.Field{Name: name + "[]", Value: staging.Text(value)}
		}
		return fields, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []staging.Field{{Name: name, Value: staging.Text(text)}}, nil
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil, configuredFailure(name, "is not valid JSON")
	}
	return []staging.Field{{Name: name, Value: staging.Text(compact.String())}}, nil
}

func isJSON(raw json.RawMessage, literal string) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte(literal))
}

func configuredFailure(name, detail string) *Error {
	return Fail(400, "unsupported_parameter", "The configured media field "+strconv.Quote(name)+" "+detail+".")
}
