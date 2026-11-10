package media

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
)

// ImageResult is the decoded unary image response. Base64 payloads are
// already staged in the spool and referenced by handle.
type ImageResult struct {
	CreatedAt  int64
	Images     []ImageArtifact
	Usage      *ImageUsage
	Extra      map[string]any // top-level extension fields for the client reply
	DataExtra  []map[string]any
	UsageExtra map[string]any
}

// ImageArtifact is one produced image: either an upstream URL or a staged file.
type ImageArtifact struct {
	URL           string
	Handle        *staging.Handle
	RevisedPrompt *string
}

// ImageUsage is the token usage a media response reported, such as an image
// model's, or a speech or transcription model's that bills by token.
type ImageUsage struct {
	InputTokens       int64
	OutputTokens      int64
	TotalTokens       int64
	AudioInputTokens  *int64
	AudioOutputTokens *int64
	ImageInputTokens  *int64
	ImageOutputTokens *int64
}

// DecodeImageResponse parses an upstream image document, staging each
// base64 payload through stage before the result is returned.
func DecodeImageResponse(body []byte, stage func(b64 string, index int) (*staging.Artifact, *Error)) (*ImageResult, *Error) {
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
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
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
			return nil, protocolError("The provider image result must carry exactly one of url or b64_json.")
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
			// Without an output breakdown every output token an image model
			// reports is the image it produced.
			output := encodedWire.Usage.OutputTokens
			result.Usage.ImageOutputTokens = &output
		}
	}
	return result, nil
}

