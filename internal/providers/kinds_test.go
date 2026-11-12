package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/vendors"
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
		kind, vendor string
		tuple        connection.Capability
		want         bool
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
		kind  string
		tuple connection.Capability
		want  bool
	}{
		"bedrock retrieval through its profile":         {connectors.KindBedrock, retrieval, true},
		"vertex retrieval through its profile":          {connectors.KindVertexAI, retrieval, true},
		"azure ai search retrieval through its profile": {connectors.KindAzureAISearch, retrieval, true},
		"gemini realtime through the live profile":      {connectors.KindGemini, geminiRealtime, true},
		"compatible batches through a vendor profile":   {connectors.KindOpenAICompatible, openAIBatch, true},
		"anthropic offers no realtime":                  {connectors.KindAnthropic, openAIRealtime, false},
		"openai offers no native batch":                 {connectors.KindOpenAI, nativeBatch, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := slices.Contains(capabilityOptionsForKind(c.kind), c.tuple); got != c.want {
				t.Fatalf("%s offers %+v = %t, want %t", c.kind, c.tuple, got, c.want)
			}
		})
	}
}

type listedVendor struct {
	ID             string   `json:"id"`
	Connector      string   `json:"connector"`
	Authentication []string `json:"authentication"`
}

func TestVendorListingPublishesEveryVendorWithItsAuthentication(t *testing.T) {
	t.Parallel()
	reply, err := (&Server{}).listVendors(httptest.NewRequest(http.MethodGet, "/api/v1/provider-vendors", nil), access.Principal{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	var listed []listedVendor
	if err := json.Unmarshal(encoded, &listed); err != nil {
		t.Fatal(err)
	}
	contracts := vendors.All()
	if len(listed) != len(contracts) {
		t.Errorf("listed %d vendors, the vendors package publishes %d", len(listed), len(contracts))
	}
	for _, contract := range contracts {
		i := slices.IndexFunc(listed, func(v listedVendor) bool { return v.ID == contract.ID })
		if i < 0 {
			t.Errorf("vendor %s is not listed", contract.ID)
			continue
		}
		if listed[i].Connector != contract.Connector || len(listed[i].Authentication) == 0 {
			t.Errorf("vendor %s listed as %+v, want connector %s with its authentication modes", contract.ID, listed[i], contract.Connector)
		}
	}
}
