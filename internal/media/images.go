package media

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

type ImageGeneration struct {
	Prompt            string  `json:"prompt"`
	Count             *int64  `json:"n"`
	Size              *string `json:"size"`
	Stream            bool    `json:"stream"`
	Quality           *string `json:"quality"`
	Format            *string `json:"response_format"`
	Style             *string `json:"style"`
	User              *string `json:"user"`
	Background        *string `json:"background"`
	Moderation        *string `json:"moderation"`
	OutputCompression *int64  `json:"output_compression"`
	OutputFormat      *string `json:"output_format"`
	PartialImages     *int64  `json:"partial_images"`
}

type ImageEdit struct {
	Images            []staging.Part
	Mask              *staging.Part
	Prompt            string
	Count             *int64
	Size              *string
	Stream            bool
	Quality           *string
	Format            *string
	User              *string
	Background        *string
	InputFidelity     *string
	OutputCompression *int64
	OutputFormat      *string
	PartialImages     *int64
}

type ImageVariation struct {
	Image  staging.Part
	Count  *int64
	Size   *string
	Format *string
	User   *string
}

func (ImageGeneration) Operation() Operation { return OpImageGeneration }
func (ImageEdit) Operation() Operation       { return OpImageEdit }
func (ImageVariation) Operation() Operation  { return OpImageVariation }

func (ImageGeneration) uploads() []staging.Part { return nil }
func (p ImageEdit) uploads() []staging.Part {
	if p.Mask == nil {
		return p.Images
	}
	return append(slices.Clone(p.Images), *p.Mask)
}
func (p ImageVariation) uploads() []staging.Part { return []staging.Part{p.Image} }

func (p ImageGeneration) streaming() bool { return p.Stream }
func (p ImageEdit) streaming() bool       { return p.Stream }
func (ImageVariation) streaming() bool    { return false }

func DecodeImageGeneration(body []byte) (*Request, *Error) {
	var p ImageGeneration
	route, source, extra, failure := decodeJSON(body, &p)
	switch {
	case failure != nil:
		return nil, failure
	case strings.TrimSpace(p.Prompt) == "":
		return nil, invalidMedia("The image prompt is required.")
	case p.Count != nil && *p.Count == 0:
		return nil, invalidMedia("The image count must be at least 1.")
	}
	return newRequest(route, p, extra, source)
}

func DecodeImageEdit(form *staging.Form) (*Request, *Error) {
	images := form.TakeFilesWithPrefix("image")
	mask := form.TakeSingleFile("mask")
	route := form.Required("model")
	p := ImageEdit{
		Images: images, Mask: mask, Prompt: form.Required("prompt"), Count: int64Ptr(form.OptionalInt("n")),
		Size: form.Optional("size"), Stream: valueOf(form.OptionalBool("stream")), Quality: form.Optional("quality"),
		Format: form.Optional("response_format"), User: form.Optional("user"), Background: form.Optional("background"),
		InputFidelity: form.Optional("input_fidelity"), OutputCompression: int64Ptr(form.OptionalInt("output_compression")),
		OutputFormat: form.Optional("output_format"), PartialImages: int64Ptr(form.OptionalInt("partial_images")),
	}
	extra, failure := formExtensions(form)
	switch {
	case failure != nil:
		return nil, failure
	case len(images) == 0:
		return nil, invalidMedia("The image file is required.")
	case strings.TrimSpace(p.Prompt) == "":
		return nil, invalidMedia("The image prompt is required.")
	case p.Count != nil && *p.Count == 0:
		return nil, invalidMedia("The image count must be at least 1.")
	}
	return formRequest(form, route, p, extra)
}

func DecodeImageVariation(form *staging.Form) (*Request, *Error) {
	image := form.TakeSingleFile("image")
	route := form.Required("model")
	count := int64Ptr(form.OptionalInt("n"))
	size, format, user := form.Optional("size"), form.Optional("response_format"), form.Optional("user")
	extra, failure := formExtensions(form)
	switch {
	case failure != nil:
		return nil, failure
	case image == nil:
		return nil, invalidMedia("The image file is required.")
	case count != nil && *count == 0:
		return nil, invalidMedia("The image count must be at least 1.")
	}
	return formRequest(form, route, ImageVariation{Image: *image, Count: count, Size: size, Format: format, User: user}, extra)
}

