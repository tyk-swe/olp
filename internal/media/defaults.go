package media

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
)

// EncodeConfigured fills omitted operation members from the selected profile
// and binding. The returned request owns detached metadata for response decoding
// and accounting; its staged file handles retain the caller's lifetime ownership.
func EncodeConfigured(request *Request, cfg connectors.Config, model string) (*UpstreamCall, *Request, *Error) {
	if request == nil {
		return nil, nil, invalidMedia("The media request is required.")
	}
	effective := cloneConfiguredRequest(request)
	if cfg.ProfileID == "" {
		call, failure := Encode(effective, cfg.Kind, cfg.Model(model))
		return call, effective, failure
	}
	if err := cfg.ValidateProfile(); err != nil {
		return nil, nil, invalidMedia(err.Error())
	}
	profile, _ := cfg.Profile()
	if !slices.Contains(profile.Operations, request.Op) {
		return nil, nil, invalidMedia("The selected profile does not own this media operation.")
	}
	defaults, _, err := cfg.DefaultsFor(request.Op, model)
	if err != nil {
		return nil, nil, invalidMedia(err.Error())
	}
	fields, failure := configuredFields(request)
	if failure != nil {
		return nil, nil, failure
	}
	cloud := request.Op == OpImageGeneration && (cfg.Kind == "vertex_ai" || cfg.Kind == "bedrock")
	multipart := request.Op != OpImageGeneration && request.Op != OpSpeech
	applied := map[string]json.RawMessage{}
	for _, name := range slices.Sorted(maps.Keys(defaults)) {
		if _, present := fields[name]; present {
			continue
		}
		for present := range fields {
			if strings.HasPrefix(present, name+"[") || strings.HasPrefix(name, present+"[") {
				return nil, nil, configuredFailure(name, "collides with a caller multipart member")
			}
		}
		raw := defaults[name]
		if name == "stream_format" && request.Op != OpSpeech {
			return nil, nil, configuredFailure(name, "has no qualified delivery control for this operation")
		}
		if cloud && (name != "n" && name != "size" && name != "response_format") {
			return nil, nil, configuredFailure(name, "has no qualified position in the native image wrapper")
		}
		if multipart && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, nil, configuredFailure(name, "native null has no qualified multipart representation")
		}
		if failure := applyConfiguredField(effective, name, raw); failure != nil {
			return nil, nil, failure
		}
		fields[name], applied[name] = bytes.Clone(raw), bytes.Clone(raw)
	}
	if effective.Mode() != request.Mode() {
		return nil, nil, configuredFailure("stream_format", "cannot change the caller's delivery mode")
	}
	if failure := validateConfiguredResponse(effective); failure != nil {
		return nil, nil, failure
	}
	if cloud {
		for name, raw := range fields {
			switch name {
			case "model", "prompt":
			case "stream":
				if !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
					return nil, nil, configuredFailure(name, "has no qualified representation in the native image wrapper")
				}
			case "n", "size", "response_format":
				if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					continue
				}
				fallthrough
			default:
				return nil, nil, configuredFailure(name, "has no qualified representation in the native image wrapper")
			}
		}
		call, failure := Encode(effective, cfg.Kind, cfg.Model(model))
		return call, effective, failure
	}
	if !multipart {
		// Source members remain raw JSON, including native null, exact numbers,
		// arrays and schema definitions. Only the authorized model is replaced.
		fields["model"], _ = json.Marshal(cfg.Model(model))
		body, err := json.Marshal(fields)
		if err != nil {
			return nil, nil, invalidMedia("The configured media body could not be encoded.")
		}
		call, failure := Encode(effective, cfg.Kind, cfg.Model(model))
		if failure != nil {
			return nil, nil, failure
		}
		call.JSON = body
		return call, effective, nil
	}
	// Keep every original file and field in its established order; append only
	// defaults. Using Encode(effective) here would expand/drop arbitrary arrays.
	call, failure := Encode(cloneConfiguredRequest(request), cfg.Kind, cfg.Model(model))
	if failure != nil {
		return nil, nil, failure
	}
	if len(applied) != 0 && len(call.Fields) == 0 {
		return nil, nil, configuredFailure("defaults", "this resource operation has no payload default positions")
	}
	for _, name := range slices.Sorted(maps.Keys(applied)) {
		added, failure := configuredMultipartFields(name, applied[name])
		if failure != nil {
			return nil, nil, failure
		}
		call.Fields = append(call.Fields, added...)
	}
	return call, effective, nil
}

func configuredFailure(name, detail string) *Error {
	return Fail(400, "unsupported_parameter", "The configured media field "+strconv.Quote(name)+" "+detail+".")
}

