package connectors

import (
	"net/url"
	"strings"
)

// Native Vertex credentials authorize only the Google endpoint for their
// configured location, using system trust. Check before acquiring tokens or
// placing any authentication material, including service-account tokens.
func (c Config) vertexDestination(u *url.URL) bool {
	if u == nil {
		return false
	}
	if cloudTestDestination(u) {
		return true
	}
	expectedRaw := DefaultEndpoint("vertex_ai", c.CloudRegion, c.CloudProject)
	if c.Hosting() == "google-document-ai" {
		expectedRaw = DefaultProfileEndpoint("vertex_ai", "vertex-document-processing-ocr", c.CloudRegion, c.CloudProject)
	}
	expected, err := url.Parse(expectedRaw)
	return (c.Network == nil || c.Network.TrustRootsPEM == "") && err == nil && expected.Hostname() != "" &&
		cloudIdentifier.MatchString(c.CloudRegion) && u.Scheme == "https" && u.User == nil && u.Opaque == "" &&
		strings.EqualFold(u.Hostname(), expected.Hostname()) && (u.Port() == "" || u.Port() == "443")
}
