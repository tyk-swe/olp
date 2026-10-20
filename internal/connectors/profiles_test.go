package connectors

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
)

func profileConfig(t *testing.T, id string) Config {
	t.Helper()
	p, err := LookupProfile(id, ProfileRevision)
	if err != nil {
		t.Fatal(err)
	}
	c := Config{ProfileID: id, ProfileRevision: p.Revision, Kind: p.Kind, AuthMode: p.Authentication[0], Endpoint: "https://provider.example/v1"}
	switch p.Kind {
	case "azure_openai":
		c.Endpoint = "https://resource.example"
		if p.Hosting != "azure-v1" {
			c.APIVersion = "2025-04-01-preview"
			c.Deployment = "model-a"
		}
	case "vertex_ai":
		c.CloudRegion = "us-central1"
		c.CloudProject = "fixture-project"
		c.Endpoint = DefaultProfileEndpoint(c.Kind, c.ProfileID, c.CloudRegion, c.CloudProject)
	case "bedrock":
		c.Endpoint = "https://bedrock.example"
		c.CloudRegion = "us-east-1"
	}
	return c
}

func TestVersionedProfilesSelectIndependentCloudAddresses(t *testing.T) {
	for _, tc := range []struct {
		id     string
		wire   wire.Format
		stream bool
		want   string
	}{
		{"openai-chat", wire.Chat, false, "https://provider.example/v1/chat/completions"},
		{"openai-responses", wire.Responses, false, "https://provider.example/v1/responses"},
		{"anthropic-messages", wire.Anthropic, false, "https://provider.example/v1/messages"},
		{"gemini-generation", wire.Gemini, true, "https://provider.example/v1/models/model-a:streamGenerateContent?alt=sse"},
		{"azure-legacy-chat", wire.Chat, false, "https://resource.example/openai/deployments/model-a/chat/completions?api-version=2025-04-01-preview"},
		{"azure-legacy-responses", wire.Responses, false, "https://resource.example/openai/responses?api-version=2025-04-01-preview"},
		{"azure-v1-chat", wire.Chat, false, "https://resource.example/openai/v1/chat/completions"},
		{"azure-v1-responses", wire.Responses, false, "https://resource.example/openai/v1/responses"},
		{"vertex-gemini", wire.Gemini, false, "https://us-central1-aiplatform.googleapis.com/v1/projects/fixture-project/locations/us-central1/publishers/google/models/model-a:generateContent"},
		{"vertex-anthropic", wire.Anthropic, true, "https://us-central1-aiplatform.googleapis.com/v1/projects/fixture-project/locations/us-central1/publishers/anthropic/models/model-a:streamRawPredict"},
		{"bedrock-converse", wire.Bedrock, false, "https://bedrock.example/model/model-a/converse"},
		{"bedrock-anthropic-invoke", wire.Anthropic, true, "https://bedrock.example/model/model-a/invoke-with-response-stream"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c := profileConfig(t, tc.id)
			if err := c.Validate(&egress.Policy{}); err != nil {
				t.Fatal(err)
			}
			got, err := c.URL(tc.wire, "model-a", tc.stream)
			if err != nil || got != tc.want {
				t.Fatalf("address=%s err=%v want=%s", got, err, tc.want)
			}
		})
	}
	responses := profileConfig(t, "azure-v1-responses")
	if _, err := responses.URL(wire.Chat, "model-a", false); err == nil {
		t.Fatal("Responses profile fell through Chat")
	}
	if _, err := responses.URL(wire.Format("unknown"), "model-a", false); err == nil {
		t.Fatal("unknown dialect fell through Chat")
	}
	invoke := profileConfig(t, "bedrock-invoke")
	if _, err := invoke.TargetFamily(wire.Chat); err == nil {
		t.Fatal("model-specific Invoke acquired a Chat fallback")
	}
}

