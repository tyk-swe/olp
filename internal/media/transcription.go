package media

import (
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
)

type Transcription struct {
	File                   staging.Part
	Format                 *string
	Language               *string
	Prompt                 *string
	Temperature            *float64
	Include                []string
	TimestampGranularities []string
	ChunkingStrategy       string
	KnownSpeakerNames      []string
	KnownSpeakerReferences []string
	Stream                 bool
}

type Translation struct {
	File        staging.Part
	Format      *string
	Prompt      *string
	Temperature *float64
}

func (Transcription) Operation() Operation      { return OpTranscription }
func (Translation) Operation() Operation        { return OpTranslation }
func (p Transcription) uploads() []staging.Part { return []staging.Part{p.File} }
func (p Translation) uploads() []staging.Part   { return []staging.Part{p.File} }
func (p Transcription) streaming() bool         { return p.Stream }
func (Translation) streaming() bool             { return false }

func DecodeTranscription(form *staging.Form) (*Request, *Error) {
	file := form.TakeSingleFile("file")
	format := form.Optional("response_format")
	names := form.TakeRepeated("known_speaker_names")
	references := form.TakeRepeated("known_speaker_references")
	route := form.Required("model")
	language, prompt, temperature := form.Optional("language"), form.Optional("prompt"), form.OptionalFloat("temperature")
	include := form.TakeRepeated("include")
	granularities := form.TakeRepeated("timestamp_granularities")
	chunking := form.Optional("chunking_strategy")
	stream := form.OptionalBool("stream")
	extra, failure := formExtensions(form)
	effective := valueOr(format, "json")
	switch {
	case failure != nil:
		return nil, failure
	case file == nil:
		return nil, invalidMedia("The audio file is required.")
	case !validTemperature(temperature):
		return nil, invalidMedia("The transcription temperature must be between 0 and 1.")
	case !wire.ValidTranscriptionResponseFormat(effective):
		return nil, invalidMedia("The transcription response_format is not supported.")
	case len(granularities) > 0 && (effective != "verbose_json" || !allOf(granularities, "word", "segment")):
		return nil, invalidMedia("The timestamp_granularities field requires verbose_json with word or segment values.")
	case len(include) > 0 && (effective != "json" || !allOf(include, "logprobs")):
		return nil, invalidMedia("The include field requires the json response format with logprobs values.")
	case (len(names) > 0 || len(references) > 0) && (effective != "diarized_json" || len(names) == 0 || len(names) > 4 ||
		len(names) != len(references) || !validSpeakerNames(names) || !validSpeakerReferences(references)):
		return nil, invalidMedia("The known speaker fields require diarized_json with at most four name/reference pairs.")
	case chunking != nil && *chunking != "auto" && !json.Valid([]byte(*chunking)):
		return nil, invalidMedia(`chunking_strategy must be "auto" or JSON.`)
	}
	p := Transcription{
		File: *file, Format: format, Language: language, Prompt: prompt, Temperature: temperature,
		Include: include, TimestampGranularities: granularities, KnownSpeakerNames: names,
		KnownSpeakerReferences: references, Stream: valueOf(stream),
	}
	if chunking != nil {
		p.ChunkingStrategy = *chunking
	}
	return formRequest(form, route, p, extra)
}

var transcriptionControls = []string{"stream", "language", "include", "timestamp_granularities", "chunking_strategy", "known_speaker_names", "known_speaker_references"}

func DecodeTranslation(form *staging.Form) (*Request, *Error) {
	file := form.TakeSingleFile("file")
	route := form.Required("model")
	format, prompt, temperature := form.Optional("response_format"), form.Optional("prompt"), form.OptionalFloat("temperature")
	extra, failure := formExtensions(form)
	switch {
	case failure != nil:
		return nil, failure
	case file == nil || !validRouteSlug(route):
		return nil, invalidMedia("Audio translation requires a file and a model naming a route.")
	case format != nil && !wire.ValidTranslationResponseFormat(*format):
		return nil, invalidMedia("The translation response_format is not supported.")
	case !validTemperature(temperature):
		return nil, invalidMedia("The translation temperature must be between 0 and 1.")
	}
	for name := range extra {
		if control, _, _ := strings.Cut(name, "["); slices.Contains(transcriptionControls, control) {
			return nil, Fail(400, "unsupported_parameter", "Audio translation does not support transcription controls.")
		}
	}
	return formRequest(form, route, Translation{File: *file, Format: format, Prompt: prompt, Temperature: temperature}, extra)
}

func validTemperature(value *float64) bool {
	return value == nil || !math.IsNaN(*value) && *value >= 0 && *value <= 1
}

func allOf(values []string, allowed ...string) bool {
	return !slices.ContainsFunc(values, func(value string) bool { return !slices.Contains(allowed, value) })
}

func validSpeakerNames(names []string) bool {
	return !slices.ContainsFunc(names, func(name string) bool { return strings.TrimSpace(name) == "" || len(name) > 64 })
}

func validSpeakerReferences(references []string) bool {
	return !slices.ContainsFunc(references, func(reference string) bool { return !strings.HasPrefix(reference, "data:audio/") })
}

func encodeTranscription(r *Request, p Transcription, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = append(fields, staging.Field{Name: "file", Value: p.File})
	fields = textField(fields, "language", p.Language)
	fields = textField(fields, "prompt", p.Prompt)
	fields = textField(fields, "response_format", p.Format)
	fields = textField(fields, "temperature", float64Text(p.Temperature))
	for _, value := range p.Include {
		fields = textValue(fields, "include[]", value)
	}
	for _, value := range p.TimestampGranularities {
		fields = textValue(fields, "timestamp_granularities[]", value)
	}
	if len(p.ChunkingStrategy) > 0 {
		fields = textValue(fields, "chunking_strategy", p.ChunkingStrategy)
	}
	for i, name := range p.KnownSpeakerNames {
		fields = textValue(fields, "known_speaker_names[]", name)
		fields = textValue(fields, "known_speaker_references[]", p.KnownSpeakerReferences[i])
	}
	fields = textValue(fields, "stream", strconv.FormatBool(p.Stream))
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "audio/transcriptions"}, Accept: "application/json",
		Body: FormBody(extraFields(fields, r.Extra())), Response: transcriptReply(p.Stream, valueOr(p.Format, "json")), Ambiguous: true}, nil
}

func encodeTranslation(r *Request, p Translation, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = append(fields, staging.Field{Name: "file", Value: p.File})
	fields = textField(fields, "prompt", p.Prompt)
	fields = textField(fields, "response_format", p.Format)
	fields = textField(fields, "temperature", float64Text(p.Temperature))
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "audio/translations"}, Accept: "application/json",
		Body: FormBody(extraFields(fields, r.Extra())), Response: TranscriptReply{Format: valueOr(p.Format, "json")}, Ambiguous: true}, nil
}

func (p Transcription) withFormat(format *string) Transcription {
	p.Format = format
	return p
}

func (p Translation) withFormat(format *string) Translation {
	p.Format = format
	return p
}

func transcriptReply(stream bool, format string) Response {
	if stream {
		return EventReply{}
	}
	return TranscriptReply{Format: format}
}
