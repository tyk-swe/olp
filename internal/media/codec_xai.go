package media

import (
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
)

var xaiImagesCodec = codec{imageGeneration: encodeXAIImage}

var imageAspectRatios = map[string]string{
	"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3",
	"1792x1024": "16:9", "1024x1792": "9:16", "auto": "auto",
}

func encodeXAIImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	switch {
	case p.Quality != nil, p.Style != nil, p.Background != nil, p.Moderation != nil, p.OutputCompression != nil,
		p.OutputFormat != nil, p.PartialImages != nil, p.Stream, len(r.Extra()) > 0:
		return nil, invalidMedia("xAI image generation does not support the requested image parameters.")
	case p.Count != nil && (*p.Count < 1 || *p.Count > 10):
		return nil, invalidMedia("xAI generates 1 to 10 images per request.")
	case p.Format != nil && *p.Format != "url" && *p.Format != "b64_json":
		return nil, invalidMedia("xAI returns images as url or b64_json.")
	}
	fields := map[string]any{"model": model, "prompt": p.Prompt, "n": p.Count, "response_format": p.Format, "user": p.User}
	if p.Size != nil {
		ratio, ok := imageAspectRatios[*p.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by xAI image generation.")
		}
		fields["aspect_ratio"] = ratio
	}
	body, failure := jsonDoc(fields, nil)
	if failure != nil {
		return nil, failure
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "images/generations"}, Body: JSONBody(body),
		Response: ImageReply{Decode: decodeXAIImages}, Ambiguous: true}, nil
}

func decodeXAIImages(body []byte, stage StageImage) (*ImageResult, *Error) {
	result, failure := DecodeImageResponse(body, stage)
	if failure == nil && result.CreatedAt == 0 {
		result.CreatedAt = time.Now().Unix()
	}
	return result, failure
}
