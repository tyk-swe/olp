package media

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/platform/staging"
)

type ImageResult struct {
	CreatedAt  int64
	Images     []ImageArtifact
	Usage      *ImageUsage
	Extra      map[string]any
	DataExtra  []map[string]any
	UsageExtra map[string]any
}

type ImageArtifact struct {
	URL           string
	Handle        *staging.Handle
	RevisedPrompt *string
}

type ImageUsage struct {
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	AudioInputTokens  *int64
	AudioOutputTokens *int64
	ImageInputTokens  *int64
	ImageOutputTokens *int64
}

func DecodeImageResponse(body []byte, stage StageImage) (*ImageResult, *Error) {
	var encodedWire struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL           *string `json:"url"`
			B64           *string `json:"b64_json"`
			RevisedPrompt *string `json:"revised_prompt"`
		} `json:"data"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
			InputDetail  *struct {
				ImageTokens *int64 `json:"image_tokens"`
			} `json:"input_tokens_details"`
			OutputDetail *struct {
				ImageTokens *int64 `json:"image_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ProtocolError("The provider image response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, ProtocolError("The provider image response is not valid JSON.")
	}
	if len(encodedWire.Data) == 0 {
		return nil, ProtocolError("The provider image response contains no images.")
	}
	extra := diffFields(doc, "created", "data", "usage")
	result := &ImageResult{CreatedAt: encodedWire.Created, Extra: extra}
	if data, ok := doc["data"].([]any); ok {
		result.DataExtra = make([]map[string]any, len(data))
		for i, item := range data {
			if object, ok := item.(map[string]any); ok {
				result.DataExtra[i] = diffFields(object, "url", "b64_json", "revised_prompt")
			}
		}
	}
	if usageDoc, ok := doc["usage"].(map[string]any); ok {
		result.UsageExtra = diffFields(usageDoc, "input_tokens", "output_tokens", "total_tokens")
	}
	for i, image := range encodedWire.Data {
		artifact := ImageArtifact{RevisedPrompt: image.RevisedPrompt}
		switch {
		case image.URL != nil && image.B64 == nil:
			artifact.URL = *image.URL
		case image.B64 != nil && image.URL == nil:
			staged, err := stage(*image.B64, i)
			if err != nil {
				return nil, err
			}
			handle := staged.Handle
			artifact.Handle = &handle
		default:
			return nil, ProtocolError("The provider image result must carry exactly one of url or b64_json.")
		}
		result.Images = append(result.Images, artifact)
	}
	if encodedWire.Usage != nil {
		result.Usage = &ImageUsage{
			InputTokens:  encodedWire.Usage.InputTokens,
			OutputTokens: encodedWire.Usage.OutputTokens,
			TotalTokens:  encodedWire.Usage.TotalTokens,
		}
		if encodedWire.Usage.InputDetail != nil {
			result.Usage.ImageInputTokens = encodedWire.Usage.InputDetail.ImageTokens
		}
		if encodedWire.Usage.OutputDetail != nil && encodedWire.Usage.OutputDetail.ImageTokens != nil {
			result.Usage.ImageOutputTokens = encodedWire.Usage.OutputDetail.ImageTokens
		} else {
			output := encodedWire.Usage.OutputTokens
			result.Usage.ImageOutputTokens = &output
		}
	}
	return result, nil
}

type nativeImages func(body []byte, stage StageImage) (*ImageResult, *Error)

func vertexImages(expected int64) nativeImages {
	return func(body []byte, stage StageImage) (*ImageResult, *Error) {
		var encodedWire struct {
			Predictions []struct {
				BytesBase64Encoded string `json:"bytesBase64Encoded"`
				MIMEType           string `json:"mimeType"`
			} `json:"predictions"`
			Error *string `json:"error"`
		}
		if failure := decodeNativeImages(body, &encodedWire); failure != nil {
			return nil, failure
		}
		if encodedWire.Error != nil && *encodedWire.Error != "" {
			return nil, ProtocolError("The provider returned an image error.")
		}
		if encodedWire.Predictions == nil {
			return nil, ProtocolError("The provider image response has no predictions.")
		}
		encoded := make([]string, len(encodedWire.Predictions))
		for i, prediction := range encodedWire.Predictions {
			if prediction.MIMEType != "" && !strings.HasPrefix(prediction.MIMEType, "image/") {
				return nil, ProtocolError("The provider image response carries an invalid MIME type.")
			}
			encoded[i] = prediction.BytesBase64Encoded
		}
		return stageNativeImages(encoded, expected, stage)
	}
}

func bedrockImages(expected int64) nativeImages {
	return func(body []byte, stage StageImage) (*ImageResult, *Error) {
		var encodedWire struct {
			Images        []string  `json:"images"`
			Error         *string   `json:"error"`
			FinishReasons []*string `json:"finish_reasons"`
		}
		if failure := decodeNativeImages(body, &encodedWire); failure != nil {
			return nil, failure
		}
		if encodedWire.Error != nil && *encodedWire.Error != "" {
			return nil, ProtocolError("The provider returned an image error.")
		}
		for _, reason := range encodedWire.FinishReasons {
			switch {
			case reason == nil:
			case strings.HasPrefix(*reason, "Filter reason:"):
				return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
			default:
				return nil, ProtocolError("The provider reported an image inference error.")
			}
		}
		if encodedWire.Images == nil {
			return nil, ProtocolError("The provider image response has no images.")
		}
		return stageNativeImages(encodedWire.Images, expected, stage)
	}
}

