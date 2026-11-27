package media

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/ptr"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

var geminiMediaCodec = codec{imageGeneration: encodeGeminiImage, speech: encodeGeminiSpeech, transcription: encodeGeminiTranscription}

const geminiTranscriptPrompt = "Generate a transcript of the speech in this audio. Answer with the transcript only."

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiRequestPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiRequestContent struct {
	Role  string              `json:"role"`
	Parts []geminiRequestPart `json:"parts"`
}

type geminiImageConfig struct {
	AspectRatio string `json:"aspectRatio"`
}

type geminiPrebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

type geminiVoiceConfig struct {
	PrebuiltVoiceConfig geminiPrebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type geminiSpeechConfig struct {
	VoiceConfig geminiVoiceConfig `json:"voiceConfig"`
}

type geminiGenerationConfig struct {
	ResponseModalities []string            `json:"responseModalities,omitempty"`
	ImageConfig        *geminiImageConfig  `json:"imageConfig,omitempty"`
	SpeechConfig       *geminiSpeechConfig `json:"speechConfig,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
}

type geminiGenerateRequest struct {
	Contents         []geminiRequestContent `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}

type geminiResponsePart struct {
	Text       *string           `json:"text"`
	Thought    bool              `json:"thought"`
	InlineData *geminiInlineData `json:"inlineData"`
}

type geminiResponseContent struct {
	Parts []geminiResponsePart `json:"parts"`
}

type geminiCandidate struct {
	FinishReason string                `json:"finishReason"`
	Content      geminiResponseContent `json:"content"`
}

type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
}

type geminiModalityCount struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

type geminiUsageMetadata struct {
	Prompt           int64                 `json:"promptTokenCount"`
	Candidates       int64                 `json:"candidatesTokenCount"`
	Total            int64                 `json:"totalTokenCount"`
	PromptDetails    []geminiModalityCount `json:"promptTokensDetails"`
	CacheDetails     []geminiModalityCount `json:"cacheTokensDetails"`
	CandidateDetails []geminiModalityCount `json:"candidatesTokensDetails"`
}

type geminiResult struct {
	Candidates     []geminiCandidate     `json:"candidates"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback"`
	Usage          *geminiUsageMetadata  `json:"usageMetadata"`
}

func generateContent(model string) string {
	return "models/" + url.PathEscape(model) + ":generateContent"
}

func encodeGeminiImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if strings.HasPrefix(model, "imagen-") {
		return encodeVertexImage(r, p, model)
	}
	if failure := generationView(r, p).refuse("Gemini", "b64_json"); failure != nil {
		return nil, failure
	}
	if imageCount(p.Count) != 1 || p.OutputFormat != nil {
		return nil, invalidMedia("Gemini image models generate one image per request in their own format.")
	}
	ratio, failure := imageAspectRatio(p.Size, "Gemini")
	if failure != nil {
		return nil, failure
	}
	var imageConfig *geminiImageConfig
	if ratio != "" {
		imageConfig = &geminiImageConfig{AspectRatio: ratio}
	}
	body, err := json.Marshal(geminiGenerateRequest{
		Contents: []geminiRequestContent{{Role: "user", Parts: []geminiRequestPart{{Text: p.Prompt}}}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"IMAGE"},
			ImageConfig:        imageConfig,
		},
	})
	if err != nil {
		return nil, invalidMedia("The image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: generateContent(model)}, Body: JSONBody(body),
		Response: ImageReply{Decode: decodeGeminiImage}, Ambiguous: true}, nil
}

func readGemini(body []byte) (*geminiResult, *Error) {
	var result geminiResult
	if json.Unmarshal(body, &result) != nil {
		return nil, ProtocolError("The provider result is not valid JSON.")
	}
	if result.PromptFeedback != nil && result.PromptFeedback.BlockReason != "" {
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the request.")
	}
	if len(result.Candidates) == 0 {
		return nil, ProtocolError("The provider result has no candidate.")
	}
	switch reason := result.Candidates[0].FinishReason; {
	case reason == "SAFETY" || strings.HasPrefix(reason, "IMAGE_") || reason == "PROHIBITED_CONTENT" || reason == "BLOCKLIST" || reason == "RECITATION":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter withheld the result.")
	}
	return &result, nil
}

func (r *geminiResult) tokens() *ImageUsage {
	if r.Usage == nil {
		return nil
	}
	usage := &ImageUsage{InputTokens: r.Usage.Prompt, OutputTokens: r.Usage.Candidates, TotalTokens: r.Usage.Total}
	if audio, image := modalityCounts(r.Usage.PromptDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioInputTokens = &audio
		}
		if image > 0 {
			usage.ImageInputTokens = &image
		}
	}
	if audio, image := modalityCounts(r.Usage.CandidateDetails); audio > 0 || image > 0 {
		if audio > 0 {
			usage.AudioOutputTokens = &audio
		}
		if image > 0 {
			usage.ImageOutputTokens = &image
		}
	}
	return usage
}

func modalityCounts(detail []geminiModalityCount) (audio, image int64) {
	for _, entry := range detail {
		switch entry.Modality {
		case "AUDIO":
			audio += entry.TokenCount
		case "IMAGE", "VIDEO":
			image += entry.TokenCount
		}
	}
	return audio, image
}

func decodeGeminiImage(body []byte, stage StageImage) (*ImageResult, *Error) {
	result, failure := readGemini(body)
	if failure != nil {
		return nil, failure
	}
	for _, part := range result.Candidates[0].Content.Parts {
		if part.InlineData == nil || !strings.HasPrefix(part.InlineData.MimeType, "image/") || part.Thought {
			continue
		}
		staged, failure := stage(part.InlineData.Data, 0)
		if failure != nil {
			return nil, failure
		}
		handle := staged.Handle
		return &ImageResult{CreatedAt: nowUnix(), Images: []ImageArtifact{{Handle: &handle}}, Usage: result.tokens()}, nil
	}
	return nil, Fail(http.StatusBadRequest, "no_image", "The model answered without an image.")
}

func encodeGeminiSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "Gemini"); failure != nil {
		return nil, failure
	}
	format := ptr.Or(p.Format, "mp3")
	if format != "wav" && format != "pcm" || p.Speed != nil {
		return nil, invalidMedia("Gemini speech is served as wav or pcm, at its natural pace.")
	}
	if p.Voice == "" {
		return nil, invalidMedia("Gemini speech names one of its prebuilt voices, such as Kore.")
	}
	body, err := json.Marshal(geminiGenerateRequest{
		Contents: []geminiRequestContent{{Role: "user", Parts: []geminiRequestPart{{Text: p.Input}}}},
		GenerationConfig: geminiGenerationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig: &geminiSpeechConfig{
				VoiceConfig: geminiVoiceConfig{
					PrebuiltVoiceConfig: geminiPrebuiltVoiceConfig{VoiceName: p.Voice},
				},
			},
		},
	})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: generateContent(model)}, Body: JSONBody(body), Ambiguous: true,
		Response: InlineAudioReply{Decode: func(body []byte) (*AudioResult, *Error) { return decodeGeminiSpeech(body, format) }}}, nil
}

func decodeGeminiSpeech(body []byte, format string) (*AudioResult, *Error) {
	result, failure := readGemini(body)
	if failure != nil {
		return nil, failure
	}
	for _, part := range result.Candidates[0].Content.Parts {
		if part.InlineData == nil || !strings.HasPrefix(strings.ToLower(part.InlineData.MimeType), "audio/") {
			continue
		}
		return decodeGeminiAudioPart(part.InlineData, format, result.tokens())
	}
	return nil, ProtocolError("The model answered without audio.")
}

func decodeGeminiAudioPart(inline *geminiInlineData, format string, usage *ImageUsage) (*AudioResult, *Error) {
	audio, err := base64.StdEncoding.DecodeString(inline.Data)
	if err != nil {
		return nil, ProtocolError("The provider audio is not valid base64.")
	}
	mediaType, params, err := mime.ParseMediaType(inline.MimeType)
	if err != nil {
		return nil, ProtocolError("The provider audio names an invalid type.")
	}
	wav := mediaType == "audio/wav" || mediaType == "audio/x-wav"
	switch {
	case wav && format == "pcm":
		if len(audio) < 44 || string(audio[:4]) != "RIFF" {
			return nil, ProtocolError("The provider WAV audio is malformed.")
		}
		audio = audio[44:]
	case !wav && format == "wav":
		rate, err := strconv.Atoi(params["rate"])
		if !strings.EqualFold(mediaType, "audio/l16") || err != nil || rate < 8000 {
			return nil, ProtocolError("The provider audio is not 16-bit PCM of a known rate.")
		}
		audio = wavFromPCM(audio, rate)
	case !wav && !strings.EqualFold(mediaType, "audio/l16"):
		return nil, ProtocolError("The provider audio is neither WAV nor PCM.")
	}
	contentType := "audio/wav"
	if format == "pcm" {
		contentType = "audio/pcm"
	}
	return &AudioResult{Audio: audio, ContentType: contentType, Tokens: usage}, nil
}

func encodeGeminiTranscription(r *Request, p Transcription, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	format := ptr.Or(p.Format, "json")
	switch {
	case format != "json" && format != "text":
		return nil, invalidMedia("Gemini transcripts are served as json or text.")
	case p.Stream, p.Prompt != nil, p.Language != nil, len(p.Include) > 0, len(p.ChunkingStrategy) > 0, len(p.KnownSpeakerNames) > 0, len(p.TimestampGranularities) > 0, r.hasExtra():
		return nil, invalidMedia("Gemini transcription does not support the requested transcription parameters.")
	case !strings.HasPrefix(strings.ToLower(p.File.ContentType), "audio/"):
		return nil, invalidMedia("Gemini transcription needs the audio file's content type.")
	}
	return &UpstreamCall{
		Method:    http.MethodPost,
		Endpoint:  Path{Path: generateContent(model)},
		Body:      geminiTranscriptionBody(p.File, p.Temperature),
		Response:  TranscriptReply{Format: format, Decode: decodeGeminiTranscription},
		Ambiguous: true,
	}, nil
}

func geminiTranscriptionBody(file staging.Part, temperature *float64) InlineBody {
	return func(read func(staging.Part) ([]byte, *Error)) ([]byte, *Error) {
		audio, failure := read(file)
		if failure != nil {
			return nil, failure
		}
		body, err := json.Marshal(geminiGenerateRequest{
			Contents: []geminiRequestContent{{
				Role: "user",
				Parts: []geminiRequestPart{
					{InlineData: &geminiInlineData{MimeType: file.ContentType, Data: base64.StdEncoding.EncodeToString(audio)}},
					{Text: geminiTranscriptPrompt},
				},
			}},
			GenerationConfig: geminiGenerationConfig{Temperature: temperature},
		})
		if err != nil {
			return nil, invalidMedia("The transcription request could not be encoded.")
		}
		return body, nil
	}
}

func decodeGeminiTranscription(body []byte) (*TranscriptionResult, *Error) {
	result, failure := readGemini(body)
	if failure != nil {
		return nil, failure
	}
	var text strings.Builder
	for _, part := range result.Candidates[0].Content.Parts {
		if part.Text != nil && !part.Thought {
			text.WriteString(*part.Text)
		}
	}
	return &TranscriptionResult{Text: strings.TrimSpace(text.String()), Tokens: result.tokens()}, nil
}
