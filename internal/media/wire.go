package media

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Operation tags match the runtime selection registry.
const (
	OpImageGeneration = "image_generation"
	OpImageEdit       = "image_edit"
	OpImageVariation  = "image_variation"
	OpSpeech          = "speech"
	OpTranscription   = "transcription"
	OpVideoCreate     = "video_create"
	OpVideoList       = "video_list"
	OpVideoGet        = "video_get"
	OpVideoContent    = "video_content"
	OpVideoDelete     = "video_delete"
)

// Upload bounds reviewed against the reference limits.
const (
	DefaultImageUploadLimit    int64 = 50 * 1024 * 1024
	DefaultAudioUploadLimit    int64 = 25 * 1024 * 1024
	DefaultVideoReferenceLimit int64 = 20 * 1024 * 1024
	MaxVideoPromptLength             = 32000
)

// ResponseKind selects the upstream response handling rule.
type ResponseKind int

const (
	// ResponseImages is a JSON image set whose base64 payloads are staged.
	ResponseImages ResponseKind = iota
	// ResponseBinary is a staged binary body (speech audio).
	ResponseBinary
	// ResponseVideoJob is a video object document.
	ResponseVideoJob
	// ResponseVideoList is a video list document.
	ResponseVideoList
	// ResponseVideoContent is staged video or image bytes.
	ResponseVideoContent
	// ResponseVideoDelete is a deletion receipt document.
	ResponseVideoDelete
	// ResponseTranscription is text or JSON per the requested format.
	ResponseTranscription
	// ResponseSSE is a raw server-sent event passthrough.
	ResponseSSE
)

// Field is one upstream multipart field: either a text value or a staged file.
type Field struct {
	Name string
	Text *string
	File *Part
	Raw  bool // Text holds an already-encoded JSON document
}

// UpstreamCall is the exact upstream HTTP request for one media operation.
type UpstreamCall struct {
	Method    string
	Path      string
	Query     url.Values
	Accept    string
	JSON      []byte
	Fields    []Field
	Stream    bool
	Kind      ResponseKind
	Native    string
	Ambiguous bool // the request is not idempotent; post-dispatch failure is ambiguous
	// Inject carries W3C trace-context headers the caller allows upstream.
	// Only request-path calls set it; reconciliation traffic does not
	// propagate client trace context.
	Inject http.Header
}

// Request is a validated media operation bound for upstream dispatch. Staged
// file Parts stay owned by the request until Uploads releases them.
type Request struct {
	Op     string
	Route  string
	Stream bool

	Prompt                 string
	Input                  string // speech text
	Voice                  string
	Count                  *int64
	Size                   *string
	Format                 *string // response_format
	Quality                *string
	Style                  *string
	User                   *string
	Background             *string
	Moderation             *string
	InputFidelity          *string
	OutputCompression      *int64
	OutputFormat           *string
	PartialImages          *int64
	Language               *string
	TextPrompt             *string // transcription prompt
	Temperature            *float64
	Speed                  *float64
	StreamFormat           *string
	Instructions           *string
	Include                []string
	TimestampGranularities []string
	ChunkingStrategy       json.RawMessage
	KnownSpeakerNames      []string
	KnownSpeakerReferences []string
	Seconds                *string

	Images   []Part // image edit uploads in array order
	Mask     *Part
	Image    *Part // variation upload
	File     *Part // transcription upload
	InputRef *Part // video input_reference upload

	Extra map[string]any

	// SourceFields retains JSON member presence and raw values before typed decoding.
	// Multipart requests use their explicit typed fields and staged parts instead.
	SourceFields map[string]json.RawMessage

	// Video job operations.
	JobID           string
	Variant         string
	After           string
	Limit           int64
	Order           string
	DeleteMissingOK bool
}

// Uploads returns the staged handles this request owns.
func (r *Request) Uploads() []Handle {
	var handles []Handle
	appendParts := func(parts []Part) {
		for _, part := range parts {
			handles = append(handles, part.Handle)
		}
	}
	appendParts(r.Images)
	if r.Mask != nil {
		handles = append(handles, r.Mask.Handle)
	}
	if r.Image != nil {
		handles = append(handles, r.Image.Handle)
	}
	if r.File != nil {
		handles = append(handles, r.File.Handle)
	}
	if r.InputRef != nil {
		handles = append(handles, r.InputRef.Handle)
	}
	return handles
}

// Mode is the transport mode for selection.
func (r *Request) Mode() string {
	if r.Op == OpVideoCreate {
		return "async"
	}
	if r.Stream {
		return "streaming"
	}
	return "unary"
}

// SeedMediaUnits is the media quantity the request itself promises before any
// provider reply — the image count for streaming image operations.
func (r *Request) SeedMediaUnits() *string {
	if !r.Stream {
		return nil
	}
	switch r.Op {
	case OpImageGeneration, OpImageEdit:
		count := int64(1)
		if r.Count != nil {
			count = *r.Count
		}
		value := strconv.FormatInt(count, 10)
		return &value
	}
	return nil
}

func invalidMedia(message string) *Error {
	return Fail(400, "invalid_request", message)
}

func validRouteSlug(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return value[0] >= 'a' && value[0] <= 'z' || value[0] >= '0' && value[0] <= '9'
}

