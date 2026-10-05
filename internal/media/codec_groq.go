package media

import "slices"

// The groq-audio wire is Groq's OpenAI-compatible speech-to-text API. Groq
// bills audio by the second but reports a duration only in verbose_json, so
// a request for json is sent as verbose_json and its result read back as
// the json client asked for.
//
// https://console.groq.com/docs/speech-to-text
func init() {
	registerCodec("groq-audio", codec{encode: map[string]func(*Request, string) (*UpstreamCall, *Error){
		OpTranscription: encodeGroqAudio,
		OpTranslation:   encodeGroqAudio,
	}})
}

func encodeGroqAudio(r *Request, model string) (*UpstreamCall, *Error) {
	format := "json"
	if r.Format != nil {
		format = *r.Format
	}
	switch {
	case r.Stream:
		return nil, invalidMedia("Groq does not stream transcripts.")
	case !slices.Contains([]string{"json", "text", "verbose_json"}, format):
		return nil, invalidMedia("Groq returns transcripts as json, text or verbose_json.")
	case len(r.Include) > 0, len(r.ChunkingStrategy) > 0, len(r.KnownSpeakerNames) > 0:
		return nil, invalidMedia("Groq does not support the requested transcription parameters.")
	}
	sent := *r
	if format == "json" {
		sent.Format = new("verbose_json")
	}
	encode := encodeTranscription
	if r.Op == OpTranslation {
		encode = encodeTranslation
	}
	call, failure := encode(&sent, model)
	if failure != nil || format != "json" {
		return call, failure
	}
	call.DecodeTranscription = func(body []byte) (*TranscriptionResult, *Error) {
		result, failure := DecodeTranscriptionJSON(body)
		if result != nil {
			result.TextOnly = true
		}
		return result, failure
	}
	return call, nil
}
