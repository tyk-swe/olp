package branding

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"strings"
	"testing"
)

func TestLogoValidationRequiresBoundedEmbeddedImages(t *testing.T) {
	var encoded bytes.Buffer
	png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 22, 22)))
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	if normalized, err := validateLogo(valid); err != nil || normalized != valid {
		t.Fatalf("valid logo: %v", err)
	}
	if normalized, err := validateLogo(""); err != nil || normalized != "" {
		t.Fatalf("default logo: %v", err)
	}
	encoded.Reset()
	png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 513, 1)))
	for _, raw := range []string{"https://example.com/logo.png", "data:image/svg+xml;base64,PHN2Zz4=", "data:image/png;base64,broken", "data:image/jpeg;base64," + strings.TrimPrefix(valid, "data:image/png;base64,"), "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()), "data:image/png;base64," + strings.Repeat("A", base64.StdEncoding.EncodedLen(maxLogoBytes)+4)} {
		if _, err := validateLogo(raw); err == nil {
			t.Errorf("accepted invalid logo %q", raw[:min(60, len(raw))])
		}
	}
}