func TestOperationDefaultsRetainPresenceAndAtomicBindingOverrides(t *testing.T) {
	c := profileConfig(t, "openai-chat")
	c.OperationDefaults = map[string]DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"temperature": json.RawMessage(`null`), "parallel_tool_calls": json.RawMessage(`false`), "tools": json.RawMessage(`[{"type":"function","function":{"name":"old"}}]`)}}}
	c.Bindings = map[string]Binding{"logical": {Model: "actual", PrincipalID: "declared-account", Defaults: map[string]DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"tools": json.RawMessage(`[]`)}, NativeOptions: map[string]json.RawMessage{"provider_feature": json.RawMessage(`{"enabled":false}`)}}}}}
	values, origins, err := c.DefaultsFor("generation", "logical")
	if err != nil {
		t.Fatal(err)
	}
	if string(values["temperature"]) != "null" || string(values["parallel_tool_calls"]) != "false" || string(values["tools"]) != "[]" || c.Model("logical") != "actual" {
		t.Fatalf("defaults lost presence or atomic replacement: %v", values)
	}
	if len(origins) != 4 {
		t.Fatal("missing provenance")
	}
	values["tools"][0] = 'x'
	again, _, _ := c.DefaultsFor("generation", "logical")
	if string(again["tools"]) != "[]" {
		t.Fatal("caller mutated stored defaults")
	}
	block := c.Bindings["logical"]
	block.Defaults["generation"] = DefaultSet{Dialect: "openai-chat", NativeOptions: map[string]json.RawMessage{"temperature": json.RawMessage(`0`)}}
	c.Bindings["logical"] = block
	if _, _, err := c.DefaultsFor("generation", "logical"); err == nil {
		t.Fatal("native option overwrote a shared control")
	}
	for _, name := range []string{"endpoint", "headers", "model", "previous_response_id", "store", "background"} {
		c.OperationDefaults = map[string]DefaultSet{"generation": {Dialect: "openai-chat", NativeOptions: map[string]json.RawMessage{name: json.RawMessage(`"forbidden"`)}}}
		c.Bindings = nil
		if c.Validate(&egress.Policy{}) == nil {
			t.Fatalf("reserved option %s accepted", name)
		}
	}
	for _, ambiguous := range []json.RawMessage{json.RawMessage(`{"a":1,"a":2}`), json.RawMessage(`"\ud800"`)} {
		c.OperationDefaults = map[string]DefaultSet{"generation": {Dialect: "openai-chat", NativeOptions: map[string]json.RawMessage{"future_native_control": ambiguous}}}
		if c.Validate(&egress.Policy{}) == nil {
			t.Fatalf("ambiguous native default accepted: %s", ambiguous)
		}
	}
}

