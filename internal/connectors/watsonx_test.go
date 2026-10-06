package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const watsonxProject = "8f3b2c1d-1234-4abc-9def-0123456789ab"

func watsonx() Config {
	return Config{Kind: KindWatsonx, AuthMode: "ibm_iam", CloudRegion: "us-south", CloudProject: watsonxProject, VendorID: "ibm-watsonx",
		Endpoint: DefaultEndpoint(KindWatsonx, "us-south", watsonxProject)}
}

func TestWatsonxAddressesItsChatAPI(t *testing.T) {
	c := watsonx()
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for stream, want := range map[bool]string{
		false: "https://us-south.ml.cloud.ibm.com/ml/v1/text/chat?version=2025-10-25",
		true:  "https://us-south.ml.cloud.ibm.com/ml/v1/text/chat_stream?version=2025-10-25",
	} {
		if got, err := c.URL(openai.FamilyChat, "ibm/granite-3-8b-instruct", stream); err != nil || got != want {
			t.Fatalf("URL(stream %v) = %s, %v", stream, got, err)
		}
	}
	c.APIVersion = "2026-09-25"
	if got, _ := c.URL(openai.FamilyChat, "ibm/granite-3-8b-instruct", false); !strings.HasSuffix(got, "?version=2026-09-25") {
		t.Fatalf("configured version lost: %s", got)
	}
	if _, err := c.URL(openai.FamilyResponses, "ibm/granite-3-8b-instruct", false); err == nil {
		t.Fatal("watsonx addressed a Responses call")
	}
	for model, valid := range map[string]bool{"ibm/granite-3-8b-instruct": true, "meta-llama/llama-3-3-70b-instruct": true, "mistralai/mistral-large": true,
		"ibm/../x": false, "/granite": false, "ibm/": false, "a b": false} {
		if c.ValidModel(model) != valid {
			t.Fatalf("ValidModel(%q) = %v", model, !valid)
		}
	}
	if !c.Supports("generation", "openai", "streaming") || !c.Supports("generation", "anthropic", "unary") || c.Supports("generation", "bedrock", "unary") || c.Supports("embeddings", "openai", "unary") {
		t.Fatal("watsonx serves generation on the translated surfaces only")
	}
	for name, mutate := range map[string]func(*Config){
		"no project":    func(c *Config) { c.CloudProject = "" },
		"no region":     func(c *Config) { c.CloudRegion = "" },
		"path":          func(c *Config) { c.Endpoint += "/ml/v1" },
		"undated":       func(c *Config) { c.APIVersion = "latest" },
		"deployment":    func(c *Config) { c.Deployment = "x" },
		"early version": func(c *Config) { c.APIVersion = "2019-01-01" },
	} {
		c := watsonx()
		mutate(&c)
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestWatsonxAdaptsChatBodies(t *testing.T) {
	c := watsonx()
	for choice, want := range map[string]map[string]string{
		`"required"`: {"tool_choice_option": `"required"`},
		`{"type":"function","function":{"name":"lookup"}}`: {"tool_choice": `{"function":{"name":"lookup"},"type":"function"}`},
	} {
		body := `{"model":"ibm/granite-3-8b-instruct","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true},"max_tokens":32,"tool_choice":` + choice + `}`
		var got map[string]json.RawMessage
		if err := json.Unmarshal(c.WrapRequest([]byte(body), "ibm/granite-3-8b-instruct"), &got); err != nil {
			t.Fatal(err)
		}
		for _, absent := range []string{"model", "stream", "stream_options"} {
			if _, found := got[absent]; found {
				t.Fatalf("%s reached watsonx: %v", absent, got)
			}
		}
		if string(got["model_id"]) != `"ibm/granite-3-8b-instruct"` || string(got["project_id"]) != `"`+watsonxProject+`"` || string(got["max_tokens"]) != "32" {
			t.Fatalf("adapted body = %v", got)
		}
		for field, value := range want {
			if normalized(got[field]) != normalized([]byte(value)) {
				t.Fatalf("%s = %s, want %s", field, got[field], value)
			}
		}
	}
}

// TestWatsonxResultsReadAsChatCompletions decodes the documented result.
func TestWatsonxResultsReadAsChatCompletions(t *testing.T) {
	documented := `{"id":"chat-2ef4f9c0f2ec4a3a","model_id":"meta-llama/llama-3-8b-instruct","choices":[{"index":0,"message":{"role":"assistant","content":"Hello there."},"finish_reason":"stop"}],"created":1728404199,"created_at":"2024-10-08T16:16:40.102Z","usage":{"completion_tokens":12,"prompt_tokens":87,"total_tokens":99},"system":{"warnings":[{"message":"This model is a Non-IBM Product.","id":"disclaimer_warning","more_info":"https://dataplatform.cloud.ibm.com"}]}}`
	result := watsonx().UnwrapResponse([]byte(documented))
	completion, err := openai.DecodeChat(result, "team-chat")
	if err != nil || completion.OutputText != "Hello there." || completion.Usage == nil || completion.Usage.InputTokens != 87 || completion.Usage.OutputTokens != 12 {
		t.Fatalf("decoded %+v, %v", completion, err)
	}
	for _, leaked := range []string{"model_id", "created_at", "system", "Non-IBM"} {
		if strings.Contains(string(completion.Body), leaked) {
			t.Fatalf("%s reached the client: %s", leaked, completion.Body)
		}
	}
}

const watsonxStreamFixture = "id: 1\nevent: message\ndata: {\"id\":\"chat-1\",\"model_id\":\"ibm/granite-3-8b-instruct\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}],\"created\":1728404199,\"created_at\":\"2024-10-08T16:16:40Z\"}\n\n" +
	"id: 2\nevent: message\ndata: {\"id\":\"chat-1\",\"model_id\":\"ibm/granite-3-8b-instruct\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}],\"created\":1728404199}\n\n" +
	"id: 3\nevent: message\ndata: {\"id\":\"chat-1\",\"model_id\":\"ibm/granite-3-8b-instruct\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there.\"},\"finish_reason\":\"stop\"}],\"created\":1728404199}\n\n"

const watsonxUsageChunk = "id: 4\nevent: message\ndata: {\"id\":\"chat-1\",\"model_id\":\"ibm/granite-3-8b-instruct\",\"choices\":[],\"created\":1728404199,\"usage\":{\"completion_tokens\":3,\"prompt_tokens\":9,\"total_tokens\":12}}\n\n"

func streamWatsonx(fixture string) (*openai.Completion, string, error) {
	var client strings.Builder
	completion, err := openai.Stream(openai.FamilyChat, watsonx().StreamPayload(strings.NewReader(fixture), 1<<16), 1<<16, "team-chat", true, func(frame []byte) error {
		client.Write(frame)
		return nil
	})
	return completion, client.String(), err
}

// TestWatsonxStreamEndsWithItsUsageChunk covers the stream's terminal rule:
// its usage chunk ends it, and a stream that ends before it is truncated.
func TestWatsonxStreamEndsWithItsUsageChunk(t *testing.T) {
	completion, client, err := streamWatsonx(watsonxStreamFixture + watsonxUsageChunk)
	if err != nil || completion.OutputText != "Hello there." || completion.Usage == nil || completion.Usage.TotalTokens != 12 {
		t.Fatalf("stream = %+v, %v", completion, err)
	}
	if !strings.HasSuffix(client, "data: [DONE]\n\n") || strings.Contains(client, "model_id") || strings.Contains(client, "created_at") {
		t.Fatalf("client stream:\n%s", client)
	}
	_, _, err = streamWatsonx(watsonxStreamFixture)
	if failure, ok := errors.AsType[*openai.ProtocolError](err); !ok || !failure.Truncated {
		t.Fatalf("a stream without its usage chunk ended as %v", err)
	}
	unfinished := strings.Replace(watsonxStreamFixture, `"finish_reason":"stop"`, `"finish_reason":null`, 1)
	if _, _, err = streamWatsonx(unfinished + watsonxUsageChunk); err == nil {
		t.Fatal("a stream whose choice never finished completed")
	}
	failed := watsonxStreamFixture[:strings.Index(watsonxStreamFixture, "id: 3")] + "event: error\ndata: {\"errors\":[{\"code\":\"model_overloaded\",\"message\":\"busy\"}],\"trace\":\"t\",\"status_code\":503}\n\n"
	if _, _, err = streamWatsonx(failed); err == nil {
		t.Fatal("a watsonx error event did not fail the stream")
	} else if stated, ok := errors.AsType[*openai.UpstreamError](err); !ok || stated.Code != "model_overloaded" {
		t.Fatalf("error event = %v", err)
	}
}

// TestWatsonxExchangesAPIKeysForIAMTokens covers the IAM exchange: tokens are
// reused until four fifths of their lifetime, and a refused key is rejected.
func TestWatsonxExchangesAPIKeysForIAMTokens(t *testing.T) {
	var exchanges atomic.Int32
	status := atomic.Int32{}
	status.Store(http.StatusOK)
	iam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ibm:params:oauth:grant-type:apikey" || r.Form.Get("apikey") == "" ||
			r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("exchange = %v %v", r.Form, r.Header)
		}
		w.WriteHeader(int(status.Load()))
		writeTestJSON(w, map[string]any{"access_token": "iam-token-" + r.Form.Get("apikey")[:4], "refresh_token": "not-used", "token_type": "Bearer",
			"expires_in": 3600, "expiration": time.Now().Add(time.Hour).Unix()})
	}))
	defer iam.Close()
	a := NewAuth(localPolicy())
	a.publicClient, a.iamEndpoint = a.client, iam.URL
	apply := func(secret string) (*http.Request, error) {
		req, _ := http.NewRequest(http.MethodPost, "https://us-south.ml.cloud.ibm.com/ml/v1/text/chat?version=2025-10-25", nil)
		_, err := a.Apply(context.Background(), req, watsonx(), []byte(secret), nil)
		return req, err
	}
	for range 2 {
		req, err := apply("abcd-ibm-cloud-api-key-0123456789")
		if err != nil || req.Header.Get("Authorization") != "Bearer iam-token-abcd" {
			t.Fatalf("authorization = %q, %v", req.Header.Get("Authorization"), err)
		}
	}
	if exchanges.Load() != 1 {
		t.Fatalf("%d exchanges for one key", exchanges.Load())
	}
	status.Store(http.StatusBadRequest)
	if _, err := apply("wxyz-unknown-ibm-cloud-api-key"); !errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("refused key = %v", err)
	}
	status.Store(http.StatusServiceUnavailable)
	if _, err := apply("efgh-another-ibm-cloud-api-key"); !errors.Is(err, ErrAuthentication) || errors.Is(err, ErrCredentialRejected) {
		t.Fatalf("an IAM outage rejected the key: %v", err)
	}
	before := exchanges.Load()
	if _, err := apply("short"); !errors.Is(err, ErrCredentialRejected) || exchanges.Load() != before {
		t.Fatalf("a malformed key was exchanged: %v", err)
	}
}

func writeTestJSON(w io.Writer, v any) { _ = json.NewEncoder(w).Encode(v) }

func normalized(raw []byte) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v)
	return string(out)
}