// sourceMediaFields owns exact native member bytes independently of the caller.
// The shared OIF parser rejects ambiguity anywhere in the document, including
// nested duplicate names and malformed Unicode that encoding/json would repair.
func sourceMediaFields(body []byte) (oif.Document, map[string]json.RawMessage, error) {
	doc, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		return oif.Document{}, nil, err
	}
	if doc.Root().Kind() != oif.Object {
		return oif.Document{}, nil, errors.New("expected media object")
	}
	return doc, doc.Fields(), nil
}

func configuredFields(r *Request) (map[string]json.RawMessage, *Error) {
	if r.SourceFields != nil {
		return cloneSourceFields(r.SourceFields), nil
	}
	fields := map[string]json.RawMessage{}
	add := func(name string, value any, present bool) {
		if present {
			fields[name], _ = json.Marshal(value)
		}
	}
	add("model", r.Route, true)
	add("prompt", r.Prompt, r.Op == OpImageGeneration || r.Op == OpImageEdit || r.Op == OpVideoCreate)
	add("input", r.Input, r.Op == OpSpeech)
	add("voice", r.Voice, r.Op == OpSpeech)
	add("stream", r.Stream, r.Op == OpImageEdit || r.Op == OpTranscription || r.Op == OpImageGeneration && r.Stream)
	add("n", r.Count, r.Count != nil)
	for _, member := range []struct {
		name  string
		value *string
	}{
		{"size", r.Size}, {"response_format", r.Format}, {"quality", r.Quality}, {"style", r.Style}, {"user", r.User}, {"background", r.Background}, {"moderation", r.Moderation}, {"input_fidelity", r.InputFidelity}, {"output_format", r.OutputFormat}, {"language", r.Language}, {"prompt", r.TextPrompt}, {"stream_format", r.StreamFormat}, {"instructions", r.Instructions}, {"seconds", r.Seconds},
	} {
		add(member.name, member.value, member.value != nil)
	}
	add("output_compression", r.OutputCompression, r.OutputCompression != nil)
	add("partial_images", r.PartialImages, r.PartialImages != nil)
	add("temperature", r.Temperature, r.Temperature != nil)
	add("speed", r.Speed, r.Speed != nil)
	add("include", r.Include, r.Include != nil)
	add("timestamp_granularities", r.TimestampGranularities, r.TimestampGranularities != nil)
	add("chunking_strategy", r.ChunkingStrategy, r.ChunkingStrategy != nil)
	add("known_speaker_names", r.KnownSpeakerNames, r.KnownSpeakerNames != nil)
	add("known_speaker_references", r.KnownSpeakerReferences, r.KnownSpeakerReferences != nil)
	for name, value := range r.Extra {
		if _, collision := fields[name]; collision {
			return nil, configuredFailure(name, "collides with a request field")
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, configuredFailure(name, "is not valid JSON")
		}
		fields[name] = raw
	}
	return fields, nil
}

func applyConfiguredField(r *Request, name string, raw json.RawMessage) *Error {
	var target any
	switch name {
	case "n":
		target = &r.Count
	case "size":
		target = &r.Size
	case "response_format":
		target = &r.Format
	case "quality":
		target = &r.Quality
	case "style":
		target = &r.Style
	case "user":
		target = &r.User
	case "background":
		target = &r.Background
	case "moderation":
		target = &r.Moderation
	case "input_fidelity":
		target = &r.InputFidelity
	case "output_compression":
		target = &r.OutputCompression
	case "output_format":
		target = &r.OutputFormat
	case "partial_images":
		target = &r.PartialImages
	case "language":
		target = &r.Language
	case "temperature":
		target = &r.Temperature
	case "speed":
		target = &r.Speed
	case "voice":
		target = &r.Voice
	case "instructions":
		target = &r.Instructions
	case "seconds":
		target = &r.Seconds
	case "include":
		target = &r.Include
	case "timestamp_granularities":
		target = &r.TimestampGranularities
	case "known_speaker_names":
		target = &r.KnownSpeakerNames
	case "known_speaker_references":
		target = &r.KnownSpeakerReferences
	case "chunking_strategy":
		r.ChunkingStrategy = bytes.Clone(raw)
		return nil
	case "stream_format":
		target = &r.StreamFormat
	default:
		if r.Extra == nil {
			r.Extra = map[string]any{}
		}
		r.Extra[name] = json.RawMessage(bytes.Clone(raw))
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return configuredFailure(name, "does not match the operation's field type")
	}
	if name == "stream_format" {
		r.Stream = r.StreamFormat != nil && *r.StreamFormat == "sse"
	}
	return nil
}

func configuredMultipartFields(name string, raw json.RawMessage) ([]Field, *Error) {
	if slices.Contains([]string{"include", "timestamp_granularities", "known_speaker_names", "known_speaker_references"}, name) {
		var values []string
		if json.Unmarshal(raw, &values) != nil || len(values) == 0 {
			return nil, configuredFailure(name, "requires a nonempty array of strings in multipart")
		}
		fields := make([]Field, 0, len(values))
		for _, value := range values {
			fields = textValue(fields, name+"[]", value)
		}
		return fields, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []Field{{Name: name, Text: &text}}, nil
	}
	var compact bytes.Buffer
	if json.Compact(&compact, raw) != nil {
		return nil, configuredFailure(name, "is not valid JSON")
	}
	text = compact.String()
	return []Field{{Name: name, Text: &text, Raw: true}}, nil
}

