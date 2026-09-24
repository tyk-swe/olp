package media

import (
	"math"
	"slices"
	"strings"
)

// DecodeTranslation retains the native audio translation form. Unlike
// transcription, translation has no language, streaming or diarization controls.
func DecodeTranslation(form *Form) (*Request, *Error) {
	file, failure := form.TakeSingleFile("file")
	if failure != nil {
		return nil, failure
	}
	model, failure := form.Required("model")
	if failure != nil {
		return nil, failure
	}
	format, failure := form.Optional("response_format")
	if failure != nil {
		return nil, failure
	}
	prompt, failure := form.Optional("prompt")
	if failure != nil {
		return nil, failure
	}
	temperature, failure := form.OptionalFloat("temperature")
	if failure != nil {
		return nil, failure
	}
	extra, failure := form.TakeExtensions()
	if failure != nil {
		return nil, failure
	}
	if file == nil || !validRouteSlug(model) {
		return nil, invalidMedia("Audio translation requires a file and a model naming a route.")
	}
	if format != nil && !slices.Contains([]string{"json", "text", "srt", "vtt", "verbose_json"}, *format) {
		return nil, invalidMedia("The translation response_format is not supported.")
	}
	if temperature != nil && (math.IsNaN(*temperature) || *temperature < 0 || *temperature > 1) {
		return nil, invalidMedia("The translation temperature must be between 0 and 1.")
	}
	extraAny := make(map[string]any, len(extra))
	for name, value := range extra {
		control, _, _ := strings.Cut(name, "[")
		if slices.Contains([]string{"stream", "language", "include", "timestamp_granularities", "chunking_strategy", "known_speaker_names", "known_speaker_references"}, control) {
			return nil, Fail(400, "unsupported_parameter", "Audio translation does not support transcription controls.")
		}
		extraAny[name] = value
	}
	parts, normalized := form.SourceFields()
	return &Request{Op: OpTranslation, Route: model, File: file, Format: format,
		TextPrompt: prompt, Temperature: temperature, Extra: extraAny,
		SourceParts: parts, SourceNormalized: normalized}, nil
}

func encodeTranslation(r *Request, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = append(fields, Field{Name: "file", File: r.File})
	fields = textField(fields, "prompt", r.TextPrompt)
	fields = textField(fields, "response_format", r.Format)
	fields = textField(fields, "temperature", float64Text(r.Temperature))
	return &UpstreamCall{Method: "POST", Path: "audio/translations", Accept: "application/json",
		Fields: extraFields(fields, r.Extra), Kind: ResponseTranscription, Ambiguous: true}, nil
}
