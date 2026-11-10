package media

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/staging"
)

// Operation tags match the runtime selection registry.
const (
	OpImageGeneration = "image_generation"
	OpImageEdit       = "image_edit"
	OpImageVariation  = "image_variation"
	OpSpeech          = "speech"
	OpTranscription   = "transcription"
	OpTranslation     = "translation"
	OpVideoCreate     = "video_create"
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
	File *staging.Part
	Raw  bool // Text holds an already-encoded JSON document
}

// UpstreamCall is the exact upstream HTTP request for one media operation.
type UpstreamCall struct {
	Method string
	Path   string
	// URL addresses a call at another service of the connector's cloud, in
	// place of Path under its endpoint; SigningService signs it for that
	// service.
	URL            string
	SigningService string
	Query          url.Values
	Accept         string
	JSON           []byte
	Fields         []staging.Field
	// Upload is a staged file sent as the whole request body, for vendors
	// that take raw audio rather than a multipart form.
	Upload    *staging.Part
	Stream    bool
	Kind      ResponseKind
	Native    string
	Strict    bool // local decoding contract; never an upstream wire member
	Ambiguous bool // the request is not idempotent; post-dispatch failure is ambiguous
	// DecodeImages and DecodeTranscription read a vendor wire's JSON result
	// in place of the OpenAI decoders.
	DecodeImages        func(body []byte, stage func(b64 string, index int) (*staging.Artifact, *Error)) (*ImageResult, *Error)
	DecodeTranscription func(body []byte) (*TranscriptionResult, *Error)
	// DecodeVideo reads a vendor's video job object, and NoContent marks a
	// call the vendor answers with 204 and no body, such as a deletion.
	DecodeVideo func(body []byte) (*VideoJobResult, *Error)
	NoContent   bool
	// DecodeAudio reads a vendor's JSON speech result: the audio it carries
	// and the tokens it bills.
	DecodeAudio func(body []byte) (*AudioResult, *Error)
	// JSONFrom builds a JSON body that inlines uploaded files, whose bytes
	// read returns, in place of JSON.
	JSONFrom func(read func(*staging.Part) ([]byte, *Error)) ([]byte, *Error)
	// CharacterHeader names the response header in which a vendor reports
	// the characters a speech call bills.
	CharacterHeader string
	// Next continues a vendor's asynchronous work from each JSON response,
	// its submission's first: the next request, until a step fetches the
	// image itself or marks the response its result. A step's URL must be
	// on the provider endpoint's host or under one of StepDomains.
	Next        func(body []byte) (*Step, *Error)
	StepDomains []string
	// Inject carries W3C trace-context headers the caller allows upstream.
	// Only request-path calls set it; reconciliation traffic does not
	// propagate client trace context.
	Inject http.Header
}

// AudioResult is speech a vendor returned inside JSON.
type AudioResult struct {
	Audio       []byte
	ContentType string
	Tokens      *ImageUsage
}

// Step is the next request of a vendor's asynchronous work: a request of an
// absolute URL after Wait, GET unless Method names another, with JSON as its
// body and the provider's credential only when the vendor requires it there.
type Step struct {
	Method      string
	URL         string
	JSON        []byte
	Wait        time.Duration
	Credentials bool
	// Asset marks the step that fetches the work's product, which ends it:
	// a response of a content type with this prefix, such as image/ or
	// video/.
	Asset string
	// Done marks the response just read as the work's result, which the
	// call's decoder reads; Cleanup, if any, then deletes the vendor's copy.
	Done    bool
	Cleanup *Step
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

	Images   []staging.Part // image edit uploads in array order
	Mask     *staging.Part
	Image    *staging.Part // variation upload
	File     *staging.Part // transcription upload
	InputRef *staging.Part // video input_reference upload

	Extra map[string]any

	// SourceFields retains JSON member presence and raw values before typed decoding.
	// Multipart requests use their explicit typed fields and staged parts instead.
	SourceFields     map[string]json.RawMessage
	sourceDocument   oif.Document
	SourceParts      []staging.Field // original accepted multipart member order
	SourceNormalized bool            // caller multipart text or metadata was normalized

	// Video job operations.
	JobID           string
	Variant         string
	DeleteMissingOK bool
}