func encodeImageGeneration(r *Request, p ImageGeneration, cfg connectors.Config, model string) (*UpstreamCall, *Error) {
	switch cfg.Kind {
	case "vertex_ai":
		return encodeVertexImage(r, p, model)
	case "bedrock":
		return encodeBedrockImage(r, p, model)
	}
	fields := map[string]any{
		"model": model, "prompt": p.Prompt, "n": p.Count, "size": p.Size, "quality": p.Quality,
		"response_format": p.Format, "style": p.Style, "user": p.User, "background": p.Background,
		"moderation": p.Moderation, "output_compression": p.OutputCompression, "output_format": p.OutputFormat,
		"partial_images": p.PartialImages,
	}
	if p.Stream {
		fields["stream"] = true
	}
	body, failure := jsonDoc(fields, r.Extra())
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "images/generations"}, Accept: "application/json",
		Body: JSONBody(body), Response: imageReply(p.Stream), Ambiguous: true}, nil
}

func encodeImageEdit(r *Request, p ImageEdit, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = textValue(fields, "prompt", p.Prompt)
	if len(p.Images) == 1 {
		fields = append(fields, staging.Field{Name: "image", Value: p.Images[0]})
	} else {
		for i, image := range p.Images {
			fields = append(fields, staging.Field{Name: "image[" + strconv.Itoa(i) + "]", Value: image})
		}
	}
	if p.Mask != nil {
		fields = append(fields, staging.Field{Name: "mask", Value: *p.Mask})
	}
	fields = textField(fields, "n", int64Text(p.Count))
	fields = textField(fields, "size", p.Size)
	fields = textValue(fields, "stream", strconv.FormatBool(p.Stream))
	fields = textField(fields, "quality", p.Quality)
	fields = textField(fields, "response_format", p.Format)
	fields = textField(fields, "user", p.User)
	fields = textField(fields, "background", p.Background)
	fields = textField(fields, "input_fidelity", p.InputFidelity)
	fields = textField(fields, "output_compression", int64Text(p.OutputCompression))
	fields = textField(fields, "output_format", p.OutputFormat)
	fields = textField(fields, "partial_images", int64Text(p.PartialImages))
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "images/edits"}, Accept: "application/json",
		Body: FormBody(extraFields(fields, r.Extra())), Response: imageReply(p.Stream), Ambiguous: true}, nil
}

func encodeImageVariation(r *Request, p ImageVariation, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := textValue(nil, "model", model)
	fields = append(fields, staging.Field{Name: "image", Value: p.Image})
	fields = textField(fields, "n", int64Text(p.Count))
	fields = textField(fields, "size", p.Size)
	fields = textField(fields, "response_format", p.Format)
	fields = textField(fields, "user", p.User)
	return &UpstreamCall{Method: "POST", Endpoint: Path{Path: "images/variations"}, Accept: "application/json",
		Body: FormBody(extraFields(fields, r.Extra())), Response: ImageReply{}, Ambiguous: true}, nil
}

func imageReply(stream bool) Response {
	if stream {
		return EventReply{}
	}
	return ImageReply{}
}

var squareAspectRatios = map[string]string{"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3"}

