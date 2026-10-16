package media

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func imageRequest(n int64, size, format string) *Request {
	r := &Request{Op: OpImageGeneration, Route: "images", Prompt: "a small illustration"}
	if n > 0 {
		r.Count = &n
	}
	if size != "" {
		r.Size = &size
	}
	if format != "" {
		r.Format = &format
	}
	return r
}

func TestNativeImageDecode(t *testing.T) {
	staged := map[int]string{}
	stage := func(b64 string, index int) (*Artifact, *Error) {
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return nil, protocolError("invalid base64")
		}
		staged[index] = b64
		return &Artifact{Handle: Handle("h" + string(rune('0'+index)))}, nil
	}
	payload := base64.StdEncoding.EncodeToString([]byte("png"))
	vertexBody, _ := json.Marshal(map[string]any{"predictions": []any{
		map[string]any{"bytesBase64Encoded": payload, "mimeType": "image/png"},
		map[string]any{"bytesBase64Encoded": payload},
	}})
	result, e := DecodeNativeImageResponse("vertex_ai", vertexBody, 2, stage)
	if e != nil {
		t.Fatal(e.Message)
	}
	if len(result.Images) != 2 || result.Images[0].Handle == nil || len(staged) != 2 {
		t.Fatalf("vertex result: %+v", result)
	}
	bedrockBody, _ := json.Marshal(map[string]any{"images": []string{payload}})
	result, e = DecodeNativeImageResponse("bedrock", bedrockBody, 1, stage)
	if e != nil {
		t.Fatal(e.Message)
	}
	if len(result.Images) != 1 {
		t.Fatalf("bedrock result: %+v", result)
	}
	for _, test := range []struct {
		name string
		kind string
		body string
		want int64
	}{
		{"vertex error", "vertex_ai", `{"error":"safety filter"}`, 1},
		{"vertex missing predictions", "vertex_ai", `{"modelVersionId":"1"}`, 1},
		{"vertex count mismatch", "vertex_ai", `{"predictions":[{"bytesBase64Encoded":"` + payload + `"}]}`, 2},
		{"vertex bad mime", "vertex_ai", `{"predictions":[{"bytesBase64Encoded":"` + payload + `","mimeType":"text/plain"}]}`, 1},
		{"vertex empty payload", "vertex_ai", `{"predictions":[{"bytesBase64Encoded":""}]}`, 1},
		{"bedrock error", "bedrock", `{"error":"throttled"}`, 1},
		{"bedrock missing images", "bedrock", `{"error":null}`, 1},
		{"bedrock count mismatch", "bedrock", `{"images":["` + payload + `"]}`, 2},
		{"unknown kind", "openai", `{"images":["` + payload + `"]}`, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, e := DecodeNativeImageResponse(test.kind, []byte(test.body), test.want, stage); e == nil {
				t.Fatalf("%s accepted", test.name)
			}
		})
	}
}

// TestBedrockStabilityImage covers the Stability text-to-image models on
// Bedrock: one image a call at the size's aspect ratio, and a filtered
// request refused by its finish reason.
func TestBedrockStabilityImage(t *testing.T) {
	request := imageRequest(1, "1024x1536", "b64_json")
	format := "webp"
	request.OutputFormat = &format
	call, e := Encode(request, "bedrock", "stability.sd3-5-large-v1:0")
	if e != nil {
		t.Fatal(e.Message)
	}
	if call.Path != "model/stability.sd3-5-large-v1:0/invoke" || call.Native != "bedrock" || string(call.JSON) != `{"aspect_ratio":"2:3","output_format":"webp","prompt":"a small illustration"}` {
		t.Fatalf("stability call: %+v %s", call, call.JSON)
	}
	for _, model := range []string{"stability.stable-image-core-v1:1", "stability.stable-image-ultra-v1:1"} {
		if call, e := Encode(imageRequest(0, "", ""), "bedrock", model); e != nil || string(call.JSON) != `{"prompt":"a small illustration"}` {
			t.Fatalf("%s: %v", model, e)
		}
	}
	for name, invalid := range map[string]*Request{
		"two images": imageRequest(2, "", ""),
		"odd size":   imageRequest(1, "512x512", ""),
		"url format": imageRequest(1, "", "url"),
	} {
		if _, e := Encode(invalid, "bedrock", "stability.sd3-5-large-v1:0"); e == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	stage := func(string, int) (*Artifact, *Error) { return &Artifact{}, nil }
	image := base64.StdEncoding.EncodeToString([]byte("png"))
	if result, e := DecodeNativeImageResponse("bedrock", []byte(`{"seeds":[2130420379],"finish_reasons":[null],"images":["`+image+`"]}`), 1, stage); e != nil || len(result.Images) != 1 {
		t.Fatalf("documented response: %+v %v", result, e)
	}
	if _, e := DecodeNativeImageResponse("bedrock", []byte(`{"finish_reasons":["Filter reason: prompt"]}`), 1, stage); e == nil || e.Status != 400 || e.Code != "content_filter" {
		t.Fatalf("filtered response: %+v", e)
	}
	if _, e := DecodeNativeImageResponse("bedrock", []byte(`{"finish_reasons":["Inference error"]}`), 1, stage); e == nil || e.Status != 502 {
		t.Fatalf("inference error: %+v", e)
	}
}
