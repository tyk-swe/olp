package media

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
)

// The elevenlabs and deepgram wires are ElevenLabs' and Deepgram's speech
// synthesis and recognition APIs. Synthesis returns the audio itself, with
// the characters it bills in a response header; recognition returns the
// vendor's transcript, read back as OpenAI's json or verbose_json.
//
// https://elevenlabs.io/docs/api-reference/text-to-speech/convert
// https://elevenlabs.io/docs/api-reference/speech-to-text/convert
// https://developers.deepgram.com/reference/text-to-speech/speak-request
// https://developers.deepgram.com/reference/speech-to-text/listen-pre-recorded
func init() {
	registerCodec("elevenlabs", codec{encode: map[string]func(*Request, string) (*UpstreamCall, *Error){
		OpSpeech:        encodeElevenLabsSpeech,
		OpTranscription: encodeElevenLabsTranscription,
	}})
	registerCodec("deepgram", codec{encode: map[string]func(*Request, string) (*UpstreamCall, *Error){
		OpSpeech:        encodeDeepgramSpeech,
		OpTranscription: encodeDeepgramTranscription,
	}})
}

// speechFormat is the OpenAI response format a speech request asks for.
func speechFormat(r *Request) string {
	if r.Format != nil {
		return *r.Format
	}
	return "mp3"
}

// refuseSpeechControls refuses the OpenAI speech controls a vendor has no
// counterpart for.
func refuseSpeechControls(r *Request, vendor string) *Error {
	if r.Instructions != nil || r.StreamFormat != nil && *r.StreamFormat != "audio" || r.Stream || len(r.Extra) > 0 {
		return invalidMedia(vendor + " speech does not support the requested speech parameters.")
	}
	return nil
}

var elevenLabsFormats = map[string]string{"mp3": "mp3_44100_128", "opus": "opus_48000_128", "pcm": "pcm_24000", "wav": "wav_24000"}

func encodeElevenLabsSpeech(r *Request, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, "ElevenLabs"); failure != nil {
		return nil, failure
	}
	format, ok := elevenLabsFormats[speechFormat(r)]
	if !ok {
		return nil, invalidMedia("ElevenLabs returns speech as mp3, opus, pcm or wav.")
	}
	if r.Voice == "" {
		return nil, invalidMedia("ElevenLabs speech names an ElevenLabs voice ID as the voice.")
	}
	fields := map[string]any{"text": r.Input, "model_id": model}
	if r.Speed != nil {
		// ElevenLabs paces speech from 0.7 to 1.2 times its natural rate.
		if *r.Speed < 0.7 || *r.Speed > 1.2 {
			return nil, invalidMedia("ElevenLabs speech speed ranges from 0.7 to 1.2.")
		}
		fields["voice_settings"] = map[string]float64{"speed": *r.Speed}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "text-to-speech/" + url.PathEscape(r.Voice), Query: url.Values{"output_format": {format}},
		JSON: body, Kind: ResponseBinary, Ambiguous: true, CharacterHeader: "Character-Cost"}, nil
}

// transcriptFormat is the OpenAI transcript format a request asks for, of
// the two a vendor that answers in JSON can render.
func transcriptFormat(r *Request, vendor string) (string, *Error) {
	format := "json"
	if r.Format != nil {
		format = *r.Format
	}
	switch {
	case format != "json" && format != "verbose_json":
		return "", invalidMedia(vendor + " transcripts are served as json or verbose_json.")
	case r.Stream, r.Temperature != nil, len(r.Include) > 0, len(r.ChunkingStrategy) > 0, len(r.KnownSpeakerNames) > 0, len(r.Extra) > 0,
		slices.ContainsFunc(r.TimestampGranularities, func(granularity string) bool { return granularity != "word" }):
		return "", invalidMedia(vendor + " transcription does not support the requested transcription parameters.")
	}
	return format, nil
}

