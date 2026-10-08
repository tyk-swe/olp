package connectors

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestCallerCredentialsUseOnlyStatelessAuthentication(t *testing.T) {
	for _, mode := range []string{"api_key", "headers", "static"} {
		if err := ValidateCredentialSource("caller", "openai", mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"none", "adc", "service_account", "azure_default", "azure_client_secret", "default_chain", "ibm_iam", AuthGrant, AuthStaticCredential} {
		if ValidateCredentialSource("caller", "openai", mode) == nil {
			t.Fatalf("accepted cached or plugin mode %s", mode)
		}
	}
	if ValidateCredentialSource("caller", "plugin", "api_key") == nil || ValidateCredentialSource("unknown", "openai", "api_key") == nil {
		t.Fatal("accepted undeclared source")
	}
	a := NewAuth(&egress.Policy{})
	cfg := Config{Kind: "bedrock", Endpoint: "https://bedrock-runtime.us-east-1.amazonaws.com", AuthMode: "static", CredentialSource: "caller", CloudRegion: "us-east-1"}
	req := httptest.NewRequest("POST", cfg.Endpoint+"/model/test/invoke", nil)
	secret := []byte(`{"access_key_id":"AKIA1234567890ABCDEF","secret_access_key":"1234567890123456789012345678901234567890","session_token":"temporary-token"}`)
	sensitive, err := a.Apply(context.Background(), req, cfg, secret, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") == "" {
		t.Fatal("static caller credential was not signed")
	}
	if len(a.aws) != 0 || len(a.tokens) != 0 || len(a.azure) != 0 || len(a.ibmTokens) != 0 {
		t.Fatal("caller credentials entered shared authentication cache")
	}
	if sensitive.Redact("1234567890123456789012345678901234567890") == "1234567890123456789012345678901234567890" {
		t.Fatal("derived credential not redacted")
	}
}
