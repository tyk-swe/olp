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
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
		publisher := "google"
		if p.Hosting == "vertex-anthropic" {
			publisher = "anthropic"
		}
		c.Endpoint = "https://provider.example/v1/projects/fixture-project/locations/us-central1/publishers/" + publisher
	case "bedrock":
		c.Endpoint = "https://bedrock.example"
		c.CloudRegion = "us-east-1"
	}
	return c
}

func TestVersionedProfilesSelectIndependentCloudAddresses(t *testing.T) {
	for _, tc := range []struct {
		id     string
		wire   openai.Family
		stream bool
		want   string
	}{
		{"openai-chat", openai.FamilyChat, false, "https://provider.example/v1/chat/completions"},
		{"openai-responses", openai.FamilyResponses, false, "https://provider.example/v1/responses"},
		{"anthropic-messages", openai.FamilyAnthropic, false, "https://provider.example/v1/messages"},
		{"gemini-generation", openai.FamilyGemini, true, "https://provider.example/v1/models/model-a:streamGenerateContent?alt=sse"},
		{"azure-legacy-chat", openai.FamilyChat, false, "https://resource.example/openai/deployments/model-a/chat/completions?api-version=2025-04-01-preview"},
		{"azure-legacy-responses", openai.FamilyResponses, false, "https://resource.example/openai/responses?api-version=2025-04-01-preview"},
		{"azure-v1-chat", openai.FamilyChat, false, "https://resource.example/openai/v1/chat/completions"},
		{"azure-v1-responses", openai.FamilyResponses, false, "https://resource.example/openai/v1/responses"},
		{"vertex-gemini", openai.FamilyGemini, false, "https://provider.example/v1/projects/fixture-project/locations/us-central1/publishers/google/models/model-a:generateContent"},
		{"vertex-anthropic", openai.FamilyAnthropic, true, "https://provider.example/v1/projects/fixture-project/locations/us-central1/publishers/anthropic/models/model-a:streamRawPredict"},
		{"bedrock-converse", openai.FamilyBedrock, false, "https://bedrock.example/model/model-a/converse"},
		{"bedrock-anthropic-invoke", openai.FamilyAnthropic, true, "https://bedrock.example/model/model-a/invoke-with-response-stream"},
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
	if _, err := responses.URL(openai.FamilyChat, "model-a", false); err == nil {
		t.Fatal("Responses profile fell through Chat")
	}
	if _, err := responses.URL(openai.Family("unknown"), "model-a", false); err == nil {
		t.Fatal("unknown dialect fell through Chat")
	}
	invoke := profileConfig(t, "bedrock-invoke")
	if _, err := invoke.TargetFamily(openai.FamilyChat); err == nil {
		t.Fatal("model-specific Invoke acquired a Chat fallback")
	}
}

func TestProfileWrappersKeepNativeStateAndVersionBeforeSigning(t *testing.T) {
	for _, tc := range []struct {
		id, version string
		stream      bool
	}{{"vertex-anthropic", "vertex-2023-10-16", true}, {"bedrock-anthropic-invoke", "bedrock-2023-05-31", false}} {
		c := profileConfig(t, tc.id)
		body, err := c.WrapBody([]byte(`{"model":"bound-model","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"opaque","signature":"exact-native-state"}]}],"max_tokens":64,"stream":true,"unknown_native":{"ordered":[false,null,0]}}`), openai.FamilyAnthropic)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			t.Fatal("invalid wrapper JSON")
		}
		if _, present := fields["model"]; present {
			t.Fatal("cloud model remained in body")
		}
		if string(fields["anthropic_version"]) != `"`+tc.version+`"` || !bytes.Contains(fields["messages"], []byte(`"signature":"exact-native-state"`)) || string(fields["unknown_native"]) != `{"ordered":[false,null,0]}` {
			t.Fatalf("native wrapper changed semantics: %s", body)
		}
		if _, present := fields["stream"]; present != tc.stream {
			t.Fatalf("wrong cloud stream control: %s", body)
		}
		if _, err := c.WrapBody([]byte(`{"anthropic_version":"foreign"}`), openai.FamilyAnthropic); err == nil {
			t.Fatal("accepted version collision")
		}
		if tc.id == "bedrock-anthropic-invoke" {
			u, _ := c.URL(openai.FamilyAnthropic, "model-a", false)
			r, _ := http.NewRequest("POST", u, bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			a := NewAuth(&egress.Policy{})
			_, err := a.Apply(t.Context(), r, c, []byte(`{"access_key_id":"FIXTUREACCESSKEY12345","secret_access_key":"fixture-secret-access-key-value"}`), body)
			if err != nil || !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") || r.Header.Get("X-Amz-Date") == "" {
				t.Fatalf("unsigned completed native request: %v", err)
			}
			assertProfileSignature(t, r, body, "fixture-secret-access-key-value")
		}
	}
}

