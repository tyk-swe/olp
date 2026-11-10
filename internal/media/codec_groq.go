package media

import (
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
)

var groqAudioCodec = codec{transcription: encodeGroqTranscription, translation: encodeGroqTranslation}

func encodeGroqTranscription(r *Request, p Transcription, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	if p.Stream {
		return nil, invalidMedia("Groq does not stream transcripts.")
	}
	format, failure := groqFormat(p.Format)
	if failure != nil {
		return nil, failure
	}
	if len(p.Include) > 0 || len(p.ChunkingStrategy) > 0 || len(p.KnownSpeakerNames) > 0 {
		return nil, invalidMedia("Groq does not support the requested transcription parameters.")
	}
	return groqCall(format)(encodeTranscription(r, p.withFormat(groqWireFormat(format)), cfg, model))
}

func encodeGroqTranslation(r *Request, p Translation, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	format, failure := groqFormat(p.Format)
	if failure != nil {
		return nil, failure
	}
	return groqCall(format)(encodeTranslation(r, p.withFormat(groqWireFormat(format)), cfg, model))
}

func groqFormat(format *string) (string, *Error) {
	effective := valueOr(format, "json")
	if !slices.Contains([]string{"json", "text", "verbose_json"}, effective) {
		return "", invalidMedia("Groq returns transcripts as json, text or verbose_json.")
	}
	return effective, nil
}

func groqWireFormat(format string) *string {
	if format == "json" {
		return new("verbose_json")
	}
	return &format
}

func groqCall(format string) func(*UpstreamCall, *Error) (*UpstreamCall, *Error) {
	return func(call *UpstreamCall, failure *Error) (*UpstreamCall, *Error) {
		if failure != nil {
			return nil, failure
		}
		return call.withResponse(groqReply(format)), nil
	}
}

func groqReply(format string) TranscriptReply {
	if format == "json" {
		return TranscriptReply{Format: format, Decode: DecodeTranscriptionJSON}
	}
	return TranscriptReply{Format: format}
}