// SourceDocument is the immutable complete native JSON source, including member
// order and exact numeric spellings. Multipart inputs use staged Parts instead.
func (r *Request) SourceDocument() oif.Document {
	if r == nil {
		return oif.Document{}
	}
	return r.sourceDocument
}

// Uploads returns the staged handles this request owns.
func (r *Request) Uploads() []staging.Handle {
	var handles []staging.Handle
	appendParts := func(parts []staging.Part) {
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
		// Typed nil pointers are absent options, not explicit nulls.
		if v := reflect.ValueOf(value); v.Kind() == reflect.Pointer && v.IsNil() {
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
func textField(fields []staging.Field, name string, value *string) []staging.Field {
	if value == nil {
		return fields
	}
	return append(fields, staging.Field{Name: name, Value: staging.Text(*value)})
}

func textValue(fields []staging.Field, name, value string) []staging.Field {
	return append(fields, staging.Field{Name: name, Value: staging.Text(value)})
}

// extraFields renders client extension fields as upstream multipart text
// fields: arrays repeat as name[] and non-strings are compact JSON.
func extraFields(fields []staging.Field, extra map[string]any) []staging.Field {
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
				fields = append(fields, staging.Field{Name: field, Value: staging.Text(text)})
			}
			continue
		}
		text := extraText(value)
		fields = append(fields, staging.Field{Name: name, Value: staging.Text(text)})
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
	var encodedWire struct {
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
	extra, err := decodeJSON(body, &encodedWire, "model", "prompt", "n", "size", "stream", "quality",
		"response_format", "style", "user", "background", "moderation", "output_compression",
		"output_format", "partial_images")
	if err != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	if strings.TrimSpace(encodedWire.Prompt) == "" {
		return nil, invalidMedia("The image prompt is required.")
	}
	if encodedWire.N != nil && *encodedWire.N == 0 {
		return nil, invalidMedia("The image count must be at least 1.")
	}
	if !validRouteSlug(encodedWire.Model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	document, source, sourceErr := sourceMediaFields(body)
	if sourceErr != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	return &Request{
		SourceFields: source, sourceDocument: document,
		Op: OpImageGeneration, Route: encodedWire.Model, Stream: encodedWire.Stream,
		Prompt: encodedWire.Prompt, Count: encodedWire.N, Size: encodedWire.Size, Quality: encodedWire.Quality,
		Format: encodedWire.ResponseFormat, Style: encodedWire.Style, User: encodedWire.User,
		Background: encodedWire.Background, Moderation: encodedWire.Moderation,
		OutputCompression: encodedWire.OutputCompression, OutputFormat: encodedWire.OutputFormat,
		PartialImages: encodedWire.PartialImages, Extra: extra,
	}, nil
}

// DecodeSpeech validates a speech JSON body.
func DecodeSpeech(body []byte) (*Request, *Error) {
	var encodedWire struct {
		Model          string   `json:"model"`
		Input          string   `json:"input"`
		Voice          string   `json:"voice"`
		ResponseFormat *string  `json:"response_format"`
		Instructions   *string  `json:"instructions"`
		Speed          *float64 `json:"speed"`
		StreamFormat   *string  `json:"stream_format"`
	}
	extra, err := decodeJSON(body, &encodedWire, "model", "input", "voice", "response_format",
		"instructions", "speed", "stream_format")
	if err != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	if encodedWire.Input == "" {
		return nil, invalidMedia("The speech input is required.")
	}
	if encodedWire.Voice == "" {
		return nil, invalidMedia("The speech voice is required.")
	}
	if encodedWire.Speed != nil && (*encodedWire.Speed < 0.25 || *encodedWire.Speed > 4.0) {
		return nil, invalidMedia("The speech speed must be between 0.25 and 4.0.")
	}
	if !validRouteSlug(encodedWire.Model) {
		return nil, invalidMedia("The model field must name a route.")
	}
	document, source, sourceErr := sourceMediaFields(body)
	if sourceErr != nil {
		return nil, invalidMedia("The request body is not valid JSON.")
	}
	return &Request{
		SourceFields: source, sourceDocument: document,
		Op: OpSpeech, Route: encodedWire.Model, Stream: encodedWire.StreamFormat != nil && *encodedWire.StreamFormat == "sse",
		Input: encodedWire.Input, Voice: encodedWire.Voice, Format: encodedWire.ResponseFormat,
		Instructions: encodedWire.Instructions, Speed: encodedWire.Speed,
		StreamFormat: encodedWire.StreamFormat, Extra: extra,
	}, nil
}

// decodeJSON unmarshals body into wire and returns the remaining top-level
// fields as extension values. Duplicate or colliding keys are rejected by the
// encoder at dispatch time.
func decodeJSON(body []byte, encodedWire any, known ...string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(encodedWire); err != nil {
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
	// Numbers stay json.Number so extension fields re-encode upstream with
	// the client's exact spelling.
	return doc, nil
}

// DecodeImageEdit validates a parsed image-edit form.
func DecodeImageEdit(form *staging.Form) (*Request, *Error) {
	images := form.TakeFilesWithPrefix("image")
	mask := form.TakeSingleFile("mask")
	model := form.Required("model")
	prompt := form.Required("prompt")
	n := form.OptionalInt("n")
	size := form.Optional("size")
	stream := form.OptionalBool("stream")
	quality := form.Optional("quality")
	responseFormat := form.Optional("response_format")
	user := form.Optional("user")
	background := form.Optional("background")
	inputFidelity := form.Optional("input_fidelity")
	outputCompression := form.OptionalInt("output_compression")
	outputFormat := form.Optional("output_format")
	partialImages := form.OptionalInt("partial_images")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
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
	sourceParts, sourceNormalized := form.SourceFields()
	return &Request{
		Op: OpImageEdit, Route: model, Stream: stream != nil && *stream,
		SourceParts: sourceParts, SourceNormalized: sourceNormalized,
		Prompt: prompt, Count: count64, Size: size, Quality: quality,
		Format: responseFormat, User: user, Background: background,
		InputFidelity: inputFidelity, OutputCompression: int64Ptr(outputCompression),
		OutputFormat: outputFormat, PartialImages: int64Ptr(partialImages),
		Images: images, Mask: mask, Extra: extraAny,
	}, nil
}

// DecodeImageVariation validates a parsed image-variation form.
func DecodeImageVariation(form *staging.Form) (*Request, *Error) {
	image := form.TakeSingleFile("image")
	model := form.Required("model")
	n := form.OptionalInt("n")
	size := form.Optional("size")
	responseFormat := form.Optional("response_format")
	user := form.Optional("user")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
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
	sourceParts, sourceNormalized := form.SourceFields()
	return &Request{
		Op: OpImageVariation, Route: model,
		SourceParts: sourceParts, SourceNormalized: sourceNormalized,
		Count: int64Ptr(n), Size: size, Format: responseFormat, User: user,
		Image: image, Extra: extraAny,
	}, nil
}

// DecodeTranscription validates a parsed transcription form.
func DecodeTranscription(form *staging.Form) (*Request, *Error) {
	file := form.TakeSingleFile("file")
	responseFormat := form.Optional("response_format")
	names := form.TakeRepeated("known_speaker_names")
	references := form.TakeRepeated("known_speaker_references")
	model := form.Required("model")
	language := form.Optional("language")
	prompt := form.Optional("prompt")
	temperature := form.OptionalFloat("temperature")
	include := form.TakeRepeated("include")
	granularities := form.TakeRepeated("timestamp_granularities")
	chunking := form.Optional("chunking_strategy")
	stream := form.OptionalBool("stream")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
	}
	if file == nil {
		return nil, invalidMedia("The audio file is required.")
	}
	if temperature != nil && (math.IsNaN(*temperature) || *temperature < 0 || *temperature > 1) {
		return nil, invalidMedia("The transcription temperature must be between 0 and 1.")
	}
	format := "json"
	if responseFormat != nil {
		format = *responseFormat
	}
	if !wire.ValidTranscriptionResponseFormat(format) {
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
	sourceParts, sourceNormalized := form.SourceFields()
	return &Request{
		Op: OpTranscription, Route: model, Stream: stream != nil && *stream,
		SourceParts: sourceParts, SourceNormalized: sourceNormalized,
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
func DecodeVideoCreate(form *staging.Form) (*Request, *Error) {
	model := form.Required("model")
	prompt := form.Required("prompt")
	inputRef := form.TakeSingleFile("input_reference")
	seconds := form.Optional("seconds")
	size := form.Optional("size")
	extra := form.TakeExtensions()
	if err := form.Err(); err != nil {
		return nil, invalidMedia(err.Error())
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
	sourceParts, sourceNormalized := form.SourceFields()
	return &Request{
		Op: OpVideoCreate, Route: model, Prompt: prompt,
		SourceParts: sourceParts, SourceNormalized: sourceNormalized,
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
	for name, values := range query {
		if name != "after" && name != "limit" && name != "order" || len(values) != 1 {
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
	for name, values := range query {
		if name != "variant" || len(values) != 1 {
			return "", invalidMedia("Video content contains unsupported query parameters.")
		}
	}
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
	case OpTranslation:
		return encodeTranslation(r, upstreamModel)
	case OpVideoCreate:
		return encodeVideoCreate(r, upstreamModel)
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
	fields := []staging.Field{}
	fields = textValue(fields, "model", model)
	fields = textValue(fields, "prompt", r.Prompt)
	if len(r.Images) == 1 {
		fields = append(fields, staging.Field{Name: "image", Value: r.Images[0]})
	} else {
		for i := range r.Images {
			fields = append(fields, staging.Field{Name: "image[" + strconv.Itoa(i) + "]", Value: r.Images[i]})
		}
	}
	if r.Mask != nil {
		fields = append(fields, staging.Field{Name: "mask", Value: *r.Mask})
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
	fields := []staging.Field{}
	fields = textValue(fields, "model", model)
	fields = append(fields, staging.Field{Name: "image", Value: *r.Image})
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
	fields := []staging.Field{}
	fields = textValue(fields, "model", model)
	fields = append(fields, staging.Field{Name: "file", Value: *r.File})
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
		fields = append(fields, staging.Field{Name: "chunking_strategy", Value: staging.Text(r.ChunkingStrategy)})
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
	fields := []staging.Field{}
	fields = textValue(fields, "model", model)
	fields = textValue(fields, "prompt", r.Prompt)
	fields = textField(fields, "seconds", r.Seconds)
	fields = textField(fields, "size", r.Size)
	if r.InputRef != nil {
		fields = append(fields, staging.Field{Name: "input_reference", Value: *r.InputRef})
	}
	fields = extraFields(fields, r.Extra)
	return &UpstreamCall{Method: "POST", Path: "videos", Accept: "application/json",
		Fields: fields, Kind: ResponseVideoJob, Ambiguous: true}, nil
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

// bedrockStabilityImageModels are the Stability text-to-image models Bedrock
// serves through InvokeModel, each returning one image per call.
var bedrockStabilityImageModels = []string{"stability.sd3-5-large-", "stability.stable-image-core-", "stability.stable-image-ultra-"}

func encodeBedrockImage(r *Request, model string) (*UpstreamCall, *Error) {
	if slices.ContainsFunc(bedrockStabilityImageModels, func(prefix string) bool { return strings.HasPrefix(model, prefix) }) {
		return encodeBedrockStabilityImage(r, model)
	}
	if !strings.HasPrefix(model, "amazon.titan-image-generator-") {
		return nil, invalidMedia("The configured model is not a qualified Bedrock Titan or Stability image model.")
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

// encodeBedrockStabilityImage requests one image from a Stability model on
// Bedrock, at the aspect ratio of the requested size.
//
// https://docs.aws.amazon.com/bedrock/latest/userguide/model-parameters-diffusion-3-5-large.html
func encodeBedrockStabilityImage(r *Request, model string) (*UpstreamCall, *Error) {
	generic := *r
	generic.OutputFormat = nil
	if err := rejectNativeImageFields(&generic); err != nil {
		return nil, err
	}
	if r.Count != nil && *r.Count != 1 {
		return nil, invalidMedia("Stability models on Bedrock generate one image per request.")
	}
	fields := map[string]any{"prompt": r.Prompt}
	if r.Size != nil {
		aspect, ok := map[string]string{"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3"}[*r.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by the Bedrock Stability target.")
		}
		fields["aspect_ratio"] = aspect
	}
	if r.OutputFormat != nil {
		if !slices.Contains([]string{"png", "jpeg", "webp"}, *r.OutputFormat) {
			return nil, invalidMedia("The output format is not supported by the Bedrock Stability target.")
		}
		fields["output_format"] = *r.OutputFormat
	}
	body, err := json.Marshal(fields)
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
