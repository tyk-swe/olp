package probe

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
)

var (
	openAIBatch     = connection.Capability{Operation: "batch", Surface: "openai", Mode: "unary"}
	anthropicBatch  = connection.Capability{Operation: "batch", Surface: "anthropic", Mode: "unary"}
	nativeBatch     = connection.Capability{Operation: "batch", Surface: "native", Mode: "unary"}
	openAIRealtime  = connection.Capability{Operation: "realtime", Surface: "openai", Mode: "realtime"}
	webRTCRealtime  = connection.Capability{Operation: "realtime", Surface: "openai", Mode: "webrtc"}
	geminiRealtime  = connection.Capability{Operation: "realtime", Surface: "gemini", Mode: "realtime"}
	nativeRealtime  = connection.Capability{Operation: "realtime", Surface: "native", Mode: "realtime"}
	bedrockInvoke   = connection.Capability{Operation: "bedrock_invoke", Surface: "bedrock", Mode: "unary"}
	retrieval       = connection.Capability{Operation: "retrieval", Surface: "openai", Mode: "unary"}
	asyncVideoStart = connection.Capability{Operation: "video_create", Surface: "openai", Mode: "async"}
)

func TestCertifiableFollowsTheVendorRules(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind   connectors.Kind
		vendor string
		tuple  connection.Capability
		want   bool
	}{
		"openai batches": {connectors.KindOpenAI, "openai", openAIBatch, true},
		"azure batches":  {connectors.KindAzureOpenAI, "azure", openAIBatch, true},
		"compatible without a vendor cannot batch":     {connectors.KindOpenAICompatible, "", openAIBatch, false},
		"compatible groq batches":                      {connectors.KindOpenAICompatible, "groq", openAIBatch, true},
		"compatible xai cannot batch":                  {connectors.KindOpenAICompatible, "xai", openAIBatch, false},
		"gemini batches through the openai surface":    {connectors.KindGemini, "google", openAIBatch, true},
		"anthropic batches on its own surface":         {connectors.KindAnthropic, "anthropic", anthropicBatch, true},
		"openai cannot batch on the anthropic surface": {connectors.KindOpenAI, "openai", anthropicBatch, false},
		"vertex batches natively":                      {connectors.KindVertexAI, "google-vertex", nativeBatch, true},
		"bedrock batches natively":                     {connectors.KindBedrock, "amazon-bedrock", nativeBatch, true},
		"openai serves webrtc":                         {connectors.KindOpenAI, "openai", webRTCRealtime, true},
		"azure cannot serve webrtc":                    {connectors.KindAzureOpenAI, "azure", webRTCRealtime, false},
		"azure serves openai realtime":                 {connectors.KindAzureOpenAI, "azure", openAIRealtime, true},
		"xai serves native realtime":                   {connectors.KindOpenAICompatible, "xai", nativeRealtime, true},
		"groq has no native realtime":                  {connectors.KindOpenAICompatible, "groq", nativeRealtime, false},
		"bedrock serves native realtime":               {connectors.KindBedrock, "amazon-bedrock", nativeRealtime, true},
		"gemini realtime needs the live profile":       {connectors.KindGemini, "google", geminiRealtime, false},
		"bedrock serves invoke":                        {connectors.KindBedrock, "amazon-bedrock", bedrockInvoke, true},
		"openai cannot serve bedrock invoke":           {connectors.KindOpenAI, "openai", bedrockInvoke, false},
		"bedrock retrieval needs its profile":          {connectors.KindBedrock, "amazon-bedrock", retrieval, false},
		"vertex retrieval needs its profile":           {connectors.KindVertexAI, "google-vertex", retrieval, false},
		"azure openai retrieval needs no profile":      {connectors.KindAzureOpenAI, "azure", retrieval, true},
		"openai starts video creation asynchronously":  {connectors.KindOpenAI, "openai", asyncVideoStart, true},
		"anthropic cannot start video creation":        {connectors.KindAnthropic, "anthropic", asyncVideoStart, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := certifiable(c.kind, c.vendor, c.tuple); got != c.want {
				t.Fatalf("certifiable(%s, %q, %+v) = %t, want %t", c.kind, c.vendor, c.tuple, got, c.want)
			}
		})
	}
}

func TestCapabilityOptionsForKindOfferWhatAProfileCanCertify(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind  connectors.Kind
		tuple connection.Capability
		want  bool
	}{
		"bedrock retrieval through its profile":         {connectors.KindBedrock, retrieval, true},
		"vertex retrieval through its profile":          {connectors.KindVertexAI, retrieval, true},
		"azure ai search retrieval through its profile": {connectors.KindAzureAISearch, retrieval, true},
		"gemini realtime through the live profile":      {connectors.KindGemini, geminiRealtime, true},
		"gemini decisions through its profile":          {connectors.KindGemini, connection.Capability{Operation: "decision", Surface: "gemini", Mode: "unary"}, true},
		"compatible batches through a vendor profile":   {connectors.KindOpenAICompatible, openAIBatch, true},
		"anthropic offers no realtime":                  {connectors.KindAnthropic, openAIRealtime, false},
		"openai offers no native batch":                 {connectors.KindOpenAI, nativeBatch, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := slices.Contains(OptionsForKind(c.kind), c.tuple); got != c.want {
				t.Fatalf("%s offers %+v = %t, want %t", c.kind, c.tuple, got, c.want)
			}
		})
	}
}

func TestGeminiDecisionsCertifyThePublicSurface(t *testing.T) {
	t.Parallel()
	const model = "decision-model"
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/decisions" || r.Header.Get("X-Goog-Api-Key") != "sk-test" {
			t.Error("incorrect certification method, path, or credential")
		}
		var request struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode certification request: %v", err)
		}
		if request.Model != model || len(request.Questions) == 0 {
			t.Errorf("invalid certification request: %+v", request)
		}
		answers := maps.Collect(func(yield func(string, json.RawMessage) bool) {
			for name := range request.Questions {
				if !yield(name, json.RawMessage(`{"type":"noul","noul":0.9,"confidence":0.9}`)) {
					return
				}
			}
		})
		if err := json.NewEncoder(w).Encode(struct {
			Answers map[string]json.RawMessage `json:"answers"`
		}{Answers: answers}); err != nil {
			t.Errorf("encode certification result: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)
	raw, err := json.Marshal(connection.Configuration{Identity: providerconfig.Identity[*string]{
		Kind: connectors.KindGemini, AuthMode: connectors.AuthAPIKey, Endpoint: new(upstream.URL + "/v1beta"),
		ProfileID: "gemini-decisions", ProfileRevision: connectors.ProfileRevision,
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := connection.Load(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	tuple := DefaultTuple(prepared.Transport())
	want := connection.Capability{Operation: "decision", Surface: "gemini", Mode: "unary"}
	if tuple != want {
		t.Fatalf("default capability = %+v, want %+v", tuple, want)
	}
	if err := prober(loopback).Certify(t.Context(), Target{ProviderID: "provider", Connection: prepared, Credential: []byte("sk-test")}, model, tuple); err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 1 {
		t.Fatalf("issued %d certification requests, want one", probes.Load())
	}
}