func DecodeNativeImageResponse(kind string, body []byte, expected int64, stage func(b64 string, index int) (*staging.Artifact, *Error)) (*ImageResult, *Error) {
	var encodedWire struct {
		Predictions []struct {
			BytesBase64Encoded string `json:"bytesBase64Encoded"`
			MIMEType           string `json:"mimeType"`
		} `json:"predictions"`
		Images []string `json:"images"`
		Error  *string  `json:"error"`
		// FinishReasons is a Stability model's verdict on each image; any but
		// null reports a filtered request or an inference error.
		FinishReasons []*string `json:"finish_reasons"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	if encodedWire.Error != nil && *encodedWire.Error != "" {
		return nil, protocolError("The provider returned an image error.")
	}
	var encoded []string
	switch kind {
	case "vertex_ai":
		if encodedWire.Predictions == nil {
			return nil, protocolError("The provider image response has no predictions.")
		}
		for _, prediction := range encodedWire.Predictions {
			if prediction.MIMEType != "" && !strings.HasPrefix(prediction.MIMEType, "image/") {
				return nil, protocolError("The provider image response carries an invalid MIME type.")
			}
			encoded = append(encoded, prediction.BytesBase64Encoded)
		}
	case "bedrock":
		for _, reason := range encodedWire.FinishReasons {
			switch {
			case reason == nil:
			case strings.HasPrefix(*reason, "Filter reason:"):
				return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
			default:
				return nil, protocolError("The provider reported an image inference error.")
			}
		}
		if encodedWire.Images == nil {
			return nil, protocolError("The provider image response has no images.")
		}
		encoded = encodedWire.Images
	default:
		return nil, protocolError("The native image response kind is not supported.")
	}
	if int64(len(encoded)) != expected {
		return nil, protocolError("The provider image response did not match the requested count.")
	}
	result := &ImageResult{CreatedAt: time.Now().Unix()}
	for i, value := range encoded {
		if value == "" {
			return nil, protocolError("The provider image response carries an empty payload.")
		}
		staged, err := stage(value, i)
		if err != nil {
			return nil, err
		}
		handle := staged.Handle
		result.Images = append(result.Images, ImageArtifact{Handle: &handle})
	}
	return result, nil
}

// diffFields returns the object fields not listed, or nil when none exist.
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

// EncodeImageResponse renders the client JSON image document. Staged handles
// are emitted as marker strings; the caller streams base64 content in their
// place so the whole response never buffers in memory.
func EncodeImageResponse(result *ImageResult) ([]byte, []string, *Error) {
	var markers []string
	data := make([]any, len(result.Images))
	for i, image := range result.Images {
		item := map[string]any{}
		switch {
		case image.URL != "":
			item["url"] = image.URL
		case image.Handle != nil:
			marker := "__olp_streamed_media_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "") + "__"
			item["b64_json"] = marker
			markers = append(markers, marker)
		default:
			return nil, nil, protocolError("The provider image result carries no payload.")
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
		return nil, nil, protocolError("The provider image metadata could not be encoded.")
	}
	return body, markers, nil
}

// VideoJobResult is the decoded video object. Route is set by the gateway for
// client rendering — it is the public route slug, never the upstream model.
type VideoJobResult struct {
	ID           string
	Route        string
	Status       string // queued | in_progress | completed | failed | other value
	Progress     *float32
	CreatedAt    *int64
	CompletedAt  *int64
	ExpiresAt    *int64
	Prompt       *string
	Seconds      *string
	Size         *string
	RemixedFrom  *string
	ErrorCode    *string
	ErrorMessage *string
	Extra        map[string]any
}

// VideoListResult is a decoded provider list page.
type VideoListResult struct {
	Jobs    []VideoJobResult
	FirstID *string
	LastID  *string
	HasMore bool
	Extra   map[string]any
}

// VideoDeleteResult is a decoded deletion receipt.
type VideoDeleteResult struct {
	ID      string
	Deleted bool
	Object  *string
	Extra   map[string]any
}

// DecodeVideoObject parses a provider video document.
func DecodeVideoObject(body []byte) (*VideoJobResult, *Error) {
	var encodedWire struct {
		ID        string   `json:"id"`
		Object    string   `json:"object"`
		Model     string   `json:"model"`
		Status    string   `json:"status"`
		Progress  *float32 `json:"progress"`
		CreatedAt *int64   `json:"created_at"`
		Completed *int64   `json:"completed_at"`
		Expires   *int64   `json:"expires_at"`
		Prompt    *string  `json:"prompt"`
		Seconds   *string  `json:"seconds"`
		Size      *string  `json:"size"`
		Remixed   *string  `json:"remixed_from_video_id"`
		Error     *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider video response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, protocolError("The provider video response is not valid JSON.")
	}
	if encodedWire.Object != "video" {
		return nil, protocolError("The provider returned an unexpected video object type.")
	}
	result := &VideoJobResult{
		ID: encodedWire.ID, Status: encodedWire.Status, Progress: encodedWire.Progress,
		CreatedAt: encodedWire.CreatedAt, CompletedAt: encodedWire.Completed, ExpiresAt: encodedWire.Expires,
		Prompt: encodedWire.Prompt, Seconds: encodedWire.Seconds, Size: encodedWire.Size, RemixedFrom: encodedWire.Remixed,
		Extra: diffFields(doc, "id", "object", "model", "status", "progress", "created_at",
			"completed_at", "expires_at", "prompt", "seconds", "size", "remixed_from_video_id", "error"),
	}
	if encodedWire.Error != nil {
		result.ErrorCode = &encodedWire.Error.Code
		result.ErrorMessage = &encodedWire.Error.Message
	}
	return result, nil
}

// EncodeVideoObject renders the client video object. The client always sees
// the local job id and route slug, never upstream identities.
func EncodeVideoObject(result *VideoJobResult, localID, route string) ([]byte, *Error) {
	doc := map[string]any{
		"id":     localID,
		"object": "video",
		"model":  route,
		"status": result.Status,
	}
	if result.Progress != nil {
		doc["progress"] = *result.Progress
	}
	if result.CreatedAt != nil {
		doc["created_at"] = *result.CreatedAt
	}
	if result.CompletedAt != nil {
		doc["completed_at"] = *result.CompletedAt
	}
	if result.ExpiresAt != nil {
		doc["expires_at"] = *result.ExpiresAt
	}
	if result.Prompt != nil {
		doc["prompt"] = *result.Prompt
	}
	if result.Seconds != nil {
		doc["seconds"] = *result.Seconds
	}
	if result.Size != nil {
		doc["size"] = *result.Size
	}
	if result.ErrorMessage != nil {
		code := "video_error"
		if result.ErrorCode != nil {
			code = *result.ErrorCode
		}
		doc["error"] = map[string]any{"code": code, "message": *result.ErrorMessage}
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, protocolError("The provider video metadata could not be encoded.")
	}
	return body, nil
}

// EncodeStrictVideoObject keeps every native metadata member and numeric token
// intact while replacing only the provider resource ID and model binding with
// the caller's owned local job identity and route. The original source stays
// available in the encrypted job secret for recovery.
func EncodeStrictVideoObject(source oif.Document, upstreamID, localID, route string) ([]byte, *Error) {
	if !source.Valid() || source.Root().Kind() != oif.Object {
		return nil, protocolError("The native video metadata is unavailable.")
	}
	id, ok := source.Root().Lookup("id")
	actual, valid := id.Text()
	if !ok || !valid || actual != upstreamID {
		return nil, protocolError("The native video identity changed.")
	}
	localJSON, _ := json.Marshal(localID)
	changes := []oif.Change{{Pointer: "/id", Value: string(localJSON), Origin: oif.ResourceBinding, Reason: "owned local video job identity"}}
	if route != "" {
		routeJSON, _ := json.Marshal(route)
		changes = append(changes, oif.Change{Pointer: "/model", Value: string(routeJSON), Origin: oif.IdentityBinding, Reason: "published route identity"})
	}
	client, err := oif.Apply(source, changes)
	if err != nil {
		return nil, protocolError("The native video metadata could not be projected.")
	}
	return client.Bytes(), nil
}

// ValidStrictVideoJobSource guards autonomous polls, which have no gateway
// response writer or route template. They may advance a strict job only from
// an unambiguous provider object with the same pinned identity and model.
func ValidStrictVideoJobSource(source oif.Document, upstreamID, model string) bool {
	if !source.Valid() || source.Root().Kind() != oif.Object {
		return false
	}
	id, idOK := source.Root().Lookup("id")
	modelValue, modelOK := source.Root().Lookup("model")
	object, objectOK := source.Root().Lookup("object")
	status, statusOK := source.Root().Lookup("status")
	gotID, idText := id.Text()
	gotModel, modelText := modelValue.Text()
	gotObject, objectText := object.Text()
	gotStatus, statusText := status.Text()
	_, validStatus := wire.VideoJobStateOf(gotStatus)
	return idOK && idText && gotID == upstreamID && modelOK && modelText && gotModel == model &&
		objectOK && objectText && gotObject == "video" && statusOK && statusText &&
		validStatus
}

// EncodeVideoListResponse renders the client list document. Each job keeps
// its own route slug as the public model.
func EncodeVideoListResponse(result *VideoListResult, fallback string) ([]byte, *Error) {
	data := make([]any, len(result.Jobs))
	for i := range result.Jobs {
		job := result.Jobs[i]
		encoded, failure := EncodeVideoObject(&job, job.ID, modelForList(job, fallback))
		if failure != nil {
			return nil, failure
		}
		var item any
		if err := json.Unmarshal(encoded, &item); err != nil {
			return nil, protocolError("The provider video metadata could not be encoded.")
		}
		data[i] = item
	}
	doc := map[string]any{"object": "list", "data": data, "has_more": result.HasMore}
	if result.FirstID != nil {
		doc["first_id"] = *result.FirstID
	}
	if result.LastID != nil {
		doc["last_id"] = *result.LastID
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, protocolError("The provider video list could not be encoded.")
	}
	return body, nil
}

func modelForList(job VideoJobResult, fallback string) string {
	if job.Route != "" {
		return job.Route
	}
	return fallback
}

// DecodeVideoDeleteResponse parses a provider deletion receipt.
func DecodeVideoDeleteResponse(body []byte) (*VideoDeleteResult, *Error) {
	var encodedWire struct {
		ID      string  `json:"id"`
		Object  *string `json:"object"`
		Deleted bool    `json:"deleted"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider video delete response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, protocolError("The provider video delete response is not valid JSON.")
	}
	return &VideoDeleteResult{
		ID: encodedWire.ID, Deleted: encodedWire.Deleted, Object: encodedWire.Object,
		Extra: diffFields(doc, "id", "object", "deleted"),
	}, nil
}

