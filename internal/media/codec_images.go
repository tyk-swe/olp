package media

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The stability and recraft wires are Stability AI's and Recraft's image
// APIs. Both take an aspect ratio in place of a size.
//
// https://platform.stability.ai/docs/api-reference
// https://www.recraft.ai/docs/api-reference/endpoints
func init() {
	registerCodec("stability", codec{
		OpImageGeneration: modelOnly(encodeStabilityImage),
		OpImageEdit:       modelOnly(encodeStabilityEdit),
	})
	registerCodec("recraft", codec{
		OpImageGeneration: modelOnly(encodeRecraftImage),
	})
}

// refuseImageControls refuses the OpenAI image controls a vendor has no
// counterpart for, and every response format but those it returns.
func refuseImageControls(r *Request, vendor string, formats ...string) *Error {
	switch {
	case r.Quality != nil, r.Style != nil, r.Background != nil, r.Moderation != nil, r.OutputCompression != nil, r.InputFidelity != nil,
		r.PartialImages != nil, r.Stream, r.User != nil, len(r.Extra) > 0:
		return invalidMedia(vendor + " does not support the requested image parameters.")
	case r.Format != nil && !slices.Contains(formats, *r.Format):
		return invalidMedia(vendor + " returns images as " + strings.Join(formats, " or ") + ".")
	}
	return nil
}

// imageAspectRatio is the aspect ratio of an OpenAI image size, if any.
func imageAspectRatio(r *Request, vendor string) (string, *Error) {
	if r.Size == nil || *r.Size == "auto" {
		return "", nil
	}
	ratio, ok := imageAspectRatios[*r.Size]
	if !ok {
		return "", invalidMedia("The size is not supported by " + vendor + ".")
	}
	return ratio, nil
}

// stabilityGenerations are the Stability generation services and the models
// each serves; Stable Diffusion 3.5 names its model in the request.
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

func stabilityFields(r *Request) ([]Field, *Error) {
	if failure := refuseImageControls(r, "Stability", "b64_json"); failure != nil {
		return nil, failure
	}
	if r.Count != nil && *r.Count != 1 {
		return nil, invalidMedia("Stability generates one image per request.")
	}
	if r.OutputFormat != nil && !slices.Contains([]string{"png", "jpeg", "webp"}, *r.OutputFormat) {
		return nil, invalidMedia("Stability returns png, jpeg or webp images.")
	}
	fields := textValue(nil, "prompt", r.Prompt)
	return textField(fields, "output_format", r.OutputFormat), nil
}

func encodeStabilityImage(r *Request, model string) (*UpstreamCall, *Error) {
	service, named := stabilityService(model)
	if service == "" {
		return nil, invalidMedia("The model is not a Stability generation service: stable-image-ultra, stable-image-core or an sd3.5 model.")
	}
	fields, failure := stabilityFields(r)
	if failure != nil {
		return nil, failure
	}
	ratio, failure := imageAspectRatio(r, "Stability")
	if failure != nil {
		return nil, failure
	}
	if ratio != "" {
		fields = textValue(fields, "aspect_ratio", ratio)
	}
	if named {
		fields = textValue(fields, "model", model)
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "v2beta/stable-image/" + service, Accept: "application/json", Fields: fields,
		Kind: ResponseImages, Ambiguous: true, DecodeImages: decodeStabilityImage}, nil
}

// encodeStabilityEdit inpaints one image, under its mask or its transparent
// areas, as OpenAI's edits do.
func encodeStabilityEdit(r *Request, model string) (*UpstreamCall, *Error) {
	if model != "stable-image-inpaint" {
		return nil, invalidMedia("Stability edits images with stable-image-inpaint.")
	}
	if len(r.Images) != 1 || r.Size != nil {
		return nil, invalidMedia("Stability inpaints one image at its own size.")
	}
	fields, failure := stabilityFields(r)
	if failure != nil {
		return nil, failure
	}
	fields = append(fields, Field{Name: "image", File: &r.Images[0]})
	if r.Mask != nil {
		fields = append(fields, Field{Name: "mask", File: r.Mask})
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "v2beta/stable-image/edit/inpaint", Accept: "application/json", Fields: fields,
		Kind: ResponseImages, Ambiguous: true, DecodeImages: decodeStabilityImage}, nil
}

