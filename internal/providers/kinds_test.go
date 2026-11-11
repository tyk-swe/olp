package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/vendors"
)

var (
	openAIBatch     = CapabilityInput{Operation: "batch", Surface: SurfaceOpenAI, Mode: ModeUnary}
	anthropicBatch  = CapabilityInput{Operation: "batch", Surface: "anthropic", Mode: ModeUnary}
	nativeBatch     = CapabilityInput{Operation: "batch", Surface: "native", Mode: ModeUnary}
	openAIRealtime  = CapabilityInput{Operation: "realtime", Surface: SurfaceOpenAI, Mode: "realtime"}
	webRTCRealtime  = CapabilityInput{Operation: "realtime", Surface: SurfaceOpenAI, Mode: "webrtc"}
	geminiRealtime  = CapabilityInput{Operation: "realtime", Surface: "gemini", Mode: "realtime"}
	nativeRealtime  = CapabilityInput{Operation: "realtime", Surface: "native", Mode: "realtime"}
	bedrockInvoke   = CapabilityInput{Operation: "bedrock_invoke", Surface: "bedrock", Mode: ModeUnary}
	retrieval       = CapabilityInput{Operation: "retrieval", Surface: SurfaceOpenAI, Mode: ModeUnary}
	asyncVideoStart = CapabilityInput{Operation: "video_create", Surface: SurfaceOpenAI, Mode: "async"}
)

func TestCertifiableFollowsTheVendorRules(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind, vendor string
		tuple        CapabilityInput
		want         bool
	}{
		"openai batches": {KindOpenAI, "openai", openAIBatch, true},
		"azure batches":  {KindAzure, "azure", openAIBatch, true},
		"compatible without a vendor cannot batch":     {KindOpenAICompatible, "", openAIBatch, false},
		"compatible groq batches":                      {KindOpenAICompatible, "groq", openAIBatch, true},
		"compatible xai cannot batch":                  {KindOpenAICompatible, "xai", openAIBatch, false},
		"gemini batches through the openai surface":    {KindGemini, "google", openAIBatch, true},
		"anthropic batches on its own surface":         {KindAnthropic, "anthropic", anthropicBatch, true},
		"openai cannot batch on the anthropic surface": {KindOpenAI, "openai", anthropicBatch, false},
		"vertex batches natively":                      {KindVertex, "google-vertex", nativeBatch, true},
		"bedrock batches natively":                     {KindBedrock, "amazon-bedrock", nativeBatch, true},
		"openai serves webrtc":                         {KindOpenAI, "openai", webRTCRealtime, true},
		"azure cannot serve webrtc":                    {KindAzure, "azure", webRTCRealtime, false},
		"azure serves openai realtime":                 {KindAzure, "azure", openAIRealtime, true},
		"xai serves native realtime":                   {KindOpenAICompatible, "xai", nativeRealtime, true},
		"groq has no native realtime":                  {KindOpenAICompatible, "groq", nativeRealtime, false},
		"bedrock serves native realtime":               {KindBedrock, "amazon-bedrock", nativeRealtime, true},
		"gemini realtime needs the live profile":       {KindGemini, "google", geminiRealtime, false},
		"bedrock serves invoke":                        {KindBedrock, "amazon-bedrock", bedrockInvoke, true},
		"openai cannot serve bedrock invoke":           {KindOpenAI, "openai", bedrockInvoke, false},
		"bedrock retrieval needs its profile":          {KindBedrock, "amazon-bedrock", retrieval, false},
		"vertex retrieval needs its profile":           {KindVertex, "google-vertex", retrieval, false},
		"azure openai retrieval needs no profile":      {KindAzure, "azure", retrieval, true},
		"openai starts video creation asynchronously":  {KindOpenAI, "openai", asyncVideoStart, true},
		"anthropic cannot start video creation":        {KindAnthropic, "anthropic", asyncVideoStart, false},
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
		tuple CapabilityInput
		want  bool
	}{
		"bedrock retrieval through its profile":         {KindBedrock, retrieval, true},
		"vertex retrieval through its profile":          {KindVertex, retrieval, true},
		"azure ai search retrieval through its profile": {KindAzureAISearch, retrieval, true},
		"gemini realtime through the live profile":      {KindGemini, geminiRealtime, true},
		"compatible batches through a vendor profile":   {KindOpenAICompatible, openAIBatch, true},
		"anthropic offers no realtime":                  {KindAnthropic, openAIRealtime, false},
		"openai offers no native batch":                 {KindOpenAI, nativeBatch, false},
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
