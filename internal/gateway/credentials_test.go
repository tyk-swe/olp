package gateway

import (
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files from the current behavior")

// TestPresentedKeyLocations pins where every surface finds a gateway API key
// and how each refusal reads, so the official SDKs' credential locations and
// their precedence stay exactly as clients depend on them.
func TestPresentedKeyLocations(t *testing.T) {
	h := newHarness(t, Config{})
	names := map[string]string{h.keyID: "full"}
	for key, authority := range h.rt.keys {
		if authority.ID != h.keyID {
			names[authority.ID] = strings.TrimPrefix(key, "olp_")
		}
	}
	paths := []string{
		"/v1/chat/completions",
		"/native/openai/models/m",
		"/anthropic/v1/messages",
		"/gemini/v1beta/models/m:generateContent",
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
		"/bedrock/model/m/converse",
	}
	cases := []struct {
		name    string
		headers map[string]string
		query   string
	}{
		{"none", nil, ""},
		{"bearer", map[string]string{"Authorization": "Bearer " + fullKey}, ""},
		{"lowercase bearer", map[string]string{"Authorization": "bearer " + fullKey}, ""},
		{"padded bearer", map[string]string{"Authorization": "Bearer  " + fullKey + " "}, ""},
		{"basic", map[string]string{"Authorization": "Basic " + fullKey}, ""},
		{"x-api-key", map[string]string{"X-Api-Key": fullKey}, ""},
		{"x-goog-api-key", map[string]string{"X-Goog-Api-Key": fullKey}, ""},
		{"query key", nil, "key=" + fullKey},
		{"x-olp-api-key", map[string]string{"X-OLP-API-Key": fullKey}, ""},
		{"bearer over x-api-key", map[string]string{"Authorization": "Bearer " + fullKey, "X-Api-Key": otherKey}, ""},
		{"x-goog-api-key over query", map[string]string{"X-Goog-Api-Key": otherKey}, "key=" + fullKey},
		{"x-olp-api-key over bearer", map[string]string{"X-OLP-API-Key": fullKey, "Authorization": "Bearer " + otherKey}, ""},
		{"sigv4 only", map[string]string{"Authorization": "AWS4-HMAC-SHA256 Credential=akid/20240101/us-east-1/bedrock/aws4_request"}, ""},
		{"unknown key", map[string]string{"Authorization": "Bearer olp_unknown"}, ""},
		{"missing scope", map[string]string{"Authorization": "Bearer " + readKey}, ""},
	}
	outcome := func(authorityID string, e *Error) string {
		if e != nil {
			return fmt.Sprintf("%d %s %q", e.Status, e.Code, e.Message)
		}
		return "key " + names[authorityID]
	}
	var out strings.Builder
	for _, path := range paths {
		for _, tc := range cases {
			r := httptest.NewRequest(http.MethodPost, path, nil)
			if tc.query != "" {
				r.URL.RawQuery = tc.query
			}
			for name, value := range tc.headers {
				r.Header.Set(name, value)
			}
			authority, e := h.gateway.authenticate(r, "inference")
			line := fmt.Sprintf("%s | %s | %s", path, tc.name, outcome(authority.ID, e))
			if strings.HasPrefix(path, "/bedrock/") {
				authority, e = h.gateway.bedrockAuthenticate(r)
				line += " | bedrock " + outcome(authority.ID, e)
			}
			out.WriteString(line + "\n")
		}
	}
	path := "testdata/presented_keys.golden"
	if *update {
		if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != string(want) {
		t.Fatalf("where gateway keys are found changed; review and rerun with -update:\n%s", out.String())
	}
}
