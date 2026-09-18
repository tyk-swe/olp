package protocols

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestBedrockInlineImagesAcrossSurfaces(t *testing.T) {
	for _, tc := range []struct {
		family openai.Family
		body   string
	}{
		{openai.FamilyChat, `{"model":"route","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}]}`},
		{openai.FamilyResponses, `{"model":"route","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]}]}`},
		{openai.FamilyInputTokens, `{"model":"route","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]}]}`},
		{openai.FamilyAnthropic, `{"model":"route","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyAnthropicCount, `{"model":"route","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGemini, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGeminiStream, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
		{openai.FamilyGeminiCount, `{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}]}`},
	} {
		t.Run(string(tc.family), func(t *testing.T) {
			request, err := Parse(tc.family, []byte(tc.body), "route")
			if err != nil {
				t.Fatal(err)
			}
			body, wire, err := Encode(request, "bedrock", "amazon-bedrock", "wire-model", nil)
			if err != nil {
				t.Fatal(err)
			}
			wantWire := openai.Family("bedrock")
			if tc.family.Operation() == "token_count" {
				wantWire = "bedrock_count"
				if !bytes.Contains(body, []byte(`"converse"`)) {
					t.Fatalf("missing CountTokens converse envelope: %s", body)
				}
			}
			if wire != wantWire || !bytes.Contains(body, []byte(`"image":{"format":"png","source":{"bytes":"aW1hZ2U="}}`)) {
				t.Fatalf("lost inline image: %s %s", wire, body)
			}
		})
	}
}

func TestBedrockImageFormatsAndRefusals(t *testing.T) {
	for _, format := range []string{"png", "jpeg", "gif", "webp"} {
		t.Run(format, func(t *testing.T) {
			part := Part{MIME: "image/" + format, URL: "data:image/" + format + ";base64,aW1hZ2U="}
			body, err := encodeBedrock(&Generation{Messages: []Message{{Role: "user", Parts: []Part{part}}}}, "model", "generation")
			if err != nil || !bytes.Contains(raw(body), []byte(fmt.Sprintf(`"format":%q`, format))) {
				t.Fatalf("image format lost: %s %v", raw(body), err)
			}
		})
	}
	for _, tc := range []struct {
		name, role, mime, url, detail string
	}{
		{"remote URL", "user", "image/png", "https://example.com/image.png", ""},
		{"image detail", "user", "image/png", "data:image/png;base64,aW1hZ2U=", "high"},
		{"unsupported MIME", "user", "image/svg+xml", "data:image/svg+xml;base64,aW1hZ2U=", ""},
		{"invalid base64", "user", "image/png", "data:image/png;base64,invalid!", ""},
		{"system image", "system", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
		{"tool result image", "tool", "image/png", "data:image/png;base64,aW1hZ2U=", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := Message{Role: tc.role, Parts: []Part{{Text: "result"}, {MIME: tc.mime, URL: tc.url, Detail: tc.detail}}}
			if tc.role == "tool" {
				message.ToolID = "tool-1"
			}
			for _, operation := range []string{"generation", "token_count"} {
				if _, err := encodeBedrock(&Generation{Messages: []Message{message}}, "model", operation); err == nil {
					t.Errorf("%s accepted unsupported %s", operation, tc.name)
				}
			}
		})
	}
}
