package media

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/platform/staging"
)

type ImageResult struct {
	CreatedAt  int64
	Images     []ImageArtifact
	Usage      *ImageUsage
	Extra      map[string]json.RawMessage
	DataExtra  []map[string]json.RawMessage
	UsageExtra map[string]json.RawMessage
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

type imageTokenDetailWire struct {
	ImageTokens *int64 `json:"image_tokens"`
}

type imageResponseItemWire struct {
	URL           string  `json:"url,omitempty"`
	B64JSON       string  `json:"b64_json,omitempty"`
	RevisedPrompt *string `json:"revised_prompt,omitempty"`
}

type imageResponseUsageWire struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type imageResponseWire struct {
	Created int64             `json:"created"`
	Data    []json.RawMessage `json:"data"`
	Usage   json.RawMessage   `json:"usage,omitempty"`
}

func DecodeImageResponse(body []byte, stage StageImage) (*ImageResult, *Error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return nil, ProtocolError("The provider image response is not valid JSON.")
	}
	var created int64
	var rawData []json.RawMessage
	if firstErr(decodeField(raw, "created", &created), decodeField(raw, "data", &rawData)) != nil {
		return nil, ProtocolError("The provider image response is not valid JSON.")
	}
	if len(rawData) == 0 {
		return nil, ProtocolError("The provider image response contains no images.")
	}
	images := make([]ImageArtifact, len(rawData))
	dataExtra := make([]map[string]json.RawMessage, len(rawData))
	for i, itemBytes := range rawData {
		artifact, extra, failure := decodeImageItem(itemBytes, i, stage)
		if failure != nil {
			return nil, failure
		}
		images[i] = artifact
		dataExtra[i] = extra
	}
	usage, usageExtra, failure := decodeImageUsage(raw["usage"])
	if failure != nil {
		return nil, failure
	}
	return &ImageResult{
		CreatedAt:  created,
		Images:     images,
		Usage:      usage,
		Extra:      diffRawFields(raw, "created", "data", "usage"),
		DataExtra:  dataExtra,
		UsageExtra: usageExtra,
	}, nil
}

func decodeImageItem(raw []byte, index int, stage StageImage) (ImageArtifact, map[string]json.RawMessage, *Error) {
	fields, err := parseRawObject(raw)
	if err != nil {
		return ImageArtifact{}, nil, ProtocolError("The provider image response is not valid JSON.")
	}
	var urlValue, b64Value, revisedPrompt *string
	if firstErr(
		decodeField(fields, "url", &urlValue),
		decodeField(fields, "b64_json", &b64Value),
		decodeField(fields, "revised_prompt", &revisedPrompt),
	) != nil {
		return ImageArtifact{}, nil, ProtocolError("The provider image response is not valid JSON.")
	}
	extra := diffRawFields(fields, "url", "b64_json", "revised_prompt")
	switch {
	case urlValue != nil && b64Value == nil:
		return ImageArtifact{URL: *urlValue, RevisedPrompt: revisedPrompt}, extra, nil
	case b64Value != nil && urlValue == nil:
		staged, failure := stage(*b64Value, index)
		if failure != nil {
			return ImageArtifact{}, nil, failure
		}
		handle := staged.Handle
		return ImageArtifact{Handle: &handle, RevisedPrompt: revisedPrompt}, extra, nil
	}
	return ImageArtifact{}, nil, ProtocolError("The provider image result must carry exactly one of url or b64_json.")
}

