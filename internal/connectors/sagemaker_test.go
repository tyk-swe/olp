package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func sagemaker() Config {
	return Config{Kind: KindSageMaker, AuthMode: "static", CloudRegion: "us-west-2", VendorID: "amazon-sagemaker",
		Endpoint: DefaultEndpoint(KindSageMaker, "us-west-2", "")}
}

// TestSageMakerAddressesEndpointsAndComponents covers the OpenAI-compatible
// path: the URL routes to the endpoint, or to an inference component on it.
func TestSageMakerAddressesEndpointsAndComponents(t *testing.T) {
	c := sagemaker()
	if err := c.Validate(&egress.Policy{}); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"qwen3-4b":            "https://runtime.sagemaker.us-west-2.amazonaws.com/endpoints/qwen3-4b/openai/v1/chat/completions",
		"shared-ep/qwen-ic-1": "https://runtime.sagemaker.us-west-2.amazonaws.com/endpoints/shared-ep/inference-components/qwen-ic-1/openai/v1/chat/completions",
	} {
		for _, stream := range []bool{false, true} {
			if got, err := c.URL(openai.FamilyChat, model, stream); err != nil || got != want {
				t.Fatalf("URL(%s, stream %v) = %s, %v", model, stream, got, err)
			}
		}
	}
	if _, err := c.URL(openai.FamilyEmbeddings, "qwen3-4b", false); err == nil {
		t.Fatal("SageMaker addressed an embeddings call")
	}
	for model, valid := range map[string]bool{
		"a": true, "my--endpoint-2": true, "ep/ic": true, strings.Repeat("e", 63): true,
		strings.Repeat("e", 64): false, "-ep": false, "ep-": false, "ep/ic/x": false, "ep/": false, "ep.v1": false, "../ep": false,
	} {
		if ModelValid(KindSageMaker, model) != valid || c.ValidModel(model) != valid {
			t.Fatalf("ValidModel(%q) = %v", model, !valid)
		}
	}
	if !c.Supports("generation", "openai", "streaming") || c.Supports("generation", "anthropic", "unary") || c.Supports("embeddings", "openai", "unary") || c.Supports("token_count", "openai", "unary") {
		t.Fatal("SageMaker serves OpenAI chat generation only")
	}
	if china := DefaultEndpoint(KindSageMaker, "cn-north-1", ""); china != "https://runtime.sagemaker.cn-north-1.amazonaws.com.cn" {
		t.Fatalf("China endpoint = %s", china)
	}
	for name, mutate := range map[string]func(*Config){
		"path":       func(c *Config) { c.Endpoint += "/endpoints/x" },
		"no region":  func(c *Config) { c.CloudRegion = "" },
		"deployment": func(c *Config) { c.Deployment = "x" },
	} {
		c := sagemaker()
		mutate(&c)
		if err := c.Validate(&egress.Policy{}); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestSageMakerBodyLeavesModelSelectionToTheURL(t *testing.T) {
	got := sagemaker().WrapRequest([]byte(`{"model":"shared-ep/qwen-ic-1","messages":[{"role":"user","content":"hi"}],"stream":true}`), "shared-ep/qwen-ic-1")
	var body map[string]any
	if err := json.Unmarshal(got, &body); err != nil || body["model"] != "" || body["stream"] != true || len(body["messages"].([]any)) != 1 {
		t.Fatalf("body = %s, %v", got, err)
	}
}

func TestSageMakerRejectionStatesTheContainerStatus(t *testing.T) {
	c := sagemaker()
	modelError := []byte(`{"ErrorCode":"CLIENT_ERROR_FROM_MODEL","Message":"Received client error (400) from primary","OriginalStatusCode":400,"OriginalMessage":"{\"error\":{\"message\":\"bad\",\"type\":\"BadRequestError\"}}","LogStreamArn":"arn"}`)
	status, body := c.Rejection(http.StatusFailedDependency, modelError)
	if status != http.StatusBadRequest || openai.ParseErrorBody(body).Type != "BadRequestError" {
		t.Fatalf("rejection = %d %s", status, body)
	}
	for _, unchanged := range []struct {
		status int
		body   string
	}{{http.StatusFailedDependency, `{"OriginalStatusCode":200}`}, {http.StatusFailedDependency, `not json`}, {http.StatusBadRequest, `{"OriginalStatusCode":500}`}} {
		if status, body := c.Rejection(unchanged.status, []byte(unchanged.body)); status != unchanged.status || string(body) != unchanged.body {
			t.Fatalf("%d %s became %d %s", unchanged.status, unchanged.body, status, body)
		}
	}
	if status, _ := (Config{Kind: "openai_compatible"}).Rejection(http.StatusFailedDependency, modelError); status != http.StatusFailedDependency {
		t.Fatal("only SageMaker unwraps a ModelError")
	}
}

// TestSageMakerTokenMatchesTheSDK checks the bearer token against tokens the
// SageMaker Python SDK's construction produced with botocore 1.40.72 for the
// same credentials, region, lifetime and signing time.
func TestSageMakerTokenMatchesTheSDK(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for session, signature := range map[string]string{
		"":                         "0ba7fbb36150d888acbe18e62e38bbce9a6fc0983c44b021ffa4f8b6e54132f5",
		"SESSIONTOKEN/with+chars=": "14b6642bf17f527fc41523cf6c8db9cc8f34b7e622dccc2e4e7c8bed00bfaa4a",
	} {
		creds := aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", SessionToken: session}
		token, err := sagemakerToken(context.Background(), creds, "us-west-2", at)
		if err != nil {
			t.Fatal(err)
		}
		encoded, ok := strings.CutPrefix(token, "sagemaker-api-key-")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || err != nil {
			t.Fatalf("token %s is not a SageMaker API key: %v", token, err)
		}
		address, query, _ := strings.Cut(string(decoded), "?")
		values, err := url.ParseQuery(query)
		if address != "sagemaker.amazonaws.com/" || err != nil || !strings.HasSuffix(query, "&Version=1") {
			t.Fatalf("decoded token = %s", decoded)
		}
		want := url.Values{"Action": {"CallWithBearerToken"}, "X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-Credential": {"AKIDEXAMPLE/20261005/us-west-2/sagemaker/aws4_request"},
			"X-Amz-Date": {"20261005T120000Z"}, "X-Amz-Expires": {"900"}, "X-Amz-SignedHeaders": {"host"}, "X-Amz-Signature": {signature}, "Version": {"1"}}
		if session != "" {
			want.Set("X-Amz-Security-Token", session)
		}
		if values.Encode() != want.Encode() {
			t.Fatalf("token query\n got %s\nwant %s", values.Encode(), want.Encode())
		}
	}
}

// TestSageMakerAuthenticatesWithABearerToken covers both AWS credential modes'
// placement: a bearer token, and no request signature.
func TestSageMakerAuthenticatesWithABearerToken(t *testing.T) {
	secret := []byte(`{"access_key_id":"ABCDEFGHIJKLMNOP","secret_access_key":"abcdefghijklmnopabcdefghijklmnop","session_token":"fixture-session"}`)
	for _, profile := range []string{"", "sagemaker-openai-chat"} {
		c := sagemaker()
		if profile != "" {
			c.ProfileID, c.ProfileRevision = profile, ProfileRevision
		}
		req, _ := http.NewRequest(http.MethodPost, "https://runtime.sagemaker.us-west-2.amazonaws.com/endpoints/ep/openai/v1/chat/completions", strings.NewReader("{}"))
		applied, err := NewAuth(localPolicy()).Apply(context.Background(), req, c, secret, []byte("{}"))
		token, bearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer sagemaker-api-key-")
		if err != nil || !bearer || req.Header.Get("X-Amz-Date") != "" || req.Header.Get("X-Amz-Security-Token") != "" {
			t.Fatalf("%q authorization = %q, %v", profile, req.Header.Get("Authorization"), err)
		}
		if !applied.Contains("echoed sagemaker-api-key-" + token) {
			t.Fatal("the bearer token is not redacted")
		}
	}
}