func TestProfileInternalReadsRetainRegisteredSnapshot(t *testing.T) {
	profile, err := LookupProfile("compatible-chat", ProfileRevision)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = "fixture-" + uuid.NewString()
	profile.Label = "Registered snapshot"
	if err := RegisterProfile(profile); err != nil {
		t.Fatal(err)
	}
	config := Config{ProfileID: profile.ID, ProfileRevision: profile.Revision, Kind: profile.Kind, AuthMode: "api_key", Endpoint: "https://provider.example/v1"}
	originalSchema := bytes.Clone(profile.DefaultSchemas["generation"])

	// Registration owns its metadata even if its caller later changes every
	// reference-shaped component. Public lookups remain detached as well.
	profile.OperationDialects["generation"] = "corrupted"
	profile.DefaultSchemas["generation"][0] = '!'
	profile.Authentication[0] = "corrupted"
	profile.Operations[0] = "corrupted"
	profile.SemanticHeaders[0] = "corrupted"
	copy, err := config.Profile()
	if err != nil {
		t.Fatal(err)
	}
	copy.OperationDialects["generation"] = "corrupted"
	copy.DefaultSchemas["generation"][0] = '!'
	copy.Authentication[0] = "corrupted"
	copy.Operations[0] = "corrupted"
	copy.SemanticHeaders[0] = "corrupted"
	if config.Hosting() != "direct-compatible" || !config.Supports("generation", "openai", "unary") {
		t.Fatal("internal connector read changed after caller mutation")
	}
	if err := config.ValidateProfile(); err != nil {
		t.Fatalf("registered profile changed after caller mutation: %v", err)
	}
	if encodedWire, err := config.TargetFamily(wire.Chat); err != nil || encodedWire != wire.Chat {
		t.Fatalf("registered profile dialect changed: wire=%s err=%v", encodedWire, err)
	}
	fresh, err := config.Profile()
	if err != nil || fresh.OperationDialects["generation"] != "openai-chat" || !bytes.Equal(fresh.DefaultSchemas["generation"], originalSchema) || fresh.Authentication[0] != "api_key" || fresh.Operations[0] != "generation" || fresh.SemanticHeaders[0] != "Openai-Beta" {
		t.Fatalf("public lookup leaked mutated metadata: err=%v", err)
	}
	next := fresh
	next.Revision = "2"
	next.Authentication = []string{"none"}
	if err := RegisterProfile(next); err != nil {
		t.Fatal(err)
	}
	otherRevision := config
	otherRevision.ProfileRevision = next.Revision
	if err := otherRevision.ValidateProfile(); err == nil {
		t.Fatal("a different profile revision admitted the first revision's authentication")
	}
	if err := config.ValidateProfile(); err != nil {
		t.Fatalf("another revision changed the selected profile: %v", err)
	}

	// Appending unrelated revisions cannot change a selected revision while
	// existing requests read it concurrently.
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 32 {
				if config.Hosting() != "direct-compatible" || !config.Supports("generation", "openai", "unary") || config.ValidateProfile() != nil {
					t.Error("concurrent registration changed an existing profile")
					return
				}
			}
		})
	}
	for range 4 {
		another := next
		another.ID = "fixture-" + uuid.NewString()
		if err := RegisterProfile(another); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
}

func TestAnthropicBatchRequiresDirectMessageConnector(t *testing.T) {
	direct := Config{Kind: "anthropic", ProfileID: "anthropic-messages", ProfileRevision: ProfileRevision, AuthMode: "api_key", Endpoint: "https://api.anthropic.com"}
	if !direct.Supports("batch", "anthropic", "unary") || direct.Supports("batch", "openai", "unary") {
		t.Fatal("direct Anthropic message connector batch capability is not surface-bound")
	}
	for _, config := range []Config{
		{Kind: "vertex_ai", VendorID: "google-vertex", ProfileID: "vertex-anthropic", ProfileRevision: ProfileRevision},
		{Kind: "bedrock", VendorID: "amazon-bedrock", ProfileID: "bedrock-anthropic-invoke", ProfileRevision: ProfileRevision},
	} {
		if config.Supports("batch", "anthropic", "unary") {
			t.Fatalf("cloud Anthropic connector gained native message batches: %+v", config)
		}
	}
}

