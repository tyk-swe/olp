package media

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

var elevenLabsCodec = codec{speech: encodeElevenLabsSpeech, transcription: encodeElevenLabsTranscription}

var deepgramCodec = codec{speech: encodeDeepgramSpeech, transcription: encodeDeepgramTranscription}

var pollyCodec = codec{speech: encodePollySpeech}

var assemblyAICodec = codec{transcription: encodeAssemblyAI}

func refuseSpeechControls(r *Request, p Speech, vendor string) *Error {
	if p.Instructions != nil || p.StreamFormat != nil && *p.StreamFormat != "audio" || len(r.Extra()) > 0 {
		return invalidMedia(vendor + " speech does not support the requested speech parameters.")
	}
	return nil
}

var elevenLabsFormats = map[string]string{"mp3": "mp3_44100_128", "opus": "opus_48000_128", "pcm": "pcm_24000", "wav": "wav_24000"}

func encodeElevenLabsSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "ElevenLabs"); failure != nil {
		return nil, failure
	}
	format, ok := elevenLabsFormats[valueOr(p.Format, "mp3")]
	if !ok {
		return nil, invalidMedia("ElevenLabs returns speech as mp3, opus, pcm or wav.")
	}
	if p.Voice == "" {
		return nil, invalidMedia("ElevenLabs speech names an ElevenLabs voice ID as the voice.")
	}
	fields := map[string]any{"text": p.Input, "model_id": model}
	if p.Speed != nil {
		if *p.Speed < 0.7 || *p.Speed > 1.2 {
			return nil, invalidMedia("ElevenLabs speech speed ranges from 0.7 to 1.2.")
		}
		fields["voice_settings"] = map[string]float64{"speed": *p.Speed}
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "text-to-speech/" + url.PathEscape(p.Voice), Query: url.Values{"output_format": {format}},
		JSON: body, Kind: ResponseBinary, Ambiguous: true, CharacterHeader: "Character-Cost"}, nil
}

func vendorTranscriptFormat(r *Request, p Transcription, vendor string, formats ...string) (string, *Error) {
	format := valueOr(p.Format, "json")
	switch last := len(formats) - 1; {
	case !slices.Contains(formats, format):
		return "", invalidMedia(vendor + " transcripts are served as " + strings.Join(formats[:last], ", ") + " or " + formats[last] + ".")
	case p.Stream, p.Temperature != nil, len(p.Include) > 0, len(p.ChunkingStrategy) > 0, len(p.KnownSpeakerNames) > 0, len(r.Extra()) > 0,
		slices.ContainsFunc(p.TimestampGranularities, func(granularity string) bool { return granularity != "word" }):
		return "", invalidMedia(vendor + " transcription does not support the requested transcription parameters.")
	}
	return format, nil
}

type transcriptWord struct {
	Word       string
	Start, End float64
}

func vendorTranscript(format, text string, language *string, duration *float64, words []transcriptWord) *TranscriptionResult {
	result := &TranscriptionResult{Text: text, Language: language, DurationSeconds: duration}
	if format == "verbose_json" && words != nil {
		timed := make([]map[string]any, len(words))
		for i, word := range words {
			timed[i] = map[string]any{"word": word.Word, "start": word.Start, "end": word.End}
		}
		result.Extra = map[string]any{"words": timed}
	}
	return result
}

func encodeElevenLabsTranscription(r *Request, p Transcription, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	format, failure := vendorTranscriptFormat(r, p, "ElevenLabs", "json", "verbose_json")
	if failure != nil {
		return nil, failure
	}
	if p.Prompt != nil {
		return nil, invalidMedia("ElevenLabs transcription takes no prompt.")
	}
	fields := textValue(nil, "model_id", model)
	fields = append(fields, staging.Field{Name: "file", Value: p.File})
	fields = textField(fields, "language_code", p.Language)
	return &UpstreamCall{Method: http.MethodPost, Path: "speech-to-text", Accept: "application/json", Fields: fields, Kind: ResponseTranscription, Ambiguous: true,
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
			var encodedWire struct {
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
			if json.Unmarshal(body, &encodedWire) != nil || encodedWire.Text == nil {
				return nil, protocolError("The provider transcript is malformed.")
			}
			var words []transcriptWord
			for _, word := range encodedWire.Words {
				if word.Type == "word" {
					words = append(words, transcriptWord{Word: word.Text, Start: word.Start, End: word.End})
				}
			}
			return vendorTranscript(format, *encodedWire.Text, encodedWire.Language, encodedWire.Duration, words), nil
		}}, nil
}