func encodeVertexImage(r *Request, p ImageGeneration, model string) (*UpstreamCall, *Error) {
	if !strings.HasPrefix(model, "imagen-") {
		return nil, invalidMedia("The configured model is not a qualified Vertex Imagen model.")
	}
	if err := rejectNativeImageFields(r, p); err != nil {
		return nil, err
	}
	parameters := map[string]any{"sampleCount": imageCount(p.Count)}
	if p.Size != nil {
		aspect, ok := squareAspectRatios[*p.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by the Vertex Imagen target.")
		}
		parameters["aspectRatio"] = aspect
	}
	body, err := json.Marshal(map[string]any{
		"instances":  []map[string]any{{"prompt": p.Prompt}},
		"parameters": parameters,
	})
	if err != nil {
		return nil, invalidMedia("The native image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "models/" + url.PathEscape(model) + ":predict"},
		Body: NativeJSON(body), Response: ImageReply{Decode: vertexImages(imageCount(p.Count))}, Ambiguous: true}, nil
}

var bedrockStabilityImageModels = []string{"stability.sd3-5-large-", "stability.stable-image-core-", "stability.stable-image-ultra-"}

var titanSizes = map[string][2]int{"1024x1024": {1024, 1024}, "768x768": {768, 768}, "512x512": {512, 512}}

func encodeBedrockImage(r *Request, p ImageGeneration, model string) (*UpstreamCall, *Error) {
	if slices.ContainsFunc(bedrockStabilityImageModels, func(prefix string) bool { return strings.HasPrefix(model, prefix) }) {
		return encodeBedrockStabilityImage(r, p, model)
	}
	if !strings.HasPrefix(model, "amazon.titan-image-generator-") {
		return nil, invalidMedia("The configured model is not a qualified Bedrock Titan or Stability image model.")
	}
	if err := rejectNativeImageFields(r, p); err != nil {
		return nil, err
	}
	size, ok := titanSizes[valueOr(p.Size, "1024x1024")]
	if !ok {
		return nil, invalidMedia("The size is not supported by the Bedrock Titan target.")
	}
	body, err := json.Marshal(map[string]any{
		"taskType":          "TEXT_IMAGE",
		"textToImageParams": map[string]any{"text": p.Prompt},
		"imageGenerationConfig": map[string]any{
			"numberOfImages": imageCount(p.Count),
			"width":          size[0],
			"height":         size[1],
		},
	})
	if err != nil {
		return nil, invalidMedia("The native image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "model/" + url.PathEscape(model) + "/invoke"},
		Body: NativeJSON(body), Response: ImageReply{Decode: bedrockImages(imageCount(p.Count))}, Ambiguous: true}, nil
}

func encodeBedrockStabilityImage(r *Request, p ImageGeneration, model string) (*UpstreamCall, *Error) {
	generic := p
	generic.OutputFormat = nil
	if err := rejectNativeImageFields(r, generic); err != nil {
		return nil, err
	}
	if p.Count != nil && *p.Count != 1 {
		return nil, invalidMedia("Stability models on Bedrock generate one image per request.")
	}
	fields := map[string]any{"prompt": p.Prompt}
	if p.Size != nil {
		aspect, ok := squareAspectRatios[*p.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by the Bedrock Stability target.")
		}
		fields["aspect_ratio"] = aspect
	}
	if p.OutputFormat != nil {
		if !slices.Contains([]string{"png", "jpeg", "webp"}, *p.OutputFormat) {
			return nil, invalidMedia("The output format is not supported by the Bedrock Stability target.")
		}
		fields["output_format"] = *p.OutputFormat
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return nil, invalidMedia("The native image request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "model/" + url.PathEscape(model) + "/invoke"},
		Body: NativeJSON(body), Response: ImageReply{Decode: bedrockImages(imageCount(p.Count))}, Ambiguous: true}, nil
}

func rejectNativeImageFields(r *Request, p ImageGeneration) *Error {
	switch {
	case p.Format != nil && *p.Format != "b64_json":
		return invalidMedia("Native image targets only support b64_json responses.")
	case p.Quality != nil, p.Style != nil, p.User != nil, p.Background != nil, p.Moderation != nil,
		p.OutputCompression != nil, p.OutputFormat != nil, p.PartialImages != nil:
		return invalidMedia("The native image target does not support the requested image parameters.")
	case p.Stream:
		return invalidMedia("The native image target does not support streaming.")
	case p.Count != nil && (*p.Count < 1 || *p.Count > 4):
		return invalidMedia("Native image targets support n from 1 to 4.")
	case len(r.Extra()) > 0:
		return invalidMedia("The native image target does not support extension fields.")
	}
	return nil
}