// EncodeVideoDeleteResponse renders the client deletion receipt.
func EncodeVideoDeleteResponse(result *VideoDeleteResult, localID string) ([]byte, *Error) {
	object := "video.deleted"
	if result.Object != nil && *result.Object != "" {
		object = *result.Object
	}
	doc := map[string]any{"id": localID, "object": object, "deleted": result.Deleted}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, protocolError("The provider video deletion receipt could not be encoded.")
	}
	return body, nil
}

// TranscriptionResult is a decoded transcription.
type TranscriptionResult struct {
	// TextOnly renders the transcript as OpenAI's json format, its text
	// alone, for a client that asked for json of a vendor whose own
	// transcript says more.
	TextOnly bool
	// Tokens is the usage of a vendor that bills transcripts by token.
	Tokens          *ImageUsage
	Text            string
	Language        *string
	DurationSeconds *float64
	Segments        []TranscriptionSegment
	Extra           map[string]any
}

// TranscriptionSegment is one timed text span.
type TranscriptionSegment struct {
	// ID is kept as raw JSON: verbose_json uses integer ids and diarized_json
	// uses string ids such as "seg_001".
	ID      json.RawMessage
	Start   float64
	End     float64
	Text    string
	Speaker *string
	Extra   map[string]any
}

// TranscriptionFormat is the response format class.
func TranscriptionFormatIsText(format string) bool {
	return format == "text" || format == "srt" || format == "vtt"
}

