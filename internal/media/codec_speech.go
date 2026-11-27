package media

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

var elevenLabsCodec = codec{speech: encodeElevenLabsSpeech, transcription: encodeElevenLabsTranscription}

var deepgramCodec = codec{speech: encodeDeepgramSpeech, transcription: encodeDeepgramTranscription}

var pollyCodec = codec{speech: encodePollySpeech}

var assemblyAICodec = codec{transcription: encodeAssemblyAI}

type elevenLabsVoiceSettings struct {
	Speed float64 `json:"speed"`
}

type elevenLabsSpeechRequest struct {
	Text          string                   `json:"text"`
	ModelID       string                   `json:"model_id"`
	VoiceSettings *elevenLabsVoiceSettings `json:"voice_settings,omitempty"`
}

type elevenLabsWord struct {
	Text  string  `json:"text"`
	Type  string  `json:"type"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type elevenLabsTranscriptResponse struct {
	Text     *string          `json:"text"`
	Language *string          `json:"language_code"`
	Duration *float64         `json:"audio_duration_secs"`
	Words    []elevenLabsWord `json:"words"`
}

type deepgramSpeechRequest struct {
	Text string `json:"text"`
}

type deepgramWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type deepgramAlternative struct {
	Transcript *string        `json:"transcript"`
	Words      []deepgramWord `json:"words"`
}

type deepgramChannel struct {
	Language     *string               `json:"detected_language"`
	Alternatives []deepgramAlternative `json:"alternatives"`
}

type deepgramMetadata struct {
	Duration *float64 `json:"duration"`
}

type deepgramResults struct {
	Channels []deepgramChannel `json:"channels"`
}

type deepgramTranscriptResponse struct {
	Metadata deepgramMetadata `json:"metadata"`
	Results  deepgramResults  `json:"results"`
}

type pollySpeechRequest struct {
	Text         string `json:"Text"`
	VoiceID      string `json:"VoiceId"`
	Engine       string `json:"Engine"`
	OutputFormat string `json:"OutputFormat"`
}

type assemblyAISubmission struct {
	SpeechModels []string `json:"speech_models"`
	LanguageCode *string  `json:"language_code,omitempty"`
	AudioURL     string   `json:"audio_url"`
}

type assemblyAIPollResponse struct {
	UploadURL string `json:"upload_url"`
	ID        string `json:"id"`
	Status    string `json:"status"`
}

type assemblyAIWord struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type assemblyAITranscriptResponse struct {
	Text     *string          `json:"text"`
	Language *string          `json:"language_code"`
	Duration *float64         `json:"audio_duration"`
	Words    []assemblyAIWord `json:"words"`
}

type transcriptWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

func refuseSpeechControls(r *Request, p Speech, vendor string) *Error {
	if p.Instructions != nil || p.StreamFormat != nil && *p.StreamFormat != "audio" || r.hasExtra() {
		return invalidMedia(vendor + " speech does not support the requested speech parameters.")
	}
	return nil
}

var elevenLabsFormats = map[string]string{"mp3": "mp3_44100_128", "opus": "opus_48000_128", "pcm": "pcm_24000", "wav": "wav_24000"}

func encodeElevenLabsSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "ElevenLabs"); failure != nil {
		return nil, failure
	}
	format, ok := elevenLabsFormats[ptr.Or(p.Format, "mp3")]
	if !ok {
		return nil, invalidMedia("ElevenLabs returns speech as mp3, opus, pcm or wav.")
	}
	if p.Voice == "" {
		return nil, invalidMedia("ElevenLabs speech names an ElevenLabs voice ID as the voice.")
	}
	var settings *elevenLabsVoiceSettings
	if p.Speed != nil {
		if *p.Speed < 0.7 || *p.Speed > 1.2 {
			return nil, invalidMedia("ElevenLabs speech speed ranges from 0.7 to 1.2.")
		}
		settings = &elevenLabsVoiceSettings{Speed: *p.Speed}
	}
	body, err := json.Marshal(elevenLabsSpeechRequest{Text: p.Input, ModelID: model, VoiceSettings: settings})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "text-to-speech/" + url.PathEscape(p.Voice), Query: url.Values{"output_format": {format}}},
		Body: JSONBody(body), Response: AudioReply{CharacterHeader: "Character-Cost"}, Ambiguous: true}, nil
}

func vendorTranscriptFormat(r *Request, p Transcription, vendor string, formats ...string) (string, *Error) {
	format := ptr.Or(p.Format, "json")
	switch last := len(formats) - 1; {
	case !slices.Contains(formats, format):
		return "", invalidMedia(vendor + " transcripts are served as " + strings.Join(formats[:last], ", ") + " or " + formats[last] + ".")
	case p.Stream, p.Temperature != nil, len(p.Include) > 0, len(p.ChunkingStrategy) > 0, len(p.KnownSpeakerNames) > 0, r.hasExtra(),
		slices.ContainsFunc(p.TimestampGranularities, func(granularity string) bool { return granularity != "word" }):
		return "", invalidMedia(vendor + " transcription does not support the requested transcription parameters.")
	}
	return format, nil
}

func vendorTranscript(format, text string, language *string, duration *float64, words []transcriptWord) (*TranscriptionResult, *Error) {
	result := &TranscriptionResult{Text: text, Language: language, DurationSeconds: duration}
	if format != "verbose_json" || words == nil {
		return result, nil
	}
	encodedWords, err := json.Marshal(words)
	if err != nil {
		return nil, ProtocolError("The provider transcript could not be encoded.")
	}
	result.Extra = map[string]json.RawMessage{"words": encodedWords}
	return result, nil
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
	return &UpstreamCall{
		Method:    http.MethodPost,
		Endpoint:  Path{Path: "speech-to-text"},
		Accept:    "application/json",
		Body:      FormBody(fields),
		Response:  TranscriptReply{Format: format, Decode: decodeElevenLabsTranscript(format)},
		Ambiguous: true,
	}, nil
}

func decodeElevenLabsTranscript(format string) func([]byte) (*TranscriptionResult, *Error) {
	return func(body []byte) (*TranscriptionResult, *Error) {
		var resp elevenLabsTranscriptResponse
		if json.Unmarshal(body, &resp) != nil || resp.Text == nil {
			return nil, ProtocolError("The provider transcript is malformed.")
		}
		var words []transcriptWord
		for _, word := range resp.Words {
			if word.Type == "word" {
				words = append(words, transcriptWord{Word: word.Text, Start: word.Start, End: word.End})
			}
		}
		return vendorTranscript(format, *resp.Text, resp.Language, resp.Duration, words)
	}
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
	encoding, ok := deepgramEncodings[ptr.Or(p.Format, "mp3")]
	if !ok {
		return nil, invalidMedia("Deepgram does not produce the requested speech format.")
	}
	query := url.Values{"model": {model}}
	for name, values := range encoding {
		query[name] = slices.Clone(values)
	}
	body, err := json.Marshal(deepgramSpeechRequest{Text: p.Input})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "speak", Query: query}, Body: JSONBody(body),
		Response: AudioReply{CharacterHeader: "Dg-Char-Count"}, Ambiguous: true}, nil
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
	return &UpstreamCall{
		Method:    http.MethodPost,
		Endpoint:  Path{Path: "listen", Query: query},
		Accept:    "application/json",
		Body:      UploadBody(p.File),
		Response:  TranscriptReply{Format: format, Decode: decodeDeepgramTranscript(format, p.Language)},
		Ambiguous: true,
	}, nil
}

func decodeDeepgramTranscript(format string, fallbackLanguage *string) func([]byte) (*TranscriptionResult, *Error) {
	return func(body []byte) (*TranscriptionResult, *Error) {
		var resp deepgramTranscriptResponse
		if json.Unmarshal(body, &resp) != nil || len(resp.Results.Channels) == 0 ||
			len(resp.Results.Channels[0].Alternatives) == 0 || resp.Results.Channels[0].Alternatives[0].Transcript == nil {
			return nil, ProtocolError("The provider transcript is malformed.")
		}
		channel := resp.Results.Channels[0]
		best := channel.Alternatives[0]
		words := make([]transcriptWord, len(best.Words))
		for i, word := range best.Words {
			words[i] = transcriptWord{Word: word.Word, Start: word.Start, End: word.End}
		}
		language := channel.Language
		if language == nil {
			language = fallbackLanguage
		}
		return vendorTranscript(format, *best.Transcript, language, resp.Metadata.Duration, words)
	}
}

var pollyFormats = map[string]string{"mp3": "mp3", "opus": "ogg_opus"}

func encodePollySpeech(r *Request, p Speech, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "Amazon Polly"); failure != nil {
		return nil, failure
	}
	format, ok := pollyFormats[ptr.Or(p.Format, "mp3")]
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
	body, err := json.Marshal(pollySpeechRequest{Text: p.Input, VoiceID: p.Voice, Engine: model, OutputFormat: format})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Absolute{URL: endpoint + "/v1/speech", SigningService: "polly"}, Body: JSONBody(body),
		Response: AudioReply{CharacterHeader: "X-Amzn-Requestcharacters"}, Ambiguous: true}, nil
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
	return &UpstreamCall{
		Method:   http.MethodPost,
		Endpoint: Path{Path: "v2/upload"},
		Accept:   "application/json",
		Body:     UploadBody(p.File),
		Response: TranscriptReply{
			Format: format,
			Poll:   &Poll{Next: assemblyAINextStep(base, model, p.Language), Domains: []string{"assemblyai.com"}},
			Decode: decodeAssemblyAITranscript(format),
		},
		Ambiguous: true,
	}, nil
}

func assemblyAINextStep(base, model string, language *string) func([]byte) (Step, *Error) {
	return func(body []byte) (Step, *Error) {
		var resp assemblyAIPollResponse
		if json.Unmarshal(body, &resp) != nil {
			return nil, ProtocolError("The provider's transcription answered with invalid JSON.")
		}
		switch {
		case resp.UploadURL != "" && resp.Status == "":
			submit, err := json.Marshal(assemblyAISubmission{SpeechModels: []string{model}, LanguageCode: language, AudioURL: resp.UploadURL})
			if err != nil {
				return nil, invalidMedia("The transcription request could not be encoded.")
			}
			return Visit{Method: http.MethodPost, URL: base + "/v2/transcript", JSON: submit, Credentials: true}, nil
		case resp.ID == "" || strings.ContainsAny(resp.ID, "/?#"):
			return nil, ProtocolError("The provider's transcript names no identity.")
		case resp.Status == "completed":
			return Finished{Cleanup: &Visit{Method: http.MethodDelete, URL: base + "/v2/transcript/" + url.PathEscape(resp.ID), Credentials: true}}, nil
		case resp.Status == "error":
			return nil, Fail(http.StatusBadRequest, "transcription_failed", "The provider could not transcribe the audio.")
		}
		return Visit{URL: base + "/v2/transcript/" + url.PathEscape(resp.ID), Wait: assemblyAIPoll, Credentials: true}, nil
	}
}

func decodeAssemblyAITranscript(format string) func([]byte) (*TranscriptionResult, *Error) {
	return func(body []byte) (*TranscriptionResult, *Error) {
		var resp assemblyAITranscriptResponse
		if json.Unmarshal(body, &resp) != nil || resp.Text == nil {
			return nil, ProtocolError("The provider transcript is malformed.")
		}
		words := make([]transcriptWord, len(resp.Words))
		for i, word := range resp.Words {
			words[i] = transcriptWord{Word: word.Text, Start: word.Start / 1000, End: word.End / 1000}
		}
		return vendorTranscript(format, *resp.Text, resp.Language, resp.Duration, words)
	}
}