func TestPluginOpenAIChatProfileSkipsBatchProfilesAsTemplates(t *testing.T) {
	profileMu.Lock()
	original := profileRegistry
	filtered := make([]Profile, 0, len(original))
	for _, registered := range original {
		if registered.Kind == "openai_compatible" && slices.Contains(registered.Operations, "batch") {
			continue
		}
		filtered = append(filtered, registered)
	}
	profileRegistry = filtered
	profileMu.Unlock()
	defer func() {
		profileMu.Lock()
		profileRegistry = original
		profileMu.Unlock()
	}()
	profile, err := LookupProfile("compatible-chat", ProfileRevision)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = "fixture-plugin-openai-chat-" + uuid.NewString()
	profile.Label = "Fixture plugin OpenAI Chat"
	profile.Plugin = &Plugin{Digest: strings.Repeat("a", 64), Name: "Fixture", Version: "1"}
	wantTemplate := ""
	for i := range profileRegistry {
		existing := &profileRegistry[i]
		if existing.Kind == profile.Kind && existing.Dialect == profile.Dialect && existing.Hosting == profile.Hosting && existing.DialectRevision == profile.DialectRevision &&
			!vendorBatchProfile(*existing) {
			wantTemplate = existing.ID
			profile.Authentication = slices.Clone(existing.Authentication)
		}
	}
	if template := profileTemplateForRegistration(profile); template == nil || template.ID != wantTemplate {
		t.Fatalf("plugin template = %v, want main last-match template %q", template, wantTemplate)
	}
	if err := RegisterProfile(profile); err != nil {
		t.Fatalf("compatible-chat template registration failed: %v", err)
	}
	config := Config{ProfileID: profile.ID, ProfileRevision: profile.Revision, Kind: profile.Kind, AuthMode: "none", Endpoint: "https://provider.example/v1"}
	if err := config.ValidateProfile(); err != nil {
		t.Fatalf("registered plugin OpenAI Chat profile is invalid: %v", err)
	}
}