// decodeStabilityImage reads Stability's one image, which its content filter
// may withhold.
func decodeStabilityImage(body []byte, stage func(string, int) (*Artifact, *Error)) (*ImageResult, *Error) {
	var wire struct {
		Image        string `json:"image"`
		FinishReason string `json:"finish_reason"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return nil, protocolError("The provider image response is not valid JSON.")
	}
	switch {
	case wire.FinishReason == "CONTENT_FILTERED":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
	case wire.FinishReason != "SUCCESS" || wire.Image == "":
		return nil, protocolError("The provider image response carries no image.")
	}
	staged, failure := stage(wire.Image, 0)
	if failure != nil {
		return nil, failure
	}
	handle := staged.Handle
	return &ImageResult{CreatedAt: time.Now().Unix(), Images: []ImageArtifact{{Handle: &handle}}}, nil
}

func encodeRecraftImage(r *Request, model string) (*UpstreamCall, *Error) {
	if failure := refuseImageControls(r, "Recraft", "url", "b64_json"); failure != nil {
		return nil, failure
	}
	if r.Count != nil && (*r.Count < 1 || *r.Count > 6) {
		return nil, invalidMedia("Recraft generates 1 to 6 images per request.")
	}
	if r.OutputFormat != nil && *r.OutputFormat != "png" && *r.OutputFormat != "webp" {
		return nil, invalidMedia("Recraft returns png or webp images.")
	}
	ratio, failure := imageAspectRatio(r, "Recraft")
	if failure != nil {
		return nil, failure
	}
	fields := map[string]any{"prompt": r.Prompt, "model": model, "n": r.Count, "response_format": r.Format, "image_format": r.OutputFormat}
	if ratio != "" {
		fields["size"] = ratio
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	// The raster path refuses vector models, whose SVG is no OpenAI image.
	return &UpstreamCall{Method: http.MethodPost, Path: "images/generations/raster", JSON: body, Kind: ResponseImages, Ambiguous: true}, nil
}

// The bfl wire is Black Forest Labs' FLUX API, which works asynchronously:
// a submission answers with a polling URL, the poll reports the work's
// status, and a ready result names a signed image URL, which expires in
// minutes and takes no credential.
//
// https://docs.bfl.ai/api_integration/integration_guidelines
func init() {
	registerCodec("bfl", codec{
		OpImageGeneration: modelOnly(encodeBFLImage),
	})
}

// bflPoll is how long the work waits between polls.
const bflPoll = time.Second

func encodeBFLImage(r *Request, model string) (*UpstreamCall, *Error) {
	if failure := refuseImageControls(r, "Black Forest Labs", "b64_json"); failure != nil {
		return nil, failure
	}
	if r.Count != nil && *r.Count != 1 {
		return nil, invalidMedia("Black Forest Labs generates one image per request.")
	}
	if r.OutputFormat != nil && *r.OutputFormat != "png" && *r.OutputFormat != "jpeg" {
		return nil, invalidMedia("Black Forest Labs returns png or jpeg images.")
	}
	fields := map[string]any{"prompt": r.Prompt, "output_format": r.OutputFormat}
	if r.Size != nil && *r.Size != "auto" {
		var width, height int
		if _, err := fmt.Sscanf(*r.Size, "%dx%d", &width, &height); err != nil || width < 64 || height < 64 {
			return nil, invalidMedia("The size is not supported by Black Forest Labs.")
		}
		fields["width"], fields["height"] = width, height
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodPost, Path: url.PathEscape(model), Accept: "application/json", JSON: body, Kind: ResponseImages, Ambiguous: true,
		Next: nextBFLStep, StepDomains: []string{"bfl.ai"}}, nil
}

// nextBFLStep follows a submission to its polling URL, polls until the work
// is ready, and then fetches the image.
func nextBFLStep(body []byte) (*Step, *Error) {
	var wire struct {
		PollingURL string `json:"polling_url"`
		Status     string `json:"status"`
		Result     struct {
			Sample string `json:"sample"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return nil, protocolError("The provider's image work answered with invalid JSON.")
	}
	switch {
	case wire.PollingURL != "" && wire.Status == "":
		return &Step{URL: wire.PollingURL, Wait: bflPoll, Credentials: true}, nil
	case wire.Status == "Pending" || wire.Status == "Reasoning" || wire.Status == "Generating":
		return nil, nil
	case wire.Status == "Ready" && wire.Result.Sample != "":
		return &Step{URL: wire.Result.Sample, Asset: "image/"}, nil
	case wire.Status == "Request Moderated" || wire.Status == "Content Moderated":
		return nil, Fail(http.StatusBadRequest, "content_filter", "The provider's content filter refused the image request.")
	}
	return nil, protocolError("The provider's image work failed.")
}
