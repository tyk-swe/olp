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

func TestVertexImageEncode(t *testing.T) {
	call, e := Encode(imageRequest(2, "1536x1024", "b64_json"), "vertex_ai", "imagen-3.0-generate-002")
	if e != nil {
		t.Fatal(e.Message)
	}
	if call.Path != "models/imagen-3.0-generate-002:predict" || call.Method != "POST" || !call.Ambiguous || call.Native != "vertex_ai" {
		t.Fatalf("vertex call: %+v", call)
	}
	var body struct {
		Instances  []map[string]any `json:"instances"`
		Parameters map[string]any   `json:"parameters"`
	}
	if err := json.Unmarshal(call.JSON, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Instances) != 1 || body.Instances[0]["prompt"] != "a small illustration" {
		t.Fatalf("vertex instances: %s", call.JSON)
	}
	if body.Parameters["sampleCount"] != float64(2) || body.Parameters["aspectRatio"] != "3:2" {
		t.Fatalf("vertex parameters: %s", call.JSON)
	}
	call, e = Encode(imageRequest(0, "", ""), "vertex_ai", "imagen-4.0-generate-001")
	if e != nil {
		t.Fatal(e.Message)
	}
	var defaults struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(call.JSON, &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Parameters["sampleCount"] != float64(1) || defaults.Parameters["aspectRatio"] != nil {
		t.Fatalf("vertex defaults: %s", call.JSON)
	}
	for _, test := range []struct {
		name  string
		model string
		size  string
		extra func(*Request)
	}{
		{name: "unqualified model", model: "gemini-2.5-pro"},
		{name: "unsupported size", model: "imagen-3.0-generate-002", size: "1792x1024"},
		{name: "quality", model: "imagen-3.0-generate-002", extra: func(r *Request) { s := "high"; r.Quality = &s }},
		{name: "style", model: "imagen-3.0-generate-002", extra: func(r *Request) { s := "vivid"; r.Style = &s }},
		{name: "url format", model: "imagen-3.0-generate-002", extra: func(r *Request) { s := "url"; r.Format = &s }},
		{name: "extension", model: "imagen-3.0-generate-002", extra: func(r *Request) { r.Extra = map[string]any{"seed": 1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := imageRequest(1, test.size, "")
			if test.extra != nil {
				test.extra(r)
			}
			if _, e := Encode(r, "vertex_ai", test.model); e == nil {
				t.Fatalf("%s accepted", test.name)
			}
		})
	}
	if _, e := Encode(imageRequest(5, "", ""), "vertex_ai", "imagen-3.0-generate-002"); e == nil {
		t.Fatal("vertex accepted n=5")
	}
}

func TestBedrockImageEncode(t *testing.T) {
	call, e := Encode(imageRequest(2, "768x768", "b64_json"), "bedrock", "amazon.titan-image-generator-v2:0")
	if e != nil {
		t.Fatal(e.Message)
	}
	if call.Path != "model/amazon.titan-image-generator-v2:0/invoke" || call.Method != "POST" || !call.Ambiguous || call.Native != "bedrock" {
		t.Fatalf("bedrock call: %+v", call)
	}
	var body struct {
		TaskType              string         `json:"taskType"`
		TextToImageParams     map[string]any `json:"textToImageParams"`
		ImageGenerationConfig map[string]any `json:"imageGenerationConfig"`
	}
	if err := json.Unmarshal(call.JSON, &body); err != nil {
		t.Fatal(err)
	}
	if body.TaskType != "TEXT_IMAGE" || body.TextToImageParams["text"] != "a small illustration" {
		t.Fatalf("bedrock body: %s", call.JSON)
	}
	if body.ImageGenerationConfig["numberOfImages"] != float64(2) || body.ImageGenerationConfig["width"] != float64(768) || body.ImageGenerationConfig["height"] != float64(768) {
		t.Fatalf("bedrock config: %s", call.JSON)
	}
	call, e = Encode(imageRequest(1, "", ""), "bedrock", "amazon.titan-image-generator-v1")
	if e != nil {
		t.Fatal(e.Message)
	}
	var defaults struct {
		ImageGenerationConfig map[string]any `json:"imageGenerationConfig"`
	}
	if err := json.Unmarshal(call.JSON, &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.ImageGenerationConfig["width"] != float64(1024) {
		t.Fatalf("bedrock defaults: %s", call.JSON)
	}
	if _, e := Encode(imageRequest(1, "512x512", ""), "bedrock", "amazon.titan-image-generator-v1"); e != nil {
		t.Fatal(e.Message)
	}
	for _, test := range []struct {
		name  string
		model string
		size  string
	}{
		{name: "unqualified model", model: "amazon.nova-canvas-v1:0"},
		{name: "non-square", model: "amazon.titan-image-generator-v1", size: "1536x1024"},
		{name: "unsupported size", model: "amazon.titan-image-generator-v1", size: "256x256"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, e := Encode(imageRequest(1, test.size, ""), "bedrock", test.model); e == nil {
				t.Fatalf("%s accepted", test.name)
			}
		})
	}
	if _, e := Encode(imageRequest(5, "", ""), "bedrock", "amazon.titan-image-generator-v1"); e == nil {
		t.Fatal("bedrock accepted n=5")
	}
	if _, e := Encode(imageRequest(1, "", ""), "openai_compatible", "imagen-3.0-generate-002"); e != nil {
		t.Fatal("openai-compatible image generation must keep the OpenAI wire contract")
	}
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
