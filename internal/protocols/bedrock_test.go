package protocols

import (
	"bytes"
	"fmt"
	"testing"
)

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