// jsonDoc marshals a field map for an upstream JSON body.
func jsonDoc(fields map[string]any, extra map[string]any) ([]byte, *Error) {
	doc := make(map[string]any, len(fields)+len(extra))
	for name, value := range fields {
		if value == nil {
			continue
		}
		doc[name] = value
	}
	for name, value := range extra {
		if _, exists := doc[name]; exists {
			return nil, invalidMedia("The extension field " + strconv.Quote(name) + " collides with a request field.")
		}
		doc[name] = value
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	return body, nil
}

// textField appends a text field to a multipart field list.
func textField(fields []Field, name string, value *string) []Field {
	if value == nil {
		return fields
	}
	return append(fields, Field{Name: name, Text: value})
}

func textValue(fields []Field, name, value string) []Field {
	return append(fields, Field{Name: name, Text: &value})
}

// extraFields renders client extension fields as upstream multipart text
// fields: arrays repeat as name[] and non-strings are compact JSON.
func extraFields(fields []Field, extra map[string]any) []Field {
	if len(extra) == 0 {
		return fields
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		value := extra[name]
		if values, ok := value.([]any); ok {
			field := name
			if !strings.HasSuffix(field, "[]") {
				field += "[]"
			}
			for _, item := range values {
				text := extraText(item)
				fields = append(fields, Field{Name: field, Text: &text})
			}
			continue
		}
		text := extraText(value)
		fields = append(fields, Field{Name: name, Text: &text})
	}
	return fields
}

func extraText(value any) string {
	switch value := value.(type) {
	case string:
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprint(value)
		}
		return string(data)
	}
}

