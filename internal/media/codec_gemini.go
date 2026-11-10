package media

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

var geminiMediaCodec = codec{imageGeneration: encodeGeminiImage, speech: encodeGeminiSpeech, transcription: encodeGeminiTranscription}

const geminiTranscriptPrompt = "Generate a transcript of the speech in this audio. Answer with the transcript only."

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
	config := map[string]any{"responseModalities": []string{"IMAGE"}}
	if ratio != "" {
		config["imageConfig"] = map[string]string{"aspectRatio": ratio}
	}
	body, err := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": p.Prompt}}}}, "generationConfig": config})
	if err != nil {
		return nil, invalidMedia("The image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), JSON: body, Kind: ResponseImages, Ambiguous: true, DecodeImages: decodeGeminiImage}, nil
}

type geminiResult struct {
	Candidates []struct {
		FinishReason string `json:"finishReason"`
		Content      struct {
			Parts []struct {
				Text       *string `json:"text"`
				Thought    bool    `json:"thought"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Usage *struct {
		Prompt           int64                 `json:"promptTokenCount"`
		Candidates       int64                 `json:"candidatesTokenCount"`
		Total            int64                 `json:"totalTokenCount"`
		PromptDetails    []geminiModalityCount `json:"promptTokensDetails"`
		CacheDetails     []geminiModalityCount `json:"cacheTokensDetails"`
		CandidateDetails []geminiModalityCount `json:"candidatesTokensDetails"`
	} `json:"usageMetadata"`
}

func readGemini(body []byte) (*geminiResult, *Error) {
	var result geminiResult
	if json.Unmarshal(body, &result) != nil {
		return nil, protocolError("The provider result is not valid JSON.")
	}
	if result.PromptFeedback != nil && result.PromptFeedback.BlockReason != "" {
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the request.")
	}
	if len(result.Candidates) == 0 {
		return nil, protocolError("The provider result has no candidate.")
	}
	switch reason := result.Candidates[0].FinishReason; {
	case reason == "SAFETY" || strings.HasPrefix(reason, "IMAGE_") || reason == "PROHIBITED_CONTENT" || reason == "BLOCKLIST" || reason == "RECITATION":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter withheld the result.")
	}
	return &result, nil
}

type geminiModalityCount struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
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

func decodeGeminiImage(body []byte, stage func(string, int) (*staging.Artifact, *Error)) (*ImageResult, *Error) {
	result, failure := readGemini(body)
	if failure != nil {
		return nil, failure
	}
	for _, part := range result.Candidates[0].Content.Parts {
		if part.InlineData != nil && strings.HasPrefix(part.InlineData.MimeType, "image/") && !part.Thought {
			staged, failure := stage(part.InlineData.Data, 0)
			if failure != nil {
				return nil, failure
			}
			handle := staged.Handle
			return &ImageResult{CreatedAt: time.Now().Unix(), Images: []ImageArtifact{{Handle: &handle}}, Usage: result.tokens()}, nil
		}
	}
	return nil, Fail(http.StatusBadRequest, "no_image", "The model answered without an image.")
}

func encodeGeminiSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, p, "Gemini"); failure != nil {
		return nil, failure
	}
	format := valueOr(p.Format, "mp3")
	if format != "wav" && format != "pcm" || p.Speed != nil {
		return nil, invalidMedia("Gemini speech is served as wav or pcm, at its natural pace.")
	}
	if p.Voice == "" {
		return nil, invalidMedia("Gemini speech names one of its prebuilt voices, such as Kore.")
	}
	body, err := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": p.Input}}}},
		"generationConfig": map[string]any{"responseModalities": []string{"AUDIO"}, "speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]string{"voiceName": p.Voice}}}},
	})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), JSON: body, Kind: ResponseBinary, Ambiguous: true,
		DecodeAudio: func(body []byte) (*AudioResult, *Error) { return decodeGeminiSpeech(body, format) }}, nil
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
		audio, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			return nil, protocolError("The provider audio is not valid base64.")
		}
		mediaType, params, err := mime.ParseMediaType(part.InlineData.MimeType)
		if err != nil {
			return nil, protocolError("The provider audio names an invalid type.")
		}
		wav := mediaType == "audio/wav" || mediaType == "audio/x-wav"
		switch {
		case wav && format == "pcm":
			if len(audio) < 44 || string(audio[:4]) != "RIFF" {
				return nil, protocolError("The provider WAV audio is malformed.")
			}
			audio = audio[44:]
		case !wav && format == "wav":
			rate, err := strconv.Atoi(params["rate"])
			if !strings.EqualFold(mediaType, "audio/l16") || err != nil || rate < 8000 {
				return nil, protocolError("The provider audio is not 16-bit PCM of a known rate.")
			}
			audio = wavFromPCM(audio, rate)
		case !wav && !strings.EqualFold(mediaType, "audio/l16"):
			return nil, protocolError("The provider audio is neither WAV nor PCM.")
		}
		contentType := "audio/wav"
		if format == "pcm" {
			contentType = "audio/pcm"
		}
		return &AudioResult{Audio: audio, ContentType: contentType, Tokens: result.tokens()}, nil
	}
	return nil, protocolError("The model answered without audio.")
}

func encodeGeminiTranscription(r *Request, p Transcription, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	format := valueOr(p.Format, "json")
	switch {
	case format != "json" && format != "text":
		return nil, invalidMedia("Gemini transcripts are served as json or text.")
	case p.Stream, p.Prompt != nil, p.Language != nil, len(p.Include) > 0, len(p.ChunkingStrategy) > 0, len(p.KnownSpeakerNames) > 0, len(p.TimestampGranularities) > 0, len(r.Extra()) > 0:
		return nil, invalidMedia("Gemini transcription does not support the requested transcription parameters.")
	case !strings.HasPrefix(strings.ToLower(p.File.ContentType), "audio/"):
		return nil, invalidMedia("Gemini transcription needs the audio file's content type.")
	}
	config := map[string]any{}
	if p.Temperature != nil {
		config["temperature"] = *p.Temperature
	}
	file := &p.File
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), Kind: ResponseTranscription, Ambiguous: true,
		JSONFrom: func(read func(*staging.Part) ([]byte, *Error)) ([]byte, *Error) {
			audio, failure := read(file)
			if failure != nil {
				return nil, failure
			}
			body, err := json.Marshal(map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{
					map[string]any{"inlineData": map[string]string{"mimeType": file.ContentType, "data": base64.StdEncoding.EncodeToString(audio)}},
					map[string]string{"text": geminiTranscriptPrompt},
				}}},
				"generationConfig": config,
			})
			if err != nil {
				return nil, invalidMedia("The transcription request could not be encoded.")
			}
			return body, nil
		},
		DecodeTranscription: func(body []byte) (*TranscriptionResult, *Error) {
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
		}}, nil
}
