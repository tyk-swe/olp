package media

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

var stabilityCodec = codec{imageGeneration: encodeStabilityImage, imageEdit: encodeStabilityEdit}

var recraftCodec = codec{imageGeneration: encodeRecraftImage}

var bflCodec = codec{imageGeneration: encodeBFLImage}

type imageView struct {
	prompt                     string
	count                      *int64
	size, format, outputFormat *string
	extended                   bool
}

func generationView(r *Request, p ImageGeneration) imageView {
	extended := p.Quality != nil || p.Style != nil || p.Background != nil || p.Moderation != nil || p.OutputCompression != nil ||
		p.PartialImages != nil || p.Stream || p.User != nil || len(r.Extra()) > 0
	return imageView{p.Prompt, p.Count, p.Size, p.Format, p.OutputFormat, extended}
}

func editView(r *Request, p ImageEdit) imageView {
	extended := p.Quality != nil || p.Background != nil || p.OutputCompression != nil || p.InputFidelity != nil ||
		p.PartialImages != nil || p.Stream || p.User != nil || len(r.Extra()) > 0
	return imageView{p.Prompt, p.Count, p.Size, p.Format, p.OutputFormat, extended}
}

func (v imageView) refuse(vendor string, formats ...string) *Error {
	switch {
	case v.extended:
		return invalidMedia(vendor + " does not support the requested image parameters.")
	case v.format != nil && !slices.Contains(formats, *v.format):
		return invalidMedia(vendor + " returns images as " + strings.Join(formats, " or ") + ".")
	}
	return nil
}

func imageAspectRatio(size *string, vendor string) (string, *Error) {
	if size == nil || *size == "auto" {
		return "", nil
	}
	ratio, ok := imageAspectRatios[*size]
	if !ok {
		return "", invalidMedia("The size is not supported by " + vendor + ".")
	}
	return ratio, nil
}

func stabilityService(model string) (string, bool) {
	switch {
	case model == "stable-image-ultra":
		return "generate/ultra", false
	case model == "stable-image-core":
		return "generate/core", false
	case strings.HasPrefix(model, "sd3.5-"):
		return "generate/sd3", true
	}
	return "", false
}

func stabilityFields(v imageView) ([]staging.Field, *Error) {
	if failure := v.refuse("Stability", "b64_json"); failure != nil {
		return nil, failure
	}
	if imageCount(v.count) != 1 {
		return nil, invalidMedia("Stability generates one image per request.")
	}
	if v.outputFormat != nil && !slices.Contains([]string{"png", "jpeg", "webp"}, *v.outputFormat) {
		return nil, invalidMedia("Stability returns png, jpeg or webp images.")
	}
	return textField(textValue(nil, "prompt", v.prompt), "output_format", v.outputFormat), nil
}

func encodeStabilityImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	service, named := stabilityService(model)
	if service == "" {
		return nil, invalidMedia("The model is not a Stability generation service: stable-image-ultra, stable-image-core or an sd3.5 model.")
	}
	fields, failure := stabilityFields(generationView(r, p))
	if failure != nil {
		return nil, failure
	}
	ratio, failure := imageAspectRatio(p.Size, "Stability")
	if failure != nil {
		return nil, failure
	}
	if ratio != "" {
		fields = textValue(fields, "aspect_ratio", ratio)
	}
	if named {
		fields = textValue(fields, "model", model)
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "v2beta/stable-image/" + service}, Accept: "application/json", Body: FormBody(fields),
		Response: ImageReply{Decode: decodeStabilityImage}, Ambiguous: true}, nil
}

func encodeStabilityEdit(r *Request, p ImageEdit, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if model != "stable-image-inpaint" {
		return nil, invalidMedia("Stability edits images with stable-image-inpaint.")
	}
	if len(p.Images) != 1 || p.Size != nil {
		return nil, invalidMedia("Stability inpaints one image at its own size.")
	}
	fields, failure := stabilityFields(editView(r, p))
	if failure != nil {
		return nil, failure
	}
	fields = append(fields, staging.Field{Name: "image", Value: p.Images[0]})
	if p.Mask != nil {
		fields = append(fields, staging.Field{Name: "mask", Value: *p.Mask})
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "v2beta/stable-image/edit/inpaint"}, Accept: "application/json", Body: FormBody(fields),
		Response: ImageReply{Decode: decodeStabilityImage}, Ambiguous: true}, nil
}