// DecodeTranscriptionJSON parses a JSON transcription document.
func DecodeTranscriptionJSON(body []byte) (*TranscriptionResult, *Error) {
	var encodedWire struct {
		Text     string   `json:"text"`
		Language *string  `json:"language"`
		Duration *float64 `json:"duration"`
		Segments []struct {
			ID      json.RawMessage `json:"id"`
			Start   float64         `json:"start"`
			End     float64         `json:"end"`
			Text    string          `json:"text"`
			Speaker *string         `json:"speaker"`
		} `json:"segments"`
		Usage *struct {
			Type         string `json:"type"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			TotalTokens  int64  `json:"total_tokens"`
			InputDetail  *struct {
				AudioTokens *int64 `json:"audio_tokens"`
			} `json:"input_token_details"`
		} `json:"usage"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider transcription is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, protocolError("The provider transcription is not valid JSON.")
	}
	for _, segment := range encodedWire.Segments {
		if len(segment.ID) == 0 || string(segment.ID) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(segment.ID, &text); err == nil {
			continue
		}
		var number int64
		if err := json.Unmarshal(segment.ID, &number); err != nil {
			return nil, protocolError("The provider transcription segment ID must be a string or integer.")
		}
	}
	result := &TranscriptionResult{
		Text: encodedWire.Text, Language: encodedWire.Language, DurationSeconds: encodedWire.Duration,
		Extra: diffFields(doc, "text", "language", "duration", "segments"),
	}
	if encodedWire.Usage != nil && (encodedWire.Usage.Type == "" || encodedWire.Usage.Type == "tokens") {
		result.Tokens = &ImageUsage{
			InputTokens: encodedWire.Usage.InputTokens, OutputTokens: encodedWire.Usage.OutputTokens,
			TotalTokens: encodedWire.Usage.TotalTokens,
		}
		if encodedWire.Usage.InputDetail != nil {
			result.Tokens.AudioInputTokens = encodedWire.Usage.InputDetail.AudioTokens
		}
	}
	if segments, ok := doc["segments"].([]any); ok && len(segments) == len(encodedWire.Segments) {
		for i, item := range segments {
			extra := map[string]any(nil)
			if object, ok := item.(map[string]any); ok {
				extra = diffFields(object, "id", "start", "end", "text", "speaker")
			}
			segment := encodedWire.Segments[i]
			result.Segments = append(result.Segments, TranscriptionSegment{
				ID: segment.ID, Start: segment.Start, End: segment.End,
				Text: segment.Text, Speaker: segment.Speaker, Extra: extra,
			})
		}
	} else {
		for _, segment := range encodedWire.Segments {
			result.Segments = append(result.Segments, TranscriptionSegment{
				ID: segment.ID, Start: segment.Start, End: segment.End,
				Text: segment.Text, Speaker: segment.Speaker,
			})
		}
	}
	return result, nil
}

// EncodeTranscriptionJSON renders the client transcription JSON document.
func EncodeTranscriptionJSON(result *TranscriptionResult) ([]byte, *Error) {
	doc := map[string]any{"text": result.Text}
	if result.TextOnly {
		body, err := json.Marshal(doc)
		if err != nil {
			return nil, protocolError("The provider transcription could not be encoded.")
		}
		return body, nil
	}
	if result.Language != nil {
		doc["language"] = *result.Language
	}
	if result.DurationSeconds != nil {
		doc["duration"] = *result.DurationSeconds
	}
	segments := make([]any, len(result.Segments))
	for i, segment := range result.Segments {
		item := map[string]any{"start": segment.Start, "end": segment.End, "text": segment.Text}
		if len(segment.ID) > 0 && string(segment.ID) != "null" {
			item["id"] = segment.ID
		}
		if segment.Speaker != nil {
			item["speaker"] = *segment.Speaker
		}
		for name, value := range segment.Extra {
			if _, exists := item[name]; !exists {
				item[name] = value
			}
		}
		segments[i] = item
	}
	if result.Segments != nil {
		doc["segments"] = segments
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, protocolError("The provider transcription could not be encoded.")
	}
	return body, nil
}

// MediaUsage is the token accounting one media SSE frame observed: input,
// output and cached counts plus the modality shares the operation reports.
type MediaUsage struct {
	Input, Output, Cached   *int64
	AudioInput, AudioOutput *int64
	ImageInput, ImageOutput *int64
}

// ObserveMediaUsage extracts token usage from a raw media SSE frame payload,
// resolved through the operation's modality rule: speech bills all output as
// audio, transcription details its audio input, and an image operation's
// output is its image unless the usage names another breakdown.
func ObserveMediaUsage(data, op string) *MediaUsage {
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
		// Speech models bill their whole output as audio.
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

// IsMediaTerminal reports whether a raw media SSE event name ends the stream.
func IsMediaTerminal(event string) bool {
	return strings.HasSuffix(event, ".completed") || event == "transcript.text.done" ||
		event == "speech.audio.done" || event == "error" || event == "done"
}

func protocolError(message string) *Error {
	return Fail(502, "provider_protocol_error", message)
}

// UnixTime converts a unix-seconds timestamp to time.Time.
func UnixTime(value *int64) *time.Time {
	if value == nil {
		return nil
	}
	t := time.Unix(*value, 0).UTC()
	return &t
}
