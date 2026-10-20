package providers

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestBatchCertificationUsesProviderListings(t *testing.T) {
	for _, tc := range []struct {
		kind, vendor, profile string
		pages                 map[string]string
	}{
		{KindAnthropic, "anthropic", "", map[string]string{"/v1/files": `{"data":[]}`, "/v1/messages/batches": `{"data":[]}`}},
		{KindGemini, "gemini", "", map[string]string{"/v1beta/files": `{}`, "/v1beta/batches": `{}`}},
		{KindOpenAICompatible, "mistral", "mistral-files-batches", map[string]string{"/v1/files": `{"data":[]}`, "/v1/batch/jobs": `{"data":[]}`}},
		{KindOpenAICompatible, "together", "together-files-batches", map[string]string{"/v1/batches": `[]`}},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			seen := map[string]bool{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, ok := tc.pages[req.URL.Path]
				if !ok || req.Method != http.MethodGet {
					t.Errorf("unexpected batch probe: %s %s", req.Method, req.URL)
					http.NotFound(w, req)
					return
				}
				if tc.kind == KindAnthropic && strings.HasSuffix(req.URL.Path, "/files") && req.Header.Get("Anthropic-Beta") != "files-api-2025-04-14" {
					t.Error("Anthropic file probe is missing its beta header")
				}
				seen[req.URL.Path] = true
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(upstream.Close)
			basePath := "/v1"
			if tc.kind == KindGemini {
				basePath = "/v1beta"
			}
			cfg := Configuration{ProviderID: "batch-probe", Kind: tc.kind, AuthMode: AuthAPIKey, ProfileID: tc.profile, Endpoint: new(upstream.URL + basePath), Options: Options{VendorID: new(tc.vendor)}}
			if tc.profile != "" {
				cfg.ProfileRevision = "1"
			}
			cfg.Normalize()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			s := New(nil, policy, nil)
			if err := s.certifyBatch(t.Context(), &cfg, []byte("test-credential")); err != nil || len(seen) != len(tc.pages) {
				t.Fatalf("batch certification failed: %v probes=%v", err, seen)
			}
		})
	}
}

func TestVertexCertificationEmptyListing(t *testing.T) {
	for _, body := range []string{`{}`, `{"batchPredictionJobs":[]}`} {
		if !validBatchListing([]byte(body), "batchPredictionJobs", true) {
			t.Fatalf("valid empty protobuf listing refused: %s", body)
		}
	}
	for _, body := range []string{`null`, `[]`, `{"batchPredictionJobs":{}}`, `{"batchPredictionJobs":null}`} {
		if validBatchListing([]byte(body), "batchPredictionJobs", true) {
			t.Fatalf("malformed protobuf listing accepted: %s", body)
		}
	}
}