func decodeStabilityImage(body []byte, stage StageImage) (*ImageResult, *Error) {
	var encodedWire struct {
		Image        string `json:"image"`
		FinishReason string `json:"finish_reason"`
	}
	if json.Unmarshal(body, &encodedWire) != nil {
		return nil, ProtocolError("The provider image response is not valid JSON.")
	}
	switch {
	case encodedWire.FinishReason == "CONTENT_FILTERED":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
	case encodedWire.FinishReason != "SUCCESS" || encodedWire.Image == "":
		return nil, ProtocolError("The provider image response carries no image.")
	}
	staged, failure := stage(encodedWire.Image, 0)
	if failure != nil {
		return nil, failure
	}
	handle := staged.Handle
	return &ImageResult{CreatedAt: time.Now().Unix(), Images: []ImageArtifact{{Handle: &handle}}}, nil
}

func encodeRecraftImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := generationView(r, p).refuse("Recraft", "url", "b64_json"); failure != nil {
		return nil, failure
	}
	if p.Count != nil && (*p.Count < 1 || *p.Count > 6) {
		return nil, invalidMedia("Recraft generates 1 to 6 images per request.")
	}
	if p.OutputFormat != nil && *p.OutputFormat != "png" && *p.OutputFormat != "webp" {
		return nil, invalidMedia("Recraft returns png or webp images.")
	}
	ratio, failure := imageAspectRatio(p.Size, "Recraft")
	if failure != nil {
		return nil, failure
	}
	fields := map[string]any{"prompt": p.Prompt, "model": model, "n": p.Count, "response_format": p.Format, "image_format": p.OutputFormat}
	if ratio != "" {
		fields["size"] = ratio
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "images/generations/raster"}, Body: JSONBody(body), Response: ImageReply{}, Ambiguous: true}, nil
}

const bflPoll = time.Second

func encodeBFLImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	if failure := generationView(r, p).refuse("Black Forest Labs", "b64_json"); failure != nil {
		return nil, failure
	}
	if imageCount(p.Count) != 1 {
		return nil, invalidMedia("Black Forest Labs generates one image per request.")
	}
	if p.OutputFormat != nil && *p.OutputFormat != "png" && *p.OutputFormat != "jpeg" {
		return nil, invalidMedia("Black Forest Labs returns png or jpeg images.")
	}
	fields := map[string]any{"prompt": p.Prompt, "output_format": p.OutputFormat}
	if p.Size != nil && *p.Size != "auto" {
		var width, height int
		if _, err := fmt.Sscanf(*p.Size, "%dx%d", &width, &height); err != nil || width < 64 || height < 64 {
			return nil, invalidMedia("The size is not supported by Black Forest Labs.")
		}
		fields["width"], fields["height"] = width, height
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: url.PathEscape(model)}, Accept: "application/json", Body: JSONBody(body),
		Response: ImageReply{Poll: &Poll{Next: nextBFLStep, Domains: []string{"bfl.ai"}}}, Ambiguous: true}, nil
}

func nextBFLStep(body []byte) (Step, *Error) {
	var encodedWire struct {
		PollingURL string `json:"polling_url"`
		Status     string `json:"status"`
		Result     struct {
			Sample string `json:"sample"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &encodedWire) != nil {
		return nil, ProtocolError("The provider's image work answered with invalid JSON.")
	}
	switch {
	case encodedWire.PollingURL != "" && encodedWire.Status == "":
		return Visit{URL: encodedWire.PollingURL, Wait: bflPoll, Credentials: true}, nil
	case encodedWire.Status == "Pending" || encodedWire.Status == "Reasoning" || encodedWire.Status == "Generating":
		return Again{}, nil
	case encodedWire.Status == "Ready" && encodedWire.Result.Sample != "":
		return Fetch{URL: encodedWire.Result.Sample, Prefix: "image/"}, nil
	case encodedWire.Status == "Request Moderated" || encodedWire.Status == "Content Moderated":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
	}
	return nil, ProtocolError("The provider's image work failed.")
}