func validateConfiguredResponse(r *Request) *Error {
	if r.Op == OpSpeech && r.Speed != nil && (*r.Speed < 0.25 || *r.Speed > 4) {
		return configuredFailure("speed", "must be between 0.25 and 4")
	}
	if r.Op == OpTranscription {
		format := "json"
		if r.Format != nil {
			format = *r.Format
		}
		if !slices.Contains([]string{"json", "text", "srt", "verbose_json", "vtt", "diarized_json"}, format) {
			return configuredFailure("response_format", "is not supported")
		}
		if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 1) {
			return configuredFailure("temperature", "must be between 0 and 1")
		}
		if len(r.TimestampGranularities) > 0 && (format != "verbose_json" || !allOf(r.TimestampGranularities, "word", "segment")) {
			return configuredFailure("timestamp_granularities", "requires verbose_json and word or segment values")
		}
		if len(r.Include) > 0 && (format != "json" || !allOf(r.Include, "logprobs")) {
			return configuredFailure("include", "requires json and logprobs values")
		}
		if (len(r.KnownSpeakerNames) > 0 || len(r.KnownSpeakerReferences) > 0) && (format != "diarized_json" || len(r.KnownSpeakerNames) == 0 || len(r.KnownSpeakerNames) > 4 || len(r.KnownSpeakerNames) != len(r.KnownSpeakerReferences) || !validSpeakerNames(r.KnownSpeakerNames) || !validSpeakerReferences(r.KnownSpeakerReferences)) {
			return configuredFailure("known_speaker_names", "requires diarized_json and at most four complete speaker pairs")
		}
	}
	if r.Op == OpVideoCreate {
		if r.Seconds != nil && !slices.Contains([]string{"4", "8", "12"}, *r.Seconds) {
			return configuredFailure("seconds", "must be 4, 8 or 12")
		}
		if r.Size != nil && !slices.Contains([]string{"720x1280", "1280x720", "1024x1792", "1792x1024"}, *r.Size) {
			return configuredFailure("size", "is not supported for video")
		}
	}
	return nil
}

func cloneSourceFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	if fields == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(fields))
	for name, value := range fields {
		out[name] = bytes.Clone(value)
	}
	return out
}

func configuredCopy[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneConfiguredRequest(r *Request) *Request {
	out := *r
	out.Count, out.OutputCompression, out.PartialImages = configuredCopy(r.Count), configuredCopy(r.OutputCompression), configuredCopy(r.PartialImages)
	out.Size, out.Format, out.Quality, out.Style, out.User = configuredCopy(r.Size), configuredCopy(r.Format), configuredCopy(r.Quality), configuredCopy(r.Style), configuredCopy(r.User)
	out.Background, out.Moderation, out.InputFidelity, out.OutputFormat = configuredCopy(r.Background), configuredCopy(r.Moderation), configuredCopy(r.InputFidelity), configuredCopy(r.OutputFormat)
	out.Language, out.TextPrompt, out.StreamFormat, out.Instructions, out.Seconds = configuredCopy(r.Language), configuredCopy(r.TextPrompt), configuredCopy(r.StreamFormat), configuredCopy(r.Instructions), configuredCopy(r.Seconds)
	out.Temperature, out.Speed = configuredCopy(r.Temperature), configuredCopy(r.Speed)
	out.Include, out.TimestampGranularities = slices.Clone(r.Include), slices.Clone(r.TimestampGranularities)
	out.KnownSpeakerNames, out.KnownSpeakerReferences = slices.Clone(r.KnownSpeakerNames), slices.Clone(r.KnownSpeakerReferences)
	out.ChunkingStrategy = bytes.Clone(r.ChunkingStrategy)
	out.Images = slices.Clone(r.Images)
	out.Mask, out.Image, out.File, out.InputRef = configuredCopy(r.Mask), configuredCopy(r.Image), configuredCopy(r.File), configuredCopy(r.InputRef)
	out.SourceFields = cloneSourceFields(r.SourceFields)
	if r.Extra != nil {
		out.Extra = make(map[string]any, len(r.Extra))
		for name, value := range r.Extra {
			out.Extra[name] = cloneMediaExtra(value)
		}
	}
	return &out
}

func cloneMediaExtra(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for name, item := range value {
			out[name] = cloneMediaExtra(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = cloneMediaExtra(item)
		}
		return out
	case []string:
		return slices.Clone(value)
	case json.RawMessage:
		return json.RawMessage(bytes.Clone(value))
	default:
		return value
	}
}