func TestBedrockAnthropicFramingRetainsPayloadAndRejectsDrift(t *testing.T) {
	config := profileConfig(t, "bedrock-anthropic-invoke")
	payload := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"exact"},"unknown":[null,false,0]}`
	encode := func(kind string, payload string) []byte {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(payload))})
		var encoded bytes.Buffer
		err := eventstream.NewEncoder().Encode(&encoded, eventstream.Message{Headers: eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(kind)}}, Payload: body})
		if err != nil {
			t.Fatal(err)
		}
		return encoded.Bytes()
	}
	data, err := io.ReadAll(config.StreamPayload(bytes.NewReader(encode("chunk", payload)), 4096))
	if err != nil || string(data) != "event: content_block_delta\ndata: "+payload+"\n\n" {
		t.Fatalf("unwrapped event=%s err=%v", data, err)
	}
	for _, data := range [][]byte{encode("new-event", payload), encode("chunk", `{"type":"bad\nheader"}`), encode("chunk", payload)[:10]} {
		if _, err := io.ReadAll(config.StreamPayload(bytes.NewReader(data), 4096)); err == nil {
			t.Fatal("event drift or truncation accepted")
		}
	}
	if _, err := io.ReadAll(config.StreamPayload(bytes.NewReader(encode("chunk", payload)), 32)); err == nil {
		t.Fatal("oversized event accepted")
	}
}

// Independent SigV4 verification proves the hosting/body/header construction
// was finalized before signing; it does not reuse the maintained SDK signer.
func assertProfileSignature(t *testing.T, request *http.Request, body []byte, secret string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 "), ", ")
	values := map[string]string{}
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			t.Fatal("invalid signature fields")
		}
		values[key] = value
	}
	credential := strings.Split(values["Credential"], "/")
	if len(credential) != 5 {
		t.Fatal("invalid credential scope")
	}
	var canonical strings.Builder
	for _, name := range strings.Split(values["SignedHeaders"], ";") {
		value := request.Header.Get(name)
		if name == "host" {
			value = request.URL.Host
		}
		if name == "content-length" {
			value = strconv.FormatInt(request.ContentLength, 10)
		}
		canonical.WriteString(name + ":" + strings.Join(strings.Fields(value), " ") + "\n")
	}
	hash := func(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
	canonicalRequest := request.Method + "\n" + request.URL.EscapedPath() + "\n" + request.URL.RawQuery + "\n" + canonical.String() + "\n" + values["SignedHeaders"] + "\n" + hash(body)
	scope := strings.Join(credential[1:], "/")
	toSign := "AWS4-HMAC-SHA256\n" + request.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hash([]byte(canonicalRequest))
	sign := func(key []byte, value string) []byte {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(value))
		return mac.Sum(nil)
	}
	key := []byte("AWS4" + secret)
	for _, value := range credential[1:] {
		key = sign(key, value)
	}
	if hex.EncodeToString(sign(key, toSign)) != values["Signature"] {
		t.Fatal("signature does not cover final native body, path and headers")
	}
}

func TestFilesBatchProfilesAreVendorBound(t *testing.T) {
	for _, test := range []struct {
		vendor, profile, adapter string
	}{
		{"groq", "groq-files-batches", "openai"},
		{"together", "together-files-batches", "together"},
		{"mistral", "mistral-files-batches", "mistral"},
	} {
		config := Config{Kind: "openai_compatible", VendorID: test.vendor, ProfileID: test.profile, ProfileRevision: "1",
			Endpoint: "https://api.example/v1", AuthMode: "api_key"}
		if err := config.ValidateProfile(); err != nil {
			t.Fatalf("%s profile: %v", test.vendor, err)
		}
		if !config.SupportsFilesBatches() || config.FilesBatchesAdapter() != test.adapter ||
			!config.Supports("batch", "openai", "unary") {
			t.Fatalf("%s files/batches profile is unavailable", test.vendor)
		}
		config.VendorID = "fireworks"
		if config.ValidateProfile() == nil || config.SupportsFilesBatches() {
			t.Fatalf("%s profile can be assigned to another vendor", test.vendor)
		}
	}
	generic := Config{Kind: "openai_compatible", VendorID: "groq", Endpoint: "https://api.groq.com/openai/v1", AuthMode: "api_key"}
	if generic.SupportsFilesBatches() {
		t.Fatal("a generic compatible profile inherited provider files/batches")
	}
	if config := (Config{Kind: "openai", Endpoint: "https://api.openai.com/v1"}); !config.SupportsFilesBatches() || config.FilesBatchesAdapter() != "openai" {
		t.Fatal("official OpenAI resource endpoint lost files/batches support")
	}
	if config := (Config{Kind: "openai", Endpoint: "https://openai.example/v1"}); config.SupportsFilesBatches() {
		t.Fatal("a custom OpenAI endpoint inherited official files/batches support")
	}
	if config := (Config{Kind: "azure_openai", Endpoint: "https://resource.example"}); !config.SupportsFilesBatches() {
		t.Fatal("Azure resource endpoint lost files/batches support")
	}
}

func TestCloudBatchProfilesDoNotEnableGeneration(t *testing.T) {
	vertex := Config{Kind: "vertex_ai", ProfileID: "vertex-batch-prediction", ProfileRevision: ProfileRevision,
		Endpoint:    DefaultProfileEndpoint("vertex_ai", "vertex-batch-prediction", "us-central1", "fixture-project"),
		CloudRegion: "us-central1", CloudProject: "fixture-project", AuthMode: "adc"}
	bedrock := Config{Kind: "bedrock", ProfileID: "bedrock-batch-inference", ProfileRevision: ProfileRevision,
		Endpoint: DefaultProfileEndpoint("bedrock", "bedrock-batch-inference", "us-east-1", ""), CloudRegion: "us-east-1", AuthMode: "default_chain"}
	for _, config := range []Config{vertex, bedrock} {
		if !config.Supports("batch", "native", "unary") || config.Supports("generation", "openai", "unary") ||
			config.Supports("generation", "native", "unary") {
			t.Fatalf("%s cloud batch profile changed serving generation", config.ProfileID)
		}
	}
	for _, config := range []Config{
		{Kind: "vertex_ai", ProfileID: "vertex-gemini", ProfileRevision: ProfileRevision},
		{Kind: "bedrock", ProfileID: "bedrock-invoke", ProfileRevision: ProfileRevision},
	} {
		if config.Supports("batch", "native", "unary") {
			t.Fatalf("%s acquired cloud batch capability", config.ProfileID)
		}
	}
}

// What the connector publishes is what hosting sends, and it validates.