func decodeImageUsage(raw json.RawMessage) (*ImageUsage, map[string]json.RawMessage, *Error) {
	if len(raw) == 0 || isJSON(raw, "null") {
		return nil, nil, nil
	}
	fields, err := parseRawObject(raw)
	if err != nil {
		return nil, nil, ProtocolError("The provider image response is not valid JSON.")
	}
	var inputTokens, outputTokens, totalTokens int64
	var inputDetail, outputDetail *imageTokenDetailWire
	if firstErr(
		decodeField(fields, "input_tokens", &inputTokens),
		decodeField(fields, "output_tokens", &outputTokens),
		decodeField(fields, "total_tokens", &totalTokens),
		decodeField(fields, "input_tokens_details", &inputDetail),
		decodeField(fields, "output_tokens_details", &outputDetail),
	) != nil {
		return nil, nil, ProtocolError("The provider image response is not valid JSON.")
	}
	usage := &ImageUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}
	if inputDetail != nil {
		usage.ImageInputTokens = inputDetail.ImageTokens
	}
	if outputDetail != nil && outputDetail.ImageTokens != nil {
		usage.ImageOutputTokens = outputDetail.ImageTokens
	} else {
		output := outputTokens
		usage.ImageOutputTokens = &output
	}
	return usage, diffRawFields(fields, "input_tokens", "output_tokens", "total_tokens"), nil
}

type nativeImages func(body []byte, stage StageImage) (*ImageResult, *Error)

type vertexPredictionWire struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded"`
	MIMEType           string `json:"mimeType"`
}

type vertexImageResponseWire struct {
	Predictions []vertexPredictionWire `json:"predictions"`
	Error       *string                `json:"error"`
}

func vertexImages(expected int64) nativeImages {
	return func(body []byte, stage StageImage) (*ImageResult, *Error) {
		var wireResp vertexImageResponseWire
		if err := json.Unmarshal(body, &wireResp); err != nil {
			return nil, ProtocolError("The provider image response is not valid JSON.")
		}
		if wireResp.Error != nil && *wireResp.Error != "" {
			return nil, ProtocolError("The provider returned an image error.")
		}
		if wireResp.Predictions == nil {
			return nil, ProtocolError("The provider image response has no predictions.")
		}
		encoded := make([]string, len(wireResp.Predictions))
		for i, prediction := range wireResp.Predictions {
			if prediction.MIMEType != "" && !strings.HasPrefix(prediction.MIMEType, "image/") {
				return nil, ProtocolError("The provider image response carries an invalid MIME type.")
			}
			encoded[i] = prediction.BytesBase64Encoded
		}
		return stageNativeImages(encoded, expected, stage)
	}
}

type bedrockImageResponseWire struct {
	Images        []string  `json:"images"`
	Error         *string   `json:"error"`
	FinishReasons []*string `json:"finish_reasons"`
}

func bedrockImages(expected int64) nativeImages {
	return func(body []byte, stage StageImage) (*ImageResult, *Error) {
		var wireResp bedrockImageResponseWire
		if err := json.Unmarshal(body, &wireResp); err != nil {
			return nil, ProtocolError("The provider image response is not valid JSON.")
		}
		if wireResp.Error != nil && *wireResp.Error != "" {
			return nil, ProtocolError("The provider returned an image error.")
		}
		for _, reason := range wireResp.FinishReasons {
			switch {
			case reason == nil:
			case strings.HasPrefix(*reason, "Filter reason:"):
				return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
			default:
				return nil, ProtocolError("The provider reported an image inference error.")
			}
		}
		if wireResp.Images == nil {
			return nil, ProtocolError("The provider image response has no images.")
		}
		return stageNativeImages(wireResp.Images, expected, stage)
	}
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
		handle := staged.Handle
		images[i] = ImageArtifact{Handle: &handle}
	}
	return &ImageResult{CreatedAt: nowUnix(), Images: images}, nil
}