func TestSemanticConfigurationSurvivesSecretRotationAndRefusesReservedControls(t *testing.T) {
	c := profileConfig(t, "anthropic-messages")
	c.SemanticHeaders = map[string]string{"Anthropic-Version": "2023-06-01", "Anthropic-Beta": "fixture-feature-2026-09-22"}
	a := NewAuth(&egress.Policy{})
	for _, secret := range []string{"first-key", "second-key"} {
		r, _ := http.NewRequest("POST", c.Endpoint+"/messages", nil)
		if _, err := a.Apply(t.Context(), r, c, []byte(secret), nil); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Anthropic-Beta") != "fixture-feature-2026-09-22" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("X-Api-Key") != secret {
			t.Fatal("secret rotation changed semantic configuration")
		}
	}
	for _, name := range []string{"Authorization", "Host", "X-Olp-Route", "Proxy-Authorization", "Connection", "OpenAI-Project"} {
		c.SemanticHeaders = map[string]string{name: "forbidden"}
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatalf("accepted reserved semantic control %s", name)
		}
	}
	for _, value := range []string{"bad\x01value", "bad\x7fvalue", "bad\r\nvalue"} {
		c.SemanticHeaders = map[string]string{"Anthropic-Beta": value}
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatal("invalid HTTP semantic header admitted")
		} else if strings.Contains(err.Error(), value) {
			t.Fatal("header diagnostic echoed value")
		}
	}
	c.SemanticHeaders = map[string]string{"Anthropic-Beta": "a", "anthropic-beta": "b"}
	if c.Validate(&egress.Policy{}) == nil {
		t.Fatal("accepted duplicate semantic header spellings")
	}
	c.SemanticHeaders = map[string]string{"Anthropic-Beta": "a"}
	c.AuthMode = "headers"
	c.CredentialHeaders = []string{"X-Api-Key", "Anthropic-Beta"}
	if c.Validate(&egress.Policy{}) == nil {
		t.Fatal("semantic header mixed with credentials")
	}
	gemini := profileConfig(t, "gemini-generation")
	gemini.QuerySettings = map[string]string{"$xgafv": "2"}
	r, _ := http.NewRequest("POST", "https://provider.example/models/m:generateContent", nil)
	if err := gemini.ApplySemantic(r); err != nil || r.URL.Query().Get("$xgafv") != "2" {
		t.Fatal("schema-owned query unavailable")
	}
	gemini.QuerySettings = map[string]string{"key": "secret"}
	if gemini.Validate(&egress.Policy{}) == nil {
		t.Fatal("query credential was accepted as semantics")
	}
	if profileConfig(t, "azure-v1-chat").AzureScope() == profileConfig(t, "azure-legacy-chat").AzureScope() {
		t.Fatal("Azure audiences were conflated")
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

func TestExistingDialectProviderExtensionNeedsOnlyRegistrationAndBinding(t *testing.T) {
	p, err := LookupProfile("compatible-chat", ProfileRevision)
	if err != nil {
		t.Fatal(err)
	}
	p.ID = "fixture-" + uuid.NewString()
	p.Label = "Independent fixture provider"
	if err := RegisterProfile(p); err != nil {
		t.Fatal(err)
	}
	c := Config{ProfileID: p.ID, ProfileRevision: p.Revision, Kind: p.Kind, AuthMode: "none", Endpoint: "https://fixture.example/v1", Bindings: map[string]Binding{"alias": {Model: "native-model"}}}
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	wire, err := c.TargetFamily(openai.FamilyChat)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.URL(wire, "alias", false)
	if err != nil || endpoint != "https://fixture.example/v1/chat/completions" || c.Model("alias") != "native-model" {
		t.Fatal("registered profile did not reuse components")
	}
	copy, _ := LookupProfile(p.ID, p.Revision)
	copy.OperationDialects["generation"] = "corrupted"
	fresh, _ := LookupProfile(p.ID, p.Revision)
	if reflect.DeepEqual(copy.OperationDialects, fresh.OperationDialects) {
		t.Fatal("catalogue metadata aliases registry")
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

func TestBedrockHostingEnvelopeRejectsAmbiguousMembers(t *testing.T) {
	cfg := profileConfig(t, "bedrock-anthropic-invoke")
	native := base64.StdEncoding.EncodeToString([]byte(`{"type":"message_stop"}`))
	valid := `{"bytes":"` + native + `"}`
	for _, tc := range []struct {
		name, payload string
		duplicate     bool
	}{
		{"duplicate body member", `{"bytes":"` + native + `","bytes":"` + native + `"}`, false},
		{"trailing document", valid + `{}`, false},
		{"duplicate reserved header", valid, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue("chunk")}}
			if tc.duplicate {
				headers = append(headers, eventstream.Header{Name: ":event-type", Value: eventstream.StringValue("chunk")})
			}
			var frame bytes.Buffer
			if err := eventstream.NewEncoder().Encode(&frame, eventstream.Message{Headers: headers, Payload: []byte(tc.payload)}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(cfg.StreamPayload(&frame, 4096)); err == nil {
				t.Fatal("ambiguous hosting envelope accepted")
			}
		})
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