// transcriptWord is one recognized word, timed in seconds.
type transcriptWord struct {
	Word       string
	Start, End float64
}

// vendorTranscript is a vendor's transcript as OpenAI's json, or verbose_json
// with its language, duration and words.
func vendorTranscript(format, text string, language *string, duration *float64, words []transcriptWord) *TranscriptionResult {
	result := &TranscriptionResult{Text: text, TextOnly: format == "json", Language: language, DurationSeconds: duration}
	if format == "verbose_json" && words != nil {
		timed := make([]map[string]any, len(words))
		for i, word := range words {
			timed[i] = map[string]any{"word": word.Word, "start": word.Start, "end": word.End}
		}
		result.Extra = map[string]any{"words": timed}
	}
	return result
}

func encodeElevenLabsTranscription(r *Request, model string) (*UpstreamCall, *Error) {
	format, failure := transcriptFormat(r, "ElevenLabs")
	if failure != nil {
		return nil, failure
	}
	if r.TextPrompt != nil {
		return nil, invalidMedia("ElevenLabs transcription takes no prompt.")
	}
	fields := textValue(nil, "model_id", model)
	fields = append(fields, Field{Name: "file", File: r.File})
	fields = textField(fields, "language_code", r.Language)
	return &UpstreamCall{Method: http.MethodPost, Path: "speech-to-text", Accept: "application/json", Fields: fields, Kind: ResponseTranscription, Ambiguous: true,
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
			var wire struct {
				Text     *string  `json:"text"`
				Language *string  `json:"language_code"`
				Duration *float64 `json:"audio_duration_secs"`
				Words    []struct {
					Text  string  `json:"text"`
					Type  string  `json:"type"`
					Start float64 `json:"start"`
					End   float64 `json:"end"`
				} `json:"words"`
			}
			if json.Unmarshal(body, &wire) != nil || wire.Text == nil {
				return nil, protocolError("The provider transcript is malformed.")
			}
			var words []transcriptWord
			for _, word := range wire.Words {
				if word.Type == "word" {
					words = append(words, transcriptWord{Word: word.Text, Start: word.Start, End: word.End})
				}
			}
			return vendorTranscript(format, *wire.Text, wire.Language, wire.Duration, words), nil
		}}, nil
}

// deepgramEncodings are Deepgram's encoding and container for each OpenAI
// speech format.
var deepgramEncodings = map[string]url.Values{
	"mp3":  {"encoding": {"mp3"}},
	"opus": {"encoding": {"opus"}, "container": {"ogg"}},
	"aac":  {"encoding": {"aac"}},
	"flac": {"encoding": {"flac"}},
	"wav":  {"encoding": {"linear16"}, "container": {"wav"}},
	"pcm":  {"encoding": {"linear16"}, "container": {"none"}, "sample_rate": {"24000"}},
}

// encodeDeepgramSpeech names the voice in the model, as Deepgram's Aura
// models do: aura-2-thalia-en speaks as thalia, so a request for that model
// names thalia as its voice.
func encodeDeepgramSpeech(r *Request, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, "Deepgram"); failure != nil {
		return nil, failure
	}
	if r.Speed != nil {
		return nil, invalidMedia("Deepgram speech has no documented speed range.")
	}
	if r.Voice == "" || !strings.Contains(model, "-"+r.Voice+"-") {
		return nil, invalidMedia("Deepgram speech names the voice of its model, such as thalia for aura-2-thalia-en.")
	}
	encoding, ok := deepgramEncodings[speechFormat(r)]
	if !ok {
		return nil, invalidMedia("Deepgram does not produce the requested speech format.")
	}
	query := url.Values{"model": {model}}
	for name, values := range encoding {
		query[name] = values
	}
	body, err := json.Marshal(map[string]string{"text": r.Input})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "speak", Query: query, JSON: body, Kind: ResponseBinary, Ambiguous: true, CharacterHeader: "Dg-Char-Count"}, nil
}

