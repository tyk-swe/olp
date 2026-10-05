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
)

// The gemini wire is Gemini's generateContent, on the Gemini API and on
// Vertex: an image model answers a prompt with an image, a speech model reads
// text aloud, and a model hears uploaded audio and writes its transcript.
// Vertex's Imagen models keep their predict API.
//
// https://ai.google.dev/gemini-api/docs/image-generation
// https://ai.google.dev/gemini-api/docs/speech-generation
// https://ai.google.dev/gemini-api/docs/audio
func init() {
	registerCodec("gemini", codec{encode: map[string]func(*Request, string) (*UpstreamCall, *Error){
		OpImageGeneration: encodeGeminiImage,
		OpSpeech:          encodeGeminiSpeech,
		OpTranscription:   encodeGeminiTranscription,
	}})
}

// geminiTranscriptPrompt asks a model for the transcript of the audio it hears.
const geminiTranscriptPrompt = "Generate a transcript of the speech in this audio. Answer with the transcript only."

func generateContent(model string) string {
	return "models/" + url.PathEscape(model) + ":generateContent"
}

func encodeGeminiImage(r *Request, model string) (*UpstreamCall, *Error) {
	if strings.HasPrefix(model, "imagen-") {
		return encodeVertexImage(r, model)
	}
	if failure := refuseImageControls(r, "Gemini", "b64_json"); failure != nil {
		return nil, failure
	}
	if r.Count != nil && *r.Count != 1 || r.OutputFormat != nil {
		return nil, invalidMedia("Gemini image models generate one image per request in their own format.")
	}
	ratio, failure := imageAspectRatio(r, "Gemini")
	if failure != nil {
		return nil, failure
	}
	config := map[string]any{"responseModalities": []string{"IMAGE"}}
	if ratio != "" {
		config["imageConfig"] = map[string]string{"aspectRatio": ratio}
	}
	body, err := json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": r.Prompt}}}}, "generationConfig": config})
	if err != nil {
		return nil, invalidMedia("The image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), JSON: body, Kind: ResponseImages, Ambiguous: true, DecodeImages: decodeGeminiImage}, nil
}

// geminiResult is a generateContent result's first candidate and usage.
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
		Prompt     int64 `json:"promptTokenCount"`
		Candidates int64 `json:"candidatesTokenCount"`
		Total      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// readGemini reads a result, refusing one Gemini's safety filters withheld.
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

func (r *geminiResult) tokens() *ImageUsage {
	if r.Usage == nil {
		return nil
	}
	return &ImageUsage{InputTokens: r.Usage.Prompt, OutputTokens: r.Usage.Candidates, TotalTokens: r.Usage.Total}
}

func decodeGeminiImage(body []byte, stage func(string, int) (*Artifact, *Error)) (*ImageResult, *Error) {
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
			return &ImageResult{CreatedAt: nowUnix(), Images: []ImageArtifact{{Handle: &handle}}, Usage: result.tokens()}, nil
		}
	}
	return nil, Fail(http.StatusBadRequest, "no_image", "The model answered without an image.")
}

func encodeGeminiSpeech(r *Request, model string) (*UpstreamCall, *Error) {
	if failure := refuseSpeechControls(r, "Gemini"); failure != nil {
		return nil, failure
	}
	format := speechFormat(r)
	if format != "wav" && format != "pcm" || r.Speed != nil {
		return nil, invalidMedia("Gemini speech is served as wav or pcm, at its natural pace.")
	}
	if r.Voice == "" {
		return nil, invalidMedia("Gemini speech names one of its prebuilt voices, such as Kore.")
	}
	body, err := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": r.Input}}}},
		"generationConfig": map[string]any{"responseModalities": []string{"AUDIO"}, "speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]string{"voiceName": r.Voice}}}},
	})
	if err != nil {
		return nil, invalidMedia("The speech request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), JSON: body, Kind: ResponseBinary, Ambiguous: true,
		DecodeAudio: func(body []byte) (*AudioResult, *Error) { return decodeGeminiSpeech(body, format) }}, nil
}

// decodeGeminiSpeech reads the audio as the format the client asked for:
// Gemini answers with WAV, or with raw 16-bit PCM whose MIME type names its
// rate, and either becomes the other.
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

func encodeGeminiTranscription(r *Request, model string) (*UpstreamCall, *Error) {
	format := "json"
	if r.Format != nil {
		format = *r.Format
	}
	switch {
	case format != "json" && format != "text":
		return nil, invalidMedia("Gemini transcripts are served as json or text.")
	case r.Stream, r.TextPrompt != nil, r.Language != nil, len(r.Include) > 0, len(r.ChunkingStrategy) > 0, len(r.KnownSpeakerNames) > 0, len(r.TimestampGranularities) > 0, len(r.Extra) > 0:
		return nil, invalidMedia("Gemini transcription does not support the requested transcription parameters.")
	case r.File == nil || !strings.HasPrefix(strings.ToLower(r.File.ContentType), "audio/"):
		return nil, invalidMedia("Gemini transcription needs the audio file's content type.")
	}
	config := map[string]any{}
	if r.Temperature != nil {
		config["temperature"] = *r.Temperature
	}
	file := r.File
	return &UpstreamCall{Method: http.MethodPost, Path: generateContent(model), Kind: ResponseTranscription, Ambiguous: true,
		JSONFrom: func(read func(*Part) ([]byte, *Error)) ([]byte, *Error) {
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
			return &TranscriptionResult{Text: strings.TrimSpace(text.String()), TextOnly: true, Tokens: result.tokens()}, nil
		}}, nil
}

func nowUnix() int64 { return time.Now().Unix() }
