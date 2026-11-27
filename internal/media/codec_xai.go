package media

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
)

var xaiImagesCodec = codec{imageGeneration: encodeXAIImage}

var imageAspectRatios = map[string]string{
	"1024x1024": "1:1", "1536x1024": "3:2", "1024x1536": "2:3",
	"1792x1024": "16:9", "1024x1792": "9:16", "auto": "auto",
}

type xaiImageRequest struct {
	Model       string  `json:"model"`
	Prompt      string  `json:"prompt"`
	Count       *int64  `json:"n,omitempty"`
	Format      *string `json:"response_format,omitempty"`
	User        *string `json:"user,omitempty"`
	AspectRatio string  `json:"aspect_ratio,omitempty"`
}

func encodeXAIImage(r *Request, p ImageGeneration, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	switch {
	case p.Quality != nil, p.Style != nil, p.Background != nil, p.Moderation != nil, p.OutputCompression != nil,
		p.OutputFormat != nil, p.PartialImages != nil, p.Stream, r.hasExtra():
		return nil, invalidMedia("xAI image generation does not support the requested image parameters.")
	case p.Count != nil && (*p.Count < 1 || *p.Count > 10):
		return nil, invalidMedia("xAI generates 1 to 10 images per request.")
	case p.Format != nil && *p.Format != "url" && *p.Format != "b64_json":
		return nil, invalidMedia("xAI returns images as url or b64_json.")
	}
	var ratio string
	if p.Size != nil {
		var ok bool
		ratio, ok = imageAspectRatios[*p.Size]
		if !ok {
			return nil, invalidMedia("The size is not supported by xAI image generation.")
		}
	}
	body, err := json.Marshal(xaiImageRequest{
		Model:       model,
		Prompt:      p.Prompt,
		Count:       p.Count,
		Format:      p.Format,
		User:        p.User,
		AspectRatio: ratio,
	})
	if err != nil {
		return nil, invalidMedia("The media request could not be encoded.")
	}
	return &UpstreamCall{Method: http.MethodPost, Endpoint: Path{Path: "images/generations"}, Body: JSONBody(body),
		Response: ImageReply{Decode: decodeXAIImages}, Ambiguous: true}, nil
}

func decodeXAIImages(body []byte, stage StageImage) (*ImageResult, *Error) {
	result, failure := DecodeImageResponse(body, stage)
	if failure == nil && result.CreatedAt == 0 {
		result.CreatedAt = nowUnix()
	}
	return result, failure
}