// DecodeImageGeneration validates an image generation JSON body.
func DecodeImageGeneration(body []byte) (*Request, *Error) {
	var wire struct {
		Model             string  `json:"model"`
		Prompt            string  `json:"prompt"`
		N                 *int64  `json:"n"`
		Size              *string `json:"size"`
		Stream            bool    `json:"stream"`
		Quality           *string `json:"quality"`
		ResponseFormat    *string `json:"response_format"`
		Style             *string `json:"style"`
		User              *string `json:"user"`
		Background        *string `json:"background"`
		Moderation        *string `json:"moderation"`
		OutputCompression *int64  `json:"output_compression"`
		OutputFormat      *string `json:"output_format"`
		PartialImages     *int64  `json:"partial_images"`
	}
	extra, err := decodeJSON(body, &wire, "model", "prompt", "n", "size", "stream", "quality",
		"response_format", "style", "user", "background", "moderation", "output_compression",
		"output_format", "partial_images")
	if err != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	if strings.TrimSpace(wire.Prompt) == "" {
		return nil, invalidMedia("The image prompt is required.")
	}
	if wire.N != nil && *wire.N == 0 {
		return nil, invalidMedia("The image count must be at least 1.")
	}
	if !validRouteSlug(wire.Model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	source, sourceErr := sourceMediaFields(body)
	if sourceErr != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	return &Request{
		SourceFields: source,
		Op:           OpImageGeneration, Route: wire.Model, Stream: wire.Stream,
		Prompt: wire.Prompt, Count: wire.N, Size: wire.Size, Quality: wire.Quality,
		Format: wire.ResponseFormat, Style: wire.Style, User: wire.User,
		Background: wire.Background, Moderation: wire.Moderation,
		OutputCompression: wire.OutputCompression, OutputFormat: wire.OutputFormat,
		PartialImages: wire.PartialImages, Extra: extra,
	}, nil
}

// DecodeSpeech validates a speech JSON body.
func DecodeSpeech(body []byte) (*Request, *Error) {
	var wire struct {
		Model          string   `json:"model"`
		Input          string   `json:"input"`
		Voice          string   `json:"voice"`
		ResponseFormat *string  `json:"response_format"`
		Instructions   *string  `json:"instructions"`
		Speed          *float64 `json:"speed"`
		StreamFormat   *string  `json:"stream_format"`
	}
	extra, err := decodeJSON(body, &wire, "model", "input", "voice", "response_format",
		"instructions", "speed", "stream_format")
	if err != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	if wire.Input == "" {
		return nil, invalidMedia("The speech input is required.")
	}
	if wire.Voice == "" {
		return nil, invalidMedia("The speech voice is required.")
	}
	if wire.Speed != nil && (*wire.Speed < 0.25 || *wire.Speed > 4.0) {
		return nil, invalidMedia("The speech speed must be between 0.25 and 4.0.")
	}
	if !validRouteSlug(wire.Model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	source, sourceErr := sourceMediaFields(body)
	if sourceErr != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	return &Request{
		SourceFields: source,
		Op:           OpSpeech, Route: wire.Model, Stream: wire.StreamFormat != nil && *wire.StreamFormat == "sse",
		Input: wire.Input, Voice: wire.Voice, Format: wire.ResponseFormat,
		Instructions: wire.Instructions, Speed: wire.Speed,
		StreamFormat: wire.StreamFormat, Extra: extra,
	}, nil
}

// decodeJSON unmarshals body into wire and returns the remaining top-level
// fields as extension values. Duplicate or colliding keys are rejected by the
// encoder at dispatch time.
func decodeJSON(body []byte, wire any, known ...string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(wire); err != nil {
		return nil, err
	}
	var doc map[string]any
	decoder = json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&doc); err != nil {
		return nil, err
	}
	for _, name := range known {
		delete(doc, name)
	}
	if len(doc) == 0 {
		return nil, nil
	}
	extra := make(map[string]any, len(doc))
	for name, value := range doc {
		extra[name] = normalizeJSON(value)
	}
	return extra, nil
}

// normalizeJSON decodes numbers as their wire values rather than json.Number
// so extension fields re-encode identically upstream.
func normalizeJSON(value any) any {
	switch value := value.(type) {
	case json.Number:
		if i, err := value.Int64(); err == nil {
			return i
		}
		if f, err := value.Float64(); err == nil {
			return f
		}
		return value.String()
	case map[string]any:
		for k, v := range value {
			value[k] = normalizeJSON(v)
		}
		return value
	case []any:
		for i, v := range value {
			value[i] = normalizeJSON(v)
		}
		return value
	}
	return value
}

// DecodeImageEdit validates a parsed image-edit form.
func DecodeImageEdit(form *Form) (*Request, *Error) {
	images, failure := form.TakeFilesWithPrefix("image")
	if failure != nil {
		return nil, failure
	}
	mask, failure := form.TakeSingleFile("mask")
	if failure != nil {
		return nil, failure
	}
	model, failure := form.Required("model")
	if failure != nil {
		return nil, failure
	}
	prompt, failure := form.Required("prompt")
	if failure != nil {
		return nil, failure
	}
	n, failure := form.OptionalInt("n")
	if failure != nil {
		return nil, failure
	}
	size, failure := form.Optional("size")
	if failure != nil {
		return nil, failure
	}
	stream, failure := form.OptionalBool("stream")
	if failure != nil {
		return nil, failure
	}
	quality, failure := form.Optional("quality")
	if failure != nil {
		return nil, failure
	}
	responseFormat, failure := form.Optional("response_format")
	if failure != nil {
		return nil, failure
	}
	user, failure := form.Optional("user")
	if failure != nil {
		return nil, failure
	}
	background, failure := form.Optional("background")
	if failure != nil {
		return nil, failure
	}
	inputFidelity, failure := form.Optional("input_fidelity")
	if failure != nil {
		return nil, failure
	}
	outputCompression, failure := form.OptionalInt("output_compression")
	if failure != nil {
		return nil, failure
	}
	outputFormat, failure := form.Optional("output_format")
	if failure != nil {
		return nil, failure
	}
	partialImages, failure := form.OptionalInt("partial_images")
	if failure != nil {
		return nil, failure
	}
	extra, failure := form.TakeExtensions()
	if failure != nil {
		return nil, failure
	}
	if len(images) == 0 {
		return nil, invalidMedia("The image file is required.")
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, invalidMedia("The image prompt is required.")
	}
	if n != nil && *n == 0 {
		return nil, invalidMedia("The image count must be at least 1.")
	}
	if !validRouteSlug(model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	extraAny := make(map[string]any, len(extra))
	for name, value := range extra {
		extraAny[name] = value
	}
	var count64 *int64
	if n != nil {
		v := int64(*n)
		count64 = &v
	}
	return &Request{
		Op: OpImageEdit, Route: model, Stream: stream != nil && *stream,
		Prompt: prompt, Count: count64, Size: size, Quality: quality,
		Format: responseFormat, User: user, Background: background,
		InputFidelity: inputFidelity, OutputCompression: int64Ptr(outputCompression),
		OutputFormat: outputFormat, PartialImages: int64Ptr(partialImages),
		Images: images, Mask: mask, Extra: extraAny,
	}, nil
}

// DecodeImageVariation validates a parsed image-variation form.
func DecodeImageVariation(form *Form) (*Request, *Error) {
	image, failure := form.TakeSingleFile("image")
	if failure != nil {
		return nil, failure
	}
	model, failure := form.Required("model")
	if failure != nil {
		return nil, failure
	}
	n, failure := form.OptionalInt("n")
	if failure != nil {
		return nil, failure
	}
	size, failure := form.Optional("size")
	if failure != nil {
		return nil, failure
	}
	responseFormat, failure := form.Optional("response_format")
	if failure != nil {
		return nil, failure
	}
	user, failure := form.Optional("user")
	if failure != nil {
		return nil, failure
	}
	extra, failure := form.TakeExtensions()
	if failure != nil {
		return nil, failure
	}
	if image == nil {
		return nil, invalidMedia("The image file is required.")
	}
	if n != nil && *n == 0 {
		return nil, invalidMedia("The image count must be at least 1.")
	}
	if !validRouteSlug(model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	extraAny := make(map[string]any, len(extra))
	for name, value := range extra {
		extraAny[name] = value
	}
	return &Request{
		Op: OpImageVariation, Route: model,
		Count: int64Ptr(n), Size: size, Format: responseFormat, User: user,
		Image: image, Extra: extraAny,
	}, nil
}

// DecodeTranscription validates a parsed transcription form.
func DecodeTranscription(form *Form) (*Request, *Error) {
	file, failure := form.TakeSingleFile("file")
	if failure != nil {
		return nil, failure
	}
	responseFormat, failure := form.Optional("response_format")
	if failure != nil {
		return nil, failure
	}
	names, failure := form.TakeRepeated("known_speaker_names")
	if failure != nil {
		return nil, failure
	}
	references, failure := form.TakeRepeated("known_speaker_references")
	if failure != nil {
		return nil, failure
	}
	model, failure := form.Required("model")
	if failure != nil {
		return nil, failure
	}
	language, failure := form.Optional("language")
	if failure != nil {
		return nil, failure
	}
	prompt, failure := form.Optional("prompt")
	if failure != nil {
		return nil, failure
	}
	temperature, failure := form.OptionalFloat("temperature")
	if failure != nil {
		return nil, failure
	}
	include, failure := form.TakeRepeated("include")
	if failure != nil {
		return nil, failure
	}
	granularities, failure := form.TakeRepeated("timestamp_granularities")
	if failure != nil {
		return nil, failure
	}
	chunking, failure := form.Optional("chunking_strategy")
	if failure != nil {
		return nil, failure
	}
	stream, failure := form.OptionalBool("stream")
	if failure != nil {
		return nil, failure
	}
	extra, failure := form.TakeExtensions()
	if failure != nil {
		return nil, failure
	}
	if file == nil {
		return nil, invalidMedia("The audio file is required.")
	}
	if temperature != nil && (*temperature < 0 || *temperature > 1) {
		return nil, invalidMedia("The transcription temperature must be between 0 and 1.")
	}
	format := "json"
	if responseFormat != nil {
		format = *responseFormat
	}
	switch format {
	case "json", "text", "srt", "verbose_json", "vtt", "diarized_json":
	default:
		return nil, invalidMedia("The transcription response_format is not supported.")
	}
	if len(granularities) > 0 && (format != "verbose_json" || !allOf(granularities, "word", "segment")) {
		return nil, invalidMedia("The timestamp_granularities field requires verbose_json with word or segment values.")
	}
	if len(include) > 0 && (format != "json" || !allOf(include, "logprobs")) {
		return nil, invalidMedia("The include field requires the json response format with logprobs values.")
	}
	if (len(names) > 0 || len(references) > 0) &&
		(format != "diarized_json" || len(names) == 0 || len(names) > 4 ||
			len(names) != len(references) || !validSpeakerNames(names) || !validSpeakerReferences(references)) {
		return nil, invalidMedia("The known speaker fields require diarized_json with at most four name/reference pairs.")
	}
	var chunkingJSON json.RawMessage
	if chunking != nil {
		if !json.Valid([]byte(*chunking)) {
			return nil, invalidMedia("chunking_strategy must be JSON.")
		}
		chunkingJSON = json.RawMessage(*chunking)
	}
	if !validRouteSlug(model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	extraAny := make(map[string]any, len(extra))
	for name, value := range extra {
		extraAny[name] = value
	}
	return &Request{
		Op: OpTranscription, Route: model, Stream: stream != nil && *stream,
		File: file, Format: responseFormat, Language: language, TextPrompt: prompt,
		Temperature: temperature, Include: include, TimestampGranularities: granularities,
		ChunkingStrategy: chunkingJSON, KnownSpeakerNames: names,
		KnownSpeakerReferences: references, Extra: extraAny,
	}, nil
}

func allOf(values []string, allowed ...string) bool {
	for _, value := range values {
		ok := slices.Contains(allowed, value)
		if !ok {
			return false
		}
	}
	return true
}

func validSpeakerNames(names []string) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == "" || len(name) > 64 {
			return false
		}
	}
	return true
}

func validSpeakerReferences(references []string) bool {
	for _, reference := range references {
		if !strings.HasPrefix(reference, "data:audio/") {
			return false
		}
	}
	return true
}

// DecodeVideoCreate validates a parsed video-create form.
func DecodeVideoCreate(form *Form) (*Request, *Error) {
	model, failure := form.Required("model")
	if failure != nil {
		return nil, failure
	}
	prompt, failure := form.Required("prompt")
	if failure != nil {
		return nil, failure
	}
	inputRef, failure := form.TakeSingleFile("input_reference")
	if failure != nil {
		return nil, failure
	}
	seconds, failure := form.Optional("seconds")
	if failure != nil {
		return nil, failure
	}
	size, failure := form.Optional("size")
	if failure != nil {
		return nil, failure
	}
	extra, failure := form.TakeExtensions()
	if failure != nil {
		return nil, failure
	}
	if prompt == "" || len(prompt) > MaxVideoPromptLength {
		return nil, invalidMedia("The video prompt must contain 1 to 32000 bytes.")
	}
	if seconds != nil && (*seconds != "4" && *seconds != "8" && *seconds != "12") {
		return nil, invalidMedia("The video seconds must be one of 4, 8, or 12.")
	}
	if size != nil && (*size != "720x1280" && *size != "1280x720" && *size != "1024x1792" && *size != "1792x1024") {
		return nil, invalidMedia("The video size is not supported.")
	}
	if inputRef != nil && inputRef.ContentType != "" && !strings.HasPrefix(inputRef.ContentType, "image/") {
		return nil, invalidMedia("The video input_reference must be an image file.")
	}
	if !validRouteSlug(model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	extraAny := make(map[string]any, len(extra))
	for name, value := range extra {
		extraAny[name] = value
	}
	return &Request{
		Op: OpVideoCreate, Route: model, Prompt: prompt,
		Seconds: seconds, Size: size, InputRef: inputRef, Extra: extraAny,
	}, nil
}

// ListQuery is the validated video list page request.
type ListQuery struct {
	After *uuid.UUID
	Order Order
	Limit int
}

// ValidateVideoListQuery checks the OpenAI video list query parameters.
func ValidateVideoListQuery(query url.Values) (*ListQuery, *Error) {
	for name := range query {
		if name != "after" && name != "limit" && name != "order" {
			return nil, invalidMedia("Video list contains unsupported query parameters.")
		}
	}
	out := &ListQuery{Order: OrderDescending, Limit: 20}
	if values := query["limit"]; len(values) == 1 {
		limit, err := strconv.ParseInt(values[0], 10, 32)
		if err != nil || limit < 1 || limit > 100 {
			return nil, invalidMedia("Video list limit must be between 1 and 100.")
		}
		out.Limit = int(limit)
	}
	switch order := query.Get("order"); order {
	case "":
	case "asc":
		out.Order = OrderAscending
	case "desc":
	default:
		return nil, invalidMedia("Video list order must be asc or desc.")
	}
	if after := query.Get("after"); after != "" {
		cursor, err := uuid.Parse(after)
		if err != nil {
			return nil, invalidMedia("The video cursor is invalid.")
		}
		out.After = &cursor
	}
	return out, nil
}

// ValidateVideoContentQuery checks the content variant parameter.
func ValidateVideoContentQuery(query url.Values) (string, *Error) {
	variant := query.Get("variant")
	switch variant {
	case "", "video", "thumbnail", "spritesheet":
		return variant, nil
	}
	return "", invalidMedia("The video content variant must be video, thumbnail, or spritesheet.")
}

// Encode builds the upstream HTTP request for a validated media operation.
func Encode(r *Request, kind, upstreamModel string) (*UpstreamCall, *Error) {
	switch r.Op {
	case OpImageGeneration:
		return encodeImageGeneration(r, kind, upstreamModel)
	case OpImageEdit:
		return encodeImageEdit(r, upstreamModel)
	case OpImageVariation:
		return encodeImageVariation(r, upstreamModel)
	case OpSpeech:
		return encodeSpeech(r, upstreamModel)
	case OpTranscription:
		return encodeTranscription(r, upstreamModel)
	case OpVideoCreate:
		return encodeVideoCreate(r, upstreamModel)
	case OpVideoList:
		return encodeVideoList(r)
	case OpVideoGet:
		return encodeVideoJob(r.JobID, "")
	case OpVideoContent:
		return encodeVideoContent(r)
	case OpVideoDelete:
		return encodeVideoJob(r.JobID, "delete")
	}
	return nil, invalidMedia("The media operation is not supported.")
}

func encodeImageGeneration(r *Request, kind, model string) (*UpstreamCall, *Error) {
	switch kind {
	case "vertex_ai":
		return encodeVertexImage(r, model)
	case "bedrock":
		return encodeBedrockImage(r, model)
	}
	fields := map[string]any{
		"model":              model,
		"prompt":             r.Prompt,
		"n":                  r.Count,
		"size":               r.Size,
		"quality":            r.Quality,
		"response_format":    r.Format,
		"style":              r.Style,
		"user":               r.User,
		"background":         r.Background,
		"moderation":         r.Moderation,
		"output_compression": r.OutputCompression,
		"output_format":      r.OutputFormat,
		"partial_images":     r.PartialImages,
	}
	if r.Stream {
		fields["stream"] = true
	}
	body, failure := jsonDoc(fields, r.Extra)
	if failure != nil {
		return nil, failure
	}
	responseKind := ResponseImages
	if r.Stream {
		responseKind = ResponseSSE
	}
	return &UpstreamCall{Method: "POST", Path: "images/generations", Accept: "application/json",
		JSON: body, Stream: r.Stream, Kind: responseKind, Ambiguous: true}, nil
}

func encodeImageEdit(r *Request, model string) (*UpstreamCall, *Error) {
	fields := []Field{}
	fields = textValue(fields, "model", model)
	fields = textValue(fields, "prompt", r.Prompt)
	if len(r.Images) == 1 {
		fields = append(fields, Field{Name: "image", File: &r.Images[0]})
	} else {
		for i := range r.Images {
			fields = append(fields, Field{Name: "image[" + strconv.Itoa(i) + "]", File: &r.Images[i]})
		}
	}
	if r.Mask != nil {
		fields = append(fields, Field{Name: "mask", File: r.Mask})
	}
	fields = textField(fields, "n", int64Text(r.Count))
	fields = textField(fields, "size", r.Size)
	fields = textValue(fields, "stream", strconv.FormatBool(r.Stream))
	fields = textField(fields, "quality", r.Quality)
	fields = textField(fields, "response_format", r.Format)
	fields = textField(fields, "user", r.User)
	fields = textField(fields, "background", r.Background)
	fields = textField(fields, "input_fidelity", r.InputFidelity)
	fields = textField(fields, "output_compression", int64Text(r.OutputCompression))
	fields = textField(fields, "output_format", r.OutputFormat)
	fields = textField(fields, "partial_images", int64Text(r.PartialImages))
	fields = extraFields(fields, r.Extra)
	responseKind := ResponseImages
	if r.Stream {
		responseKind = ResponseSSE
	}
	return &UpstreamCall{Method: "POST", Path: "images/edits", Accept: "application/json",
		Fields: fields, Stream: r.Stream, Kind: responseKind, Ambiguous: true}, nil
}

func encodeImageVariation(r *Request, model string) (*UpstreamCall, *Error) {
	fields := []Field{}
	fields = textValue(fields, "model", model)
	fields = append(fields, Field{Name: "image", File: r.Image})
	fields = textField(fields, "n", int64Text(r.Count))
	fields = textField(fields, "size", r.Size)
	fields = textField(fields, "response_format", r.Format)
	fields = textField(fields, "user", r.User)
	fields = extraFields(fields, r.Extra)
	return &UpstreamCall{Method: "POST", Path: "images/variations", Accept: "application/json",
		Fields: fields, Kind: ResponseImages, Ambiguous: true}, nil
}

func encodeSpeech(r *Request, model string) (*UpstreamCall, *Error) {
	fields := map[string]any{
		"model":           model,
		"input":           r.Input,
		"voice":           r.Voice,
		"response_format": r.Format,
		"instructions":    r.Instructions,
		"speed":           r.Speed,
	}
	if r.Stream {
		fields["stream_format"] = "sse"
	} else if r.StreamFormat != nil {
		fields["stream_format"] = *r.StreamFormat
	}
	body, failure := jsonDoc(fields, r.Extra)
	if failure != nil {
		return nil, failure
	}
	responseKind := ResponseBinary
	if r.Stream {
		responseKind = ResponseSSE
	}
	return &UpstreamCall{Method: "POST", Path: "audio/speech", Accept: "application/json",
		JSON: body, Stream: r.Stream, Kind: responseKind, Ambiguous: true}, nil
}

func encodeTranscription(r *Request, model string) (*UpstreamCall, *Error) {
	fields := []Field{}
	fields = textValue(fields, "model", model)
	fields = append(fields, Field{Name: "file", File: r.File})
	fields = textField(fields, "language", r.Language)
	fields = textField(fields, "prompt", r.TextPrompt)
	fields = textField(fields, "response_format", r.Format)
	fields = textField(fields, "temperature", float64Text(r.Temperature))
	for _, value := range r.Include {
		fields = textValue(fields, "include[]", value)
	}
	for _, value := range r.TimestampGranularities {
		fields = textValue(fields, "timestamp_granularities[]", value)
	}
	if len(r.ChunkingStrategy) > 0 {
		fields = append(fields, Field{Name: "chunking_strategy", Text: new(string(r.ChunkingStrategy))})
	}
	for i, name := range r.KnownSpeakerNames {
		fields = textValue(fields, "known_speaker_names[]", name)
		fields = textValue(fields, "known_speaker_references[]", r.KnownSpeakerReferences[i])
	}
	fields = textValue(fields, "stream", strconv.FormatBool(r.Stream))
	fields = extraFields(fields, r.Extra)
	responseKind := ResponseTranscription
	if r.Stream {
		responseKind = ResponseSSE
	}
	return &UpstreamCall{Method: "POST", Path: "audio/transcriptions", Accept: "application/json",
		Fields: fields, Stream: r.Stream, Kind: responseKind, Ambiguous: true}, nil
}

func encodeVideoCreate(r *Request, model string) (*UpstreamCall, *Error) {
	fields := []Field{}
	fields = textValue(fields, "model", model)
	fields = textValue(fields, "prompt", r.Prompt)
	fields = textField(fields, "seconds", r.Seconds)
	fields = textField(fields, "size", r.Size)
	if r.InputRef != nil {
		fields = append(fields, Field{Name: "input_reference", File: r.InputRef})
	}
	fields = extraFields(fields, r.Extra)
	return &UpstreamCall{Method: "POST", Path: "videos", Accept: "application/json",
		Fields: fields, Kind: ResponseVideoJob, Ambiguous: true}, nil
}

func encodeVideoList(r *Request) (*UpstreamCall, *Error) {
	query := url.Values{}
	if r.After != "" {
		query.Set("after", r.After)
	}
	if r.Limit > 0 {
		query.Set("limit", strconv.FormatInt(r.Limit, 10))
	}
	if r.Order == "asc" {
		query.Set("order", "asc")
	}
	return &UpstreamCall{Method: "GET", Path: "videos", Query: query, Accept: "application/json",
		Kind: ResponseVideoList}, nil
}

// ValidUpstreamJobID reports whether an upstream job identity is safe to
// place in a resource path.
func ValidUpstreamJobID(value string) bool {
	return value != "" && len(value) <= 1024 && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00") && validJobIDChars(value)
}

func validJobIDChars(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// VideoJobPath builds the upstream videos/{id}[/suffix] resource path with the
// strict identifier policy the provider API accepts.
func VideoJobPath(jobID, suffix string) (string, *Error) {
	if jobID == "" || len(jobID) > 256 {
		return "", invalidMedia("The upstream video job ID is invalid.")
	}
	for i := 0; i < len(jobID); i++ {
		c := jobID[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return "", invalidMedia("The upstream video job ID is invalid.")
		}
	}
	if suffix == "" {
		return "videos/" + jobID, nil
	}
	return "videos/" + jobID + "/" + suffix, nil
}

func encodeVideoJob(jobID, kind string) (*UpstreamCall, *Error) {
	path, failure := VideoJobPath(jobID, "")
	if failure != nil {
		return nil, failure
	}
	if kind == "delete" {
		return &UpstreamCall{Method: "DELETE", Path: path, Accept: "application/json",
			Kind: ResponseVideoDelete, Ambiguous: true}, nil
	}
	return &UpstreamCall{Method: "GET", Path: path, Accept: "application/json",
		Kind: ResponseVideoJob}, nil
}

func encodeVideoContent(r *Request) (*UpstreamCall, *Error) {
	path, failure := VideoJobPath(r.JobID, "content")
	if failure != nil {
		return nil, failure
	}
	query := url.Values{}
	if r.Variant != "" {
		query.Set("variant", r.Variant)
	}
	return &UpstreamCall{Method: "GET", Path: path, Query: query,
		Accept: "*/*", Kind: ResponseVideoContent}, nil
}

func int64Text(value *int64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatInt(*value, 10)
	return &text
}

func float64Text(value *float64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatFloat(*value, 'g', -1, 64)
	return &text
}

func int64Ptr(value *int) *int64 {
	if value == nil {
		return nil
	}
	v := int64(*value)
	return &v
}

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
	Handle        *Handle
	RevisedPrompt *string
}

// ImageUsage is the token usage an image response reported.
type ImageUsage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
}

// DecodeImageResponse parses an upstream image document, staging each
// base64 payload through stage before the result is returned.
func DecodeImageResponse(body []byte, stage func(b64 string, index int) (*Artifact, *Error)) (*ImageResult, *Error) {
	var wire struct {
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
		} `json:"usage"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	extra := diffFields(doc, "created", "data", "usage")
	result := &ImageResult{CreatedAt: wire.Created, Extra: extra}
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
	for i, image := range wire.Data {
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
	if wire.Usage != nil {
		result.Usage = &ImageUsage{
			InputTokens:  wire.Usage.InputTokens,
			OutputTokens: wire.Usage.OutputTokens,
			TotalTokens:  wire.Usage.TotalTokens,
		}
	}
	return result, nil
}

func DecodeNativeImageResponse(kind string, body []byte, expected int64, stage func(b64 string, index int) (*Artifact, *Error)) (*ImageResult, *Error) {
	var wire struct {
		Predictions []struct {
			BytesBase64Encoded string `json:"bytesBase64Encoded"`
			MIMEType           string `json:"mimeType"`
		} `json:"predictions"`
		Images []string `json:"images"`
		Error  *string  `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	if wire.Error != nil && *wire.Error != "" {
		return nil, protocolError("The provider returned an image error.")
	}
	var encoded []string
	switch kind {
	case "vertex_ai":
		if wire.Predictions == nil {
			return nil, protocolError("The provider image response has no predictions.")
		}
		for _, prediction := range wire.Predictions {
			if prediction.MIMEType != "" && !strings.HasPrefix(prediction.MIMEType, "image/") {
				return nil, protocolError("The provider image response carries an invalid MIME type.")
			}
			encoded = append(encoded, prediction.BytesBase64Encoded)
		}
	case "bedrock":
		if wire.Images == nil {
			return nil, protocolError("The provider image response has no images.")
		}
		encoded = wire.Images
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
	var wire struct {
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
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider video response is not valid JSON.")
	}
	if wire.Object != "video" {
		return nil, protocolError("The provider returned an unexpected video object type.")
	}
	switch wire.Status {
	case "queued", "in_progress", "completed", "failed":
	default:
		// Unknown provider statuses stay observable through the object but do
		// not map onto a client-visible lifecycle state.
	}
	result := &VideoJobResult{
		ID: wire.ID, Status: wire.Status, Progress: wire.Progress,
		CreatedAt: wire.CreatedAt, CompletedAt: wire.Completed, ExpiresAt: wire.Expires,
		Prompt: wire.Prompt, Seconds: wire.Seconds, Size: wire.Size, RemixedFrom: wire.Remixed,
		Extra: diffFields(doc, "id", "object", "model", "status", "progress", "created_at",
			"completed_at", "expires_at", "prompt", "seconds", "size", "remixed_from_video_id", "error"),
	}
	if wire.Error != nil {
		result.ErrorCode = &wire.Error.Code
		result.ErrorMessage = &wire.Error.Message
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

// DecodeVideoListResponse parses a provider list document.
func DecodeVideoListResponse(body []byte) (*VideoListResult, *Error) {
	var wire struct {
		Object  string            `json:"object"`
		Data    []json.RawMessage `json:"data"`
		FirstID *string           `json:"first_id"`
		LastID  *string           `json:"last_id"`
		HasMore bool              `json:"has_more"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider video list is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider video list is not valid JSON.")
	}
	if wire.Object != "list" {
		return nil, protocolError("The provider returned an unexpected video list object type.")
	}
	result := &VideoListResult{
		FirstID: wire.FirstID, LastID: wire.LastID, HasMore: wire.HasMore,
		Extra: diffFields(doc, "object", "data", "first_id", "last_id", "has_more"),
	}
	for _, raw := range wire.Data {
		job, failure := DecodeVideoObject(raw)
		if failure != nil {
			return nil, failure
		}
		result.Jobs = append(result.Jobs, *job)
	}
	return result, nil
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
	var wire struct {
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
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider video delete response is not valid JSON.")
	}
	return &VideoDeleteResult{
		ID: wire.ID, Deleted: wire.Deleted, Object: wire.Object,
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
	Text            string
	Language        *string
	DurationSeconds *float64
	Segments        []TranscriptionSegment
	Extra           map[string]any
}

// TranscriptionSegment is one timed text span.
type TranscriptionSegment struct {
	ID      *int64
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
	var wire struct {
		Text     string   `json:"text"`
		Language *string  `json:"language"`
		Duration *float64 `json:"duration"`
		Segments []struct {
			ID      *int64  `json:"id"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
			Text    string  `json:"text"`
			Speaker *string `json:"speaker"`
		} `json:"segments"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, protocolError("The provider transcription is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, protocolError("The provider transcription is not valid JSON.")
	}
	result := &TranscriptionResult{
		Text: wire.Text, Language: wire.Language, DurationSeconds: wire.Duration,
		Extra: diffFields(doc, "text", "language", "duration", "segments"),
	}
	if segments, ok := doc["segments"].([]any); ok && len(segments) == len(wire.Segments) {
		for i, item := range segments {
			extra := map[string]any(nil)
			if object, ok := item.(map[string]any); ok {
				extra = diffFields(object, "id", "start", "end", "text", "speaker")
			}
			segment := wire.Segments[i]
			result.Segments = append(result.Segments, TranscriptionSegment{
				ID: segment.ID, Start: segment.Start, End: segment.End,
				Text: segment.Text, Speaker: segment.Speaker, Extra: extra,
			})
		}
	} else {
		for _, segment := range wire.Segments {
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
	if result.Language != nil {
		doc["language"] = *result.Language
	}
	if result.DurationSeconds != nil {
		doc["duration"] = *result.DurationSeconds
	}
	segments := make([]any, len(result.Segments))
	for i, segment := range result.Segments {
		item := map[string]any{"start": segment.Start, "end": segment.End, "text": segment.Text}
		if segment.ID != nil {
			item["id"] = *segment.ID
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

// ObserveMediaUsage extracts token usage from a raw media SSE frame payload.
func ObserveMediaUsage(data string) (input, output, cached *int64) {
	var frame struct {
		Usage *struct {
			InputTokens       *int64 `json:"input_tokens"`
			PromptTokens      *int64 `json:"prompt_tokens"`
			OutputTokens      *int64 `json:"output_tokens"`
			CompletionTokens  *int64 `json:"completion_tokens"`
			InputTokenDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			PromptTokenDetails *struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &frame); err != nil || frame.Usage == nil {
		return nil, nil, nil
	}
	usage := frame.Usage
	input = firstInt(usage.InputTokens, usage.PromptTokens)
	output = firstInt(usage.OutputTokens, usage.CompletionTokens)
	if usage.InputTokenDetails != nil && usage.InputTokenDetails.CachedTokens != nil {
		cached = usage.InputTokenDetails.CachedTokens
	} else if usage.PromptTokenDetails != nil {
		cached = usage.PromptTokenDetails.CachedTokens
	}
	return input, output, cached
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

func encodeVertexImage(r *Request, model string) (*UpstreamCall, *Error) {
	if !strings.HasPrefix(model, "imagen-") {
		return nil, invalidMedia("The configured model is not a qualified Vertex Imagen model.")
	}
	if err := rejectNativeImageFields(r); err != nil {
		return nil, err
	}
	count := int64(1)
	if r.Count != nil {
		count = *r.Count
	}
	parameters := map[string]any{"sampleCount": count}
	if r.Size != nil {
		aspect, ok := map[string]string{"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3"}[*r.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by the Vertex Imagen target.")
		}
		parameters["aspectRatio"] = aspect
	}
	body, err := json.Marshal(map[string]any{
		"instances":  []map[string]any{{"prompt": r.Prompt}},
		"parameters": parameters,
	})
	if err != nil {
		return nil, invalidMedia("The native image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "models/" + url.PathEscape(model) + ":predict",
		JSON: body, Kind: ResponseImages, Native: "vertex_ai", Ambiguous: true}, nil
}

func encodeBedrockImage(r *Request, model string) (*UpstreamCall, *Error) {
	if !strings.HasPrefix(model, "amazon.titan-image-generator-") {
		return nil, invalidMedia("The configured model is not a qualified Bedrock Titan image model.")
	}
	if err := rejectNativeImageFields(r); err != nil {
		return nil, err
	}
	count := int64(1)
	if r.Count != nil {
		count = *r.Count
	}
	width, height := 1024, 1024
	if r.Size != nil {
		parts := strings.SplitN(*r.Size, "x", 2)
		if len(parts) != 2 {
			return nil, invalidMedia("The size is not supported by the Bedrock Titan target.")
		}
		parsed, parseErr := strconv.Atoi(parts[0])
		parsedH, parseErrH := strconv.Atoi(parts[1])
		if parseErr != nil || parseErrH != nil {
			return nil, invalidMedia("The size is not supported by the Bedrock Titan target.")
		}
		width, height = parsed, parsedH
		switch *r.Size {
		case "1024x1024", "768x768", "512x512":
		default:
			return nil, invalidMedia("The size is not supported by the Bedrock Titan target.")
		}
	}
	body, err := json.Marshal(map[string]any{
		"taskType":          "TEXT_IMAGE",
		"textToImageParams": map[string]any{"text": r.Prompt},
		"imageGenerationConfig": map[string]any{
			"numberOfImages": count,
			"width":          width,
			"height":         height,
		},
	})
	if err != nil {
		return nil, invalidMedia("The native image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "model/" + url.PathEscape(model) + "/invoke",
		JSON: body, Kind: ResponseImages, Native: "bedrock", Ambiguous: true}, nil
}

func rejectNativeImageFields(r *Request) *Error {
	switch {
	case r.Format != nil && *r.Format != "b64_json":
		return invalidMedia("Native image targets only support b64_json responses.")
	case r.Quality != nil, r.Style != nil, r.User != nil, r.Background != nil, r.Moderation != nil,
		r.OutputCompression != nil, r.OutputFormat != nil, r.PartialImages != nil:
		return invalidMedia("The native image target does not support the requested image parameters.")
	case r.Stream:
		return invalidMedia("The native image target does not support streaming.")
	case r.Count != nil && (*r.Count < 1 || *r.Count > 4):
		return invalidMedia("Native image targets support n from 1 to 4.")
	case len(r.Extra) > 0:
		return invalidMedia("The native image target does not support extension fields.")
	}
	return nil
}