var deepgramEncodings = map[string]url.Values{
	"mp3":  {"encoding": {"mp3"}},
	"opus": {"encoding": {"opus"}, "container": {"ogg"}},
	"aac":  {"encoding": {"aac"}},
	"flac": {"encoding": {"flac"}},
	"wav":  {"encoding": {"linear16"}, "container": {"wav"}},
	"pcm":  {"encoding": {"linear16"}, "container": {"none"}, "sample_rate": {"24000"}},
}

func encodeDeepgramSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "Deepgram"); failure != nil {
		return nil, failure
	}
	if p.Speed != nil {
		return nil, invalidMedia("Deepgram speech has no documented speed range.")
	}
	if p.Voice == "" || !strings.Contains(model, "-"+p.Voice+"-") {
		return nil, invalidMedia("Deepgram speech names the voice of its model, such as thalia for aura-2-thalia-en.")
	}
	encoding, ok := deepgramEncodings[valueOr(p.Format, "mp3")]
	if !ok {
		return nil, invalidMedia("Deepgram does not produce the requested speech format.")
	}
	query := url.Values{"model": {model}}
	for name, values := range encoding {
		query[name] = values
	}
	body, err := json.Marshal(map[string]string{"text": p.Input})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "speak", Query: query, JSON: body, Kind: ResponseBinary, Ambiguous: true, CharacterHeader: "Dg-Char-Count"}, nil
}

func encodeDeepgramTranscription(r *Request, p Transcription, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	format, failure := vendorTranscriptFormat(r, p, "Deepgram", "json", "verbose_json")
	if failure != nil {
		return nil, failure
	}
	if p.Prompt != nil {
		return nil, invalidMedia("Deepgram transcription takes no prompt.")
	}
	query := url.Values{"model": {model}}
	if p.Language != nil {
		query.Set("language", *p.Language)
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "listen", Query: query, Accept: "application/json", Upload: &p.File, Kind: ResponseTranscription, Ambiguous: true,
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
			var encodedWire struct {
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
			if json.Unmarshal(body, &encodedWire) != nil || len(encodedWire.Results.Channels) == 0 || len(encodedWire.Results.Channels[0].Alternatives) == 0 || encodedWire.Results.Channels[0].Alternatives[0].Transcript == nil {
				return nil, protocolError("The provider transcript is malformed.")
			}
			channel := encodedWire.Results.Channels[0]
			best := channel.Alternatives[0]
			words := []transcriptWord{}
			for _, word := range best.Words {
				words = append(words, transcriptWord{Word: word.Word, Start: word.Start, End: word.End})
			}
			language := channel.Language
			if language == nil {
				language = p.Language
			}
			return vendorTranscript(format, *best.Transcript, language, encodedWire.Metadata.Duration, words), nil
		}}, nil
}

var pollyFormats = map[string]string{"mp3": "mp3", "opus": "ogg_opus"}