func decodeNativeImages(body []byte, target any) *Error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(target) != nil {
		return ProtocolError("The provider image response is not valid JSON.")
	}
	return nil
}

func stageNativeImages(encoded []string, expected int64, stage StageImage) (*ImageResult, *Error) {
	if int64(len(encoded)) != expected {
		return nil, ProtocolError("The provider image response did not match the requested count.")
	}
	images := make([]ImageArtifact, len(encoded))
	for i, value := range encoded {
		if value == "" {
			return nil, ProtocolError("The provider image response carries an empty payload.")
		}
		staged, failure := stage(value, i)
		if failure != nil {
			return nil, failure
		}
		images[i] = ImageArtifact{Handle: &staged.Handle}
	}
	return &ImageResult{CreatedAt: time.Now().Unix(), Images: images}, nil
}

func diffFields(doc map[string]any, known ...string) map[string]any {
	var extra map[string]any
	for name, value := range doc {
		keep := slices.Contains(known, name)
		if !keep {
			if extra == nil {
				extra = map[string]any{}
			}
			extra[name] = value
		}
	}
	return extra
}

func EncodeImageResponse(result *ImageResult) ([]byte, []string, *Error) {
	var markers []string
	data := make([]any, len(result.Images))
	for i, image := range result.Images {
		item := map[string]any{}
		switch {
		case image.URL != "":
			item["url"] = image.URL
		case image.Handle != nil:
			marker := "__olp_streamed_media_" + rand.Text() + "__"
			item["b64_json"] = marker
			markers = append(markers, marker)
		default:
			return nil, nil, ProtocolError("The provider image result carries no payload.")
		}
		if image.RevisedPrompt != nil {
			item["revised_prompt"] = *image.RevisedPrompt
		}
		if i < len(result.DataExtra) {
			for name, value := range result.DataExtra[i] {
				if _, exists := item[name]; !exists {
					item[name] = value
				}
			}
		}
		data[i] = item
	}
	doc := map[string]any{"created": result.CreatedAt, "data": data}
	if result.Usage != nil {
		usage := map[string]any{
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.TotalTokens,
		}
		for name, value := range result.UsageExtra {
			if _, exists := usage[name]; !exists {
				usage[name] = value
			}
		}
		doc["usage"] = usage
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, ProtocolError("The provider image metadata could not be encoded.")
	}
	return body, markers, nil
}

type MediaUsage struct {
	Input, Output, Cached   *int64
	AudioInput, AudioOutput *int64
	ImageInput, ImageOutput *int64
}

func ObserveMediaUsage(data string, op Operation) *MediaUsage {
	var frame struct {
		Usage *struct {
			InputTokens       *int64 `json:"input_tokens"`
			PromptTokens      *int64 `json:"prompt_tokens"`
			OutputTokens      *int64 `json:"output_tokens"`
			CompletionTokens  *int64 `json:"completion_tokens"`
			InputTokenDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
				AudioTokens  *int64 `json:"audio_tokens"`
				ImageTokens  *int64 `json:"image_tokens"`
			} `json:"input_tokens_details"`
			InputTokenDetail *struct {
				AudioTokens *int64 `json:"audio_tokens"`
				ImageTokens *int64 `json:"image_tokens"`
			} `json:"input_token_details"`
			OutputTokenDetails *struct {
				ImageTokens *int64 `json:"image_tokens"`
			} `json:"output_tokens_details"`
			PromptTokenDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil || frame.Usage == nil {
		return nil
	}
	usage := frame.Usage
	m := &MediaUsage{
		Input:  firstInt(usage.InputTokens, usage.PromptTokens),
		Output: firstInt(usage.OutputTokens, usage.CompletionTokens),
	}
	if usage.InputTokenDetails != nil && usage.InputTokenDetails.CachedTokens != nil {
		m.Cached = usage.InputTokenDetails.CachedTokens
	} else if usage.PromptTokenDetails != nil {
		m.Cached = usage.PromptTokenDetails.CachedTokens
	}
	imageDetail := func() *int64 {
		if usage.InputTokenDetails != nil && usage.InputTokenDetails.ImageTokens != nil {
			return usage.InputTokenDetails.ImageTokens
		}
		if usage.InputTokenDetail != nil {
			return usage.InputTokenDetail.ImageTokens
		}
		return nil
	}
	audioDetail := func() *int64 {
		if usage.InputTokenDetails != nil && usage.InputTokenDetails.AudioTokens != nil {
			return usage.InputTokenDetails.AudioTokens
		}
		if usage.InputTokenDetail != nil {
			return usage.InputTokenDetail.AudioTokens
		}
		return nil
	}
	switch op {
	case OpSpeech:
		m.AudioOutput = m.Output
	case OpTranscription:
		m.AudioInput = audioDetail()
	case OpImageGeneration, OpImageEdit, OpImageVariation:
		m.ImageInput = imageDetail()
		if usage.OutputTokenDetails != nil && usage.OutputTokenDetails.ImageTokens != nil {
			m.ImageOutput = usage.OutputTokenDetails.ImageTokens
		} else {
			m.ImageOutput = m.Output
		}
	}
	return m
}

func firstInt(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func IsMediaTerminal(event string) bool {
	return strings.HasSuffix(event, ".completed") || event == "transcript.text.done" ||
		event == "speech.audio.done" || event == "error" || event == "done"
}

func ProtocolError(message string) *Error {
	return Fail(502, "provider_protocol_error", message)
}

func UnixTime(value *int64) *time.Time {
	if value == nil {
		return nil
	}
	t := time.Unix(*value, 0).UTC()
	return &t
}