func encodeDeepgramTranscription(r *Request, model string) (*UpstreamCall, *Error) {
	format, failure := transcriptFormat(r, "Deepgram")
	if failure != nil {
		return nil, failure
	}
	if r.TextPrompt != nil {
		return nil, invalidMedia("Deepgram transcription takes no prompt.")
	}
	query := url.Values{"model": {model}}
	if r.Language != nil {
		query.Set("language", *r.Language)
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "listen", Query: query, Accept: "application/json", Upload: r.File, Kind: ResponseTranscription, Ambiguous: true,
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
			var wire struct {
				Metadata struct {
					Duration *float64 `json:"duration"`
				} `json:"metadata"`
				Results struct {
					Channels []struct {
						Language     *string `json:"detected_language"`
						Alternatives []struct {
							Transcript *string `json:"transcript"`
							Words      []struct {
								Word  string  `json:"word"`
								Start float64 `json:"start"`
								End   float64 `json:"end"`
							} `json:"words"`
						} `json:"alternatives"`
					} `json:"channels"`
				} `json:"results"`
			}
			if json.Unmarshal(body, &wire) != nil || len(wire.Results.Channels) == 0 || len(wire.Results.Channels[0].Alternatives) == 0 || wire.Results.Channels[0].Alternatives[0].Transcript == nil {
				return nil, protocolError("The provider transcript is malformed.")
			}
			channel := wire.Results.Channels[0]
			best := channel.Alternatives[0]
			words := []transcriptWord{}
			for _, word := range best.Words {
				words = append(words, transcriptWord{Word: word.Word, Start: word.Start, End: word.End})
			}
			language := channel.Language
			if language == nil {
				language = r.Language
			}
			return vendorTranscript(format, *best.Transcript, language, wire.Metadata.Duration, words), nil
		}}, nil
}

// The polly wire is Amazon Polly's SynthesizeSpeech, which a Bedrock
// provider reaches in its region with its AWS credentials, signed for
// Polly. The model is the Polly engine and the voice a Polly voice ID.
//
// https://docs.aws.amazon.com/polly/latest/dg/API_SynthesizeSpeech.html
func init() {
	registerCodec("polly", codec{encodeFor: map[string]func(*Request, connectors.Config, string) (*UpstreamCall, *Error){
		OpSpeech: encodePollySpeech,
	}})
}

// pollyFormats are Polly's output formats for the OpenAI ones it serves:
// its PCM is 16 kHz, not OpenAI's 24 kHz, and it makes no WAV.
var pollyFormats = map[string]string{"mp3": "mp3", "opus": "ogg_opus"}

func encodePollySpeech(r *Request, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, "Amazon Polly"); failure != nil {
		return nil, failure
	}
	format, ok := pollyFormats[speechFormat(r)]
	switch {
	case !ok:
		return nil, invalidMedia("Amazon Polly returns speech as mp3 or opus.")
	case r.Speed != nil:
		return nil, invalidMedia("Amazon Polly paces speech through SSML, not a speed.")
	case !slices.Contains([]string{"standard", "neural", "long-form", "generative"}, model):
		return nil, invalidMedia("The model names a Polly engine: standard, neural, long-form or generative.")
	case r.Voice == "":
		return nil, invalidMedia("Amazon Polly speech names a Polly voice ID, such as Joanna.")
	}
	endpoint, err := connectors.PollyEndpoint(cfg.CloudRegion)
	if err != nil {
		return nil, invalidMedia(err.Error())
	}
	body, err := json.Marshal(map[string]string{"Text": r.Input, "VoiceId": r.Voice, "Engine": model, "OutputFormat": format})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, URL: endpoint + "/v1/speech", SigningService: "polly", JSON: body, Kind: ResponseBinary, Ambiguous: true,
		CharacterHeader: "X-Amzn-Requestcharacters"}, nil
}