func EncodeImageResponse(result *ImageResult) ([]byte, []string, *Error) {
	var markers []string
	data := make([]json.RawMessage, len(result.Images))
	for i, image := range result.Images {
		item := imageResponseItemWire{RevisedPrompt: image.RevisedPrompt}
		switch {
		case image.URL != "":
			item.URL = image.URL
		case image.Handle != nil:
			marker := "__olp_streamed_media_" + rand.Text() + "__"
			item.B64JSON = marker
			markers = append(markers, marker)
		default:
			return nil, nil, ProtocolError("The provider image result carries no payload.")
		}
		var itemExtra map[string]json.RawMessage
		if i < len(result.DataExtra) {
			itemExtra = result.DataExtra[i]
		}
		rawItem, err := marshalWithExtra(item, itemExtra)
		if err != nil {
			return nil, nil, ProtocolError("The provider image metadata could not be encoded.")
		}
		data[i] = rawItem
	}
	var rawUsage json.RawMessage
	if result.Usage != nil {
		usageWire := imageResponseUsageWire{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			TotalTokens:  result.Usage.TotalTokens,
		}
		encodedUsage, err := marshalWithExtra(usageWire, result.UsageExtra)
		if err != nil {
			return nil, nil, ProtocolError("The provider image metadata could not be encoded.")
		}
		rawUsage = encodedUsage
	}
	body, err := marshalWithExtra(imageResponseWire{Created: result.CreatedAt, Data: data, Usage: rawUsage}, result.Extra)
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

type mediaInputDetailsWire struct {
	CachedTokens *int64 `json:"cached_tokens"`
	AudioTokens  *int64 `json:"audio_tokens"`
	ImageTokens  *int64 `json:"image_tokens"`
}

type mediaOutputDetailsWire struct {
	ImageTokens *int64 `json:"image_tokens"`
}

type mediaPromptDetailsWire struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

type mediaUsageWire struct {
	InputTokens        *int64                  `json:"input_tokens"`
	PromptTokens       *int64                  `json:"prompt_tokens"`
	OutputTokens       *int64                  `json:"output_tokens"`
	CompletionTokens   *int64                  `json:"completion_tokens"`
	InputTokenDetails  *mediaInputDetailsWire  `json:"input_tokens_details"`
	InputTokenDetail   *mediaInputDetailsWire  `json:"input_token_details"`
	OutputTokenDetails *mediaOutputDetailsWire `json:"output_tokens_details"`
	PromptTokenDetails *mediaPromptDetailsWire `json:"prompt_tokens_details"`
}

type mediaUsageFrameWire struct {
	Usage *mediaUsageWire `json:"usage"`
}

func ObserveMediaUsage(data string, op Operation) *MediaUsage {
	var frame mediaUsageFrameWire
	if err := json.Unmarshal([]byte(data), &frame); err != nil || frame.Usage == nil {
		return nil
	}
	usage := frame.Usage
	m := &MediaUsage{
		Input:  firstInt(usage.InputTokens, usage.PromptTokens),
		Output: firstInt(usage.OutputTokens, usage.CompletionTokens),
		Cached: usageCachedTokens(usage),
	}
	switch op {
	case OpSpeech:
		m.AudioOutput = m.Output
	case OpTranscription:
		m.AudioInput = usageAudioTokens(usage)
	case OpImageGeneration, OpImageEdit, OpImageVariation:
		m.ImageInput = usageImageInputTokens(usage)
		m.ImageOutput = usageImageOutputTokens(usage, m.Output)
	}
	return m
}

func usageCachedTokens(usage *mediaUsageWire) *int64 {
	if usage.InputTokenDetails != nil && usage.InputTokenDetails.CachedTokens != nil {
		return usage.InputTokenDetails.CachedTokens
	}
	if usage.PromptTokenDetails != nil {
		return usage.PromptTokenDetails.CachedTokens
	}
	return nil
}

func usageImageInputTokens(usage *mediaUsageWire) *int64 {
	if usage.InputTokenDetails != nil && usage.InputTokenDetails.ImageTokens != nil {
		return usage.InputTokenDetails.ImageTokens
	}
	if usage.InputTokenDetail != nil {
		return usage.InputTokenDetail.ImageTokens
	}
	return nil
}

func usageImageOutputTokens(usage *mediaUsageWire, fallback *int64) *int64 {
	if usage.OutputTokenDetails != nil && usage.OutputTokenDetails.ImageTokens != nil {
		return usage.OutputTokenDetails.ImageTokens
	}
	return fallback
}

func usageAudioTokens(usage *mediaUsageWire) *int64 {
	if usage.InputTokenDetails != nil && usage.InputTokenDetails.AudioTokens != nil {
		return usage.InputTokenDetails.AudioTokens
	}
	if usage.InputTokenDetail != nil {
		return usage.InputTokenDetail.AudioTokens
	}
	return nil
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