func encodePollySpeech(r *Request, p Speech, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "Amazon Polly"); failure != nil {
		return nil, failure
	}
	format, ok := pollyFormats[valueOr(p.Format, "mp3")]
	switch {
	case !ok:
		return nil, invalidMedia("Amazon Polly returns speech as mp3 or opus.")
	case p.Speed != nil:
		return nil, invalidMedia("Amazon Polly paces speech through SSML, not a speed.")
	case !slices.Contains([]string{"standard", "neural", "long-form", "generative"}, model):
		return nil, invalidMedia("The model names a Polly engine: standard, neural, long-form or generative.")
	case p.Voice == "":
		return nil, invalidMedia("Amazon Polly speech names a Polly voice ID, such as Joanna.")
	}
	endpoint, err := connectors.PollyEndpoint(cfg.CloudRegion)
	if err != nil {
		return nil, invalidMedia(err.Error())
	}
	body, err := json.Marshal(map[string]string{"Text": p.Input, "VoiceId": p.Voice, "Engine": model, "OutputFormat": format})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, URL: endpoint + "/v1/speech", SigningService: "polly", JSON: body, Kind: ResponseBinary, Ambiguous: true,
		CharacterHeader: "X-Amzn-Requestcharacters"}, nil
}

const assemblyAIPoll = time.Second

func encodeAssemblyAI(r *Request, p Transcription, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	format, failure := vendorTranscriptFormat(r, p, "AssemblyAI", "json", "verbose_json", "text")
	if failure != nil {
		return nil, failure
	}
	if p.Prompt != nil {
		return nil, invalidMedia("AssemblyAI transcription takes no prompt.")
	}
	base := strings.TrimRight(cfg.Endpoint, "/")
	submission := map[string]any{"speech_models": []string{model}}
	if p.Language != nil {
		submission["language_code"] = *p.Language
	}
	var transcript string
	next := func(body []byte) (*Step, *Error) {
		var encodedWire struct {
			UploadURL string `json:"upload_url"`
			ID        string `json:"id"`
			Status    string `json:"status"`
		}
		if json.Unmarshal(body, &encodedWire) != nil {
			return nil, protocolError("The provider's transcription answered with invalid JSON.")
		}
		switch {
		case encodedWire.UploadURL != "" && encodedWire.Status == "":
			submission["audio_url"] = encodedWire.UploadURL
			submit, err := json.Marshal(submission)
			if err != nil {
				return nil, invalidMedia("The transcription request could not be encoded.")
			}
			return &Step{Method: http.MethodPost, URL: base + "/v2/transcript", JSON: submit, Credentials: true}, nil
		case encodedWire.ID == "" || strings.ContainsAny(encodedWire.ID, "/?#"):
			return nil, protocolError("The provider's transcript names no identity.")
		case encodedWire.Status == "completed":
			return &Step{Done: true, Cleanup: &Step{Method: http.MethodDelete, URL: base + "/v2/transcript/" + url.PathEscape(encodedWire.ID), Credentials: true}}, nil
		case encodedWire.Status == "error":
			return nil, Fail(http.StatusBadRequest, "transcription_failed", "The provider could not transcribe the audio.")
		case transcript == "":
			transcript = base + "/v2/transcript/" + url.PathEscape(encodedWire.ID)
			return &Step{URL: transcript, Wait: assemblyAIPoll, Credentials: true}, nil
		}
		return nil, nil
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "v2/upload", Accept: "application/json", Upload: &p.File, Kind: ResponseTranscription, Ambiguous: true,
		Next: next, StepDomains: []string{"assemblyai.com"},
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
			var encodedWire struct {
				Text     *string  `json:"text"`
				Language *string  `json:"language_code"`
				Duration *float64 `json:"audio_duration"`
				Words    []struct {
					Text  string  `json:"text"`
					Start float64 `json:"start"`
					End   float64 `json:"end"`
				} `json:"words"`
			}
			if json.Unmarshal(body, &encodedWire) != nil || encodedWire.Text == nil {
				return nil, protocolError("The provider transcript is malformed.")
			}
			words := []transcriptWord{}
			for _, word := range encodedWire.Words {
				words = append(words, transcriptWord{Word: word.Text, Start: word.Start / 1000, End: word.End / 1000})
			}
			return vendorTranscript(format, *encodedWire.Text, encodedWire.Language, encodedWire.Duration, words), nil
		}}, nil
}
