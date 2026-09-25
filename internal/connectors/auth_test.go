package connectors

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func localPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}
func TestNativeCloudURLAndAuthentication(t *testing.T) {
	for _, test := range []struct {
		config      Config
		family      openai.Family
		model, want string
	}{
		{Config{Kind: "azure_openai", Endpoint: "https://resource.example", Deployment: "prod", APIVersion: "2025-01-01-preview"}, openai.FamilyResponses, "prod", "https://resource.example/openai/deployments/prod/responses?api-version=2025-01-01-preview"},
		{Config{Kind: "gemini", Endpoint: "https://generativelanguage.googleapis.com/v1beta"}, openai.FamilyGemini, "gemini-pro", "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent"},
		{Config{Kind: "bedrock", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com"}, "bedrock", "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.model", "https://bedrock-runtime.us-east-1.amazonaws.com/model/arn:aws:bedrock:us-east-1:123456789012:inference-profile%2Fus.model/converse"},
	} {
		got, e := test.config.URL(test.family, test.model, false)
		if e != nil || got != test.want {
			t.Fatalf("address: %s %v", got, e)
		}
	}
	if got := DefaultEndpoint("vertex_ai", "global", "my-project"); got != "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/google" {
		t.Fatal(got)
	}
	auth := NewAuth(localPolicy())
	for kind, header := range map[string]string{"anthropic": "X-Api-Key", "gemini": "X-Goog-Api-Key", "azure_openai": "Api-Key", "openai": "Authorization"} {
		req, _ := http.NewRequest("POST", "https://provider.example", nil)
		if _, e := auth.Apply(context.Background(), req, Config{Kind: kind, AuthMode: "api_key"}, []byte("fixture-secret"), nil); e != nil || !strings.Contains(req.Header.Get(header), "fixture-secret") {
			t.Fatal(kind, e)
		}
	}
	req, _ := http.NewRequest("POST", "https://provider.example", nil)
	_, e := auth.Apply(context.Background(), req, Config{Kind: "anthropic", AuthMode: "headers", CredentialHeaders: []string{"X-Custom"}}, []byte(`{"X-Custom":"ok\r\nInjected: bad"}`), nil)
	if e == nil || req.Header.Get("X-Custom") != "" {
		t.Fatal("accepted credential header injection")
	}
}
func TestGoogleServiceAccountRefreshADCBoundsAndPublicTokenEgress(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if e := r.ParseForm(); e != nil || r.Form.Get("assertion") == "" {
			t.Error("missing signed service-account assertion")
		}
		w.Header().Set("Content-Type", "application/json")
		expiry := 3600
		if n == 1 {
			expiry = 1
		}
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d}`, n, expiry)
	}))
	defer tokenServer.Close()
	secret, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "fixture@project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": tokenServer.URL})
	cfg := Config{Kind: "vertex_ai", AuthMode: "service_account", CloudRegion: "global"}
	a := NewAuth(localPolicy())
	req, _ := http.NewRequest("POST", "https://provider.example", nil)
	if _, e = a.Apply(context.Background(), req, cfg, secret, nil); e != ErrAuthentication || calls.Load() != 0 {
		t.Fatalf("provider exception relaxed token egress: %v", e)
	}
	// Test-only token endpoint: production always keeps Google's public-only client.
	a.googleClient = a.client
	for _, want := range []string{"Bearer token-1", "Bearer token-2", "Bearer token-2"} {
		req, _ = http.NewRequest("POST", "https://provider.example", nil)
		if _, e = a.Apply(context.Background(), req, cfg, secret, nil); e != nil || req.Header.Get("Authorization") != want {
			t.Fatalf("token refresh: %s %v", req.Header.Get("Authorization"), e)
		}
	}
	path := filepath.Join(t.TempDir(), "adc.json")
	if e = os.WriteFile(path, secret, 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	cfg.AuthMode = "adc"
	if _, e = a.Apply(context.Background(), req, cfg, nil, nil); e != nil {
		t.Fatal("ADC", e)
	}
	done := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer blocked.Close()
	defer close(done)
	var document map[string]string
	_ = json.Unmarshal(secret, &document)
	document["token_uri"] = blocked.URL
	secret, _ = json.Marshal(document)
	cfg.AuthMode = "service_account"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, e = a.Apply(ctx, req, cfg, secret, nil); e != ErrAuthentication || time.Since(started) > time.Second {
		t.Fatalf("unbounded acquisition: %v", e)
	}
	body := &boundedAuthBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", authBodyLimit+1))), remaining: authBodyLimit}
	if _, e = io.ReadAll(body); e == nil {
		t.Fatal("unbounded auth response")
	}
}
func TestAWSStaticProcessAndSSOCredentialsSignOnce(t *testing.T) {
	cfg := Config{Kind: "bedrock", AuthMode: "static", CloudRegion: "us-east-1"}
	secret := []byte(`{"access_key_id":"ABCDEFGHIJKLMNOP","secret_access_key":"abcdefghijklmnopabcdefghijklmnop","session_token":"fixture-session"}`)
	a := NewAuth(localPolicy())
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/m/converse", strings.NewReader("{}"))
	if _, e := a.Apply(context.Background(), req, cfg, secret, []byte("{}")); e != nil || !strings.Contains(req.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") || req.Header.Get("X-Amz-Security-Token") != "fixture-session" {
		t.Fatalf("static signing: %v", e)
	}
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_PROFILE", "AWS_CA_BUNDLE"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	dir := t.TempDir()
	process := filepath.Join(dir, "credential-process")
	if e := os.WriteFile(process, []byte("#!/bin/sh\nprintf '%s' '{\"Version\":1,\"AccessKeyId\":\"PROCESSACCESSKEY123\",\"SecretAccessKey\":\"abcdefghijklmnopabcdefghijklmnop\",\"SessionToken\":\"process-session\"}'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	configPath := filepath.Join(dir, "config")
	if e := os.WriteFile(configPath, []byte("[default]\ncredential_process = "+process+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "absent"))
	cfg.AuthMode = "default_chain"
	req, _ = http.NewRequest("POST", "https://provider.example", nil)
	if _, e := a.Apply(context.Background(), req, cfg, nil, nil); e != nil || !strings.Contains(req.Header.Get("Authorization"), "PROCESSACCESSKEY123") {
		t.Fatalf("process credentials: %v", e)
	}
	var calls atomic.Int32
	ssoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Amz-Sso_bearer_token") != "fixture-sso-token" {
			t.Error("SSO bearer missing")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"roleCredentials":{"accessKeyId":"SSOACCESSKEY123456","secretAccessKey":"abcdefghijklmnopabcdefghijklmnop","sessionToken":"sso-session","expiration":%d}}`, time.Now().Add(time.Hour).UnixMilli())
	}))
	defer ssoServer.Close()
	tokenFile := filepath.Join(dir, "sso-token.json")
	token, _ := json.Marshal(map[string]string{"accessToken": "fixture-sso-token", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if e := os.WriteFile(tokenFile, token, 0600); e != nil {
		t.Fatal(e)
	}
	client := sso.NewFromConfig(aws.Config{Region: "us-east-1", HTTPClient: a.client, Retryer: func() aws.Retryer { return aws.NopRetryer{} }}, func(o *sso.Options) { o.BaseEndpoint = aws.String(ssoServer.URL) })
	provider := ssocreds.New(client, "123456789012", "fixture-role", "https://fixture.awsapps.com/start", func(o *ssocreds.Options) { o.CachedTokenFilepath = tokenFile })
	a.aws[cacheKey(cfg, nil)] = aws.NewCredentialsCache(provider)
	for range 2 {
		req, _ = http.NewRequest("POST", "https://provider.example", nil)
		if _, e := a.Apply(context.Background(), req, cfg, nil, nil); e != nil || !strings.Contains(req.Header.Get("Authorization"), "SSOACCESSKEY123456") {
			t.Fatalf("SSO: %v", e)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("SSO token was not cached")
	}
}
