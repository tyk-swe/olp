package media

import (
	"net/http"
	"time"
)

// The xai-images wire is xAI's image generation API, which takes an aspect
// ratio in place of a size and returns OpenAI's image result.
//
// https://docs.x.ai/developers/rest-api-reference/inference/images
func init() {
	registerCodec("xai-images", codec{
		OpImageGeneration: modelOnly(encodeXAIImage),
	})
}

// imageAspectRatios are the aspect ratios of OpenAI's image sizes.
var imageAspectRatios = map[string]string{
	"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3",
	"1792x1024": "16:9", "1024x1792": "9:16", "auto": "auto",
}

func encodeXAIImage(r *Request, model string) (*UpstreamCall, *Error) {
	switch {
	case r.Quality != nil, r.Style != nil, r.Background != nil, r.Moderation != nil, r.OutputCompression != nil,
		r.OutputFormat != nil, r.PartialImages != nil, r.Stream, len(r.Extra) > 0:
		return nil, invalidMedia("xAI image generation does not support the requested image parameters.")
	case r.Count != nil && (*r.Count < 1 || *r.Count > 10):
		return nil, invalidMedia("xAI generates 1 to 10 images per request.")
	case r.Format != nil && *r.Format != "url" && *r.Format != "b64_json":
		return nil, invalidMedia("xAI returns images as url or b64_json.")
	}
	fields := map[string]any{"model": model, "prompt": r.Prompt, "n": r.Count, "response_format": r.Format, "user": r.User}
	if r.Size != nil {
		ratio, ok := imageAspectRatios[*r.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by xAI image generation.")
		}
		fields["aspect_ratio"] = ratio
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodPost, Path: "images/generations", JSON: body, Kind: ResponseImages, Ambiguous: true,
		DecodeImages: decodeXAIImages}, nil
}

// decodeXAIImages reads xAI's result, which names no creation time.
func decodeXAIImages(body []byte, stage func(string, int) (*Artifact, *Error)) (*ImageResult, *Error) {
	result, failure := DecodeImageResponse(body, stage)
	if failure == nil && result.CreatedAt == 0 {
		result.CreatedAt = time.Now().Unix()
	}
	return result, failure
}
