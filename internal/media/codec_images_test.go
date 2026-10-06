package media

import (
	"encoding/base64"
	"testing"
)

func fieldText(call *UpstreamCall, name string) (string, bool) {
	for _, field := range call.Fields {
		if field.Name == name && field.Text != nil {
			return *field.Text, true
		}
	}
	return "", false
}

func TestStabilityServicesFollowTheModel(t *testing.T) {
	for model, want := range map[string]string{
		"stable-image-core":  "v2beta/stable-image/generate/core",
		"stable-image-ultra": "v2beta/stable-image/generate/ultra",
		"sd3.5-large":        "v2beta/stable-image/generate/sd3",
	} {
		call, failure := encodeStabilityImage(imageRequest(1, "1024x1536", "b64_json"), model)
		if failure != nil || call.Path != want || call.Accept != "application/json" {
			t.Fatalf("%s: %+v %v", model, call, failure)
		}
		ratio, _ := fieldText(call, "aspect_ratio")
		named, hasModel := fieldText(call, "model")
		if ratio != "2:3" || hasModel != (model == "sd3.5-large") || hasModel && named != model {
			t.Fatalf("%s fields: %+v", model, call.Fields)
		}
	}
	for name, invalid := range map[string]struct {
		r     *Request
		model string
	}{
		"unknown model": {imageRequest(1, "", ""), "dall-e-3"},
		"two images":    {imageRequest(2, "", ""), "stable-image-core"},
		"url":           {imageRequest(1, "", "url"), "stable-image-core"},
	} {
		if _, failure := encodeStabilityImage(invalid.r, invalid.model); failure == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	edit := &Request{Op: OpImageEdit, Prompt: "replace the sky", Images: []Part{{Handle: "image"}}, Mask: &Part{Handle: "mask"}}
	call, failure := encodeStabilityEdit(edit, "stable-image-inpaint")
	if failure != nil || call.Path != "v2beta/stable-image/edit/inpaint" || len(call.Fields) != 3 || call.Fields[1].Name != "image" || call.Fields[2].Name != "mask" {
		t.Fatalf("inpaint = %+v %v", call, failure)
	}
	stage := func(string, int) (*Artifact, *Error) { return &Artifact{Handle: "staged"}, nil }
	image := base64.StdEncoding.EncodeToString([]byte("png"))
	if result, failure := decodeStabilityImage([]byte(`{"image":"`+image+`","finish_reason":"SUCCESS","seed":343940597}`), stage); failure != nil || len(result.Images) != 1 {
		t.Fatalf("decoded %+v %v", result, failure)
	}
	if _, failure := decodeStabilityImage([]byte(`{"image":"","finish_reason":"CONTENT_FILTERED","seed":1}`), stage); failure == nil || failure.Code != "content_filter" {
		t.Fatalf("filtered image: %+v", failure)
	}
}

func TestRecraftTakesAspectRatios(t *testing.T) {
	format := "webp"
	request := imageRequest(3, "1792x1024", "url")
	request.OutputFormat = &format
	call, failure := encodeRecraftImage(request, "recraftv4_1")
	if failure != nil || call.Path != "images/generations/raster" ||
		string(call.JSON) != `{"image_format":"webp","model":"recraftv4_1","n":3,"prompt":"a small illustration","response_format":"url","size":"16:9"}` {
		t.Fatalf("call = %+v %s %v", call, call.JSON, failure)
	}
	if _, failure := encodeRecraftImage(imageRequest(7, "", ""), "recraftv4_1"); failure == nil {
		t.Fatal("Recraft accepted seven images")
	}
}
