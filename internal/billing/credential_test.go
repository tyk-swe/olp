package billing

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
)

const pollingKey = "sk-admin-xxxxxxxxxxxxxxxxxxxxxxxx"

func testServiceAccountInput(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	account := map[string]any{
		"type":         "service_account",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "billing@fixture-project.iam.gserviceaccount.com",
		"token_uri":    "https://oauth2.googleapis.com/token",
	}
	document, err := json.Marshal(map[string]any{"auth_mode": "service_account", "service_account": account})
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

func TestCredentialRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		source string
		input  string
		want   string
	}{
		{SourceOpenAI, pollingKey, pollingKey},
		{SourceAnthropic, pollingKey, pollingKey},
		{SourceAzureCostManagement, `{"auth_mode":"azure_default"}`, `{"auth_mode":"azure_default"}`},
		{SourceAzureCostManagement, `{"auth_mode":"azure_client_secret","tenant_id":"tenant","client_id":"client","client_secret":"secret"}`, `{"auth_mode":"azure_client_secret","tenant_id":"tenant","client_id":"client","client_secret":"secret"}`},
		{SourceAWS, `{"auth_mode":"static","access_key_id":"BILLINGKEY1234567890","secret_access_key":"billing-secret-example"}`, `{"auth_mode":"static","access_key_id":"BILLINGKEY1234567890","secret_access_key":"billing-secret-example"}`},
		{SourceAWS, `{"auth_mode":"workload"}`, `{"auth_mode":"workload"}`},
		{SourceGoogle, `{"auth_mode":"adc"}`, `{"auth_mode":"adc"}`},
	} {
		if err := ValidateCredential(tc.source, tc.input); err != nil {
			t.Fatalf("%s validate: %v", tc.source, err)
		}
		stored, err := encodeCredential(tc.source, tc.input)
		if err != nil {
			t.Fatalf("%s encode: %v", tc.source, err)
		}
		got, err := decodeCredential(tc.source, stored)
		if err != nil {
			t.Fatalf("%s decode: %v", tc.source, err)
		}
		if string(got) != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.source, got, tc.want)
		}
	}
}

func TestGoogleServiceAccountRoundTrip(t *testing.T) {
	input := testServiceAccountInput(t)
	stored, err := encodeCredential(SourceGoogle, input)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := encodeCredential(SourceGoogle, input)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(restored) {
		t.Fatal("stored Google credential is not deterministic")
	}
	got, err := decodeCredential(SourceGoogle, stored)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Source string `json:"source"`
		Mode   string `json:"auth_mode"`
	}
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Source != "" || decoded.Mode != "service_account" {
		t.Fatalf("decoded Google credential carries %+v", decoded)
	}
}

func TestValidateCredentialRejects(t *testing.T) {
	for _, tc := range []struct {
		source string
		input  string
	}{
		{"unknown_source", pollingKey},
		{SourceOpenAI, "short"},
		{SourceOpenAI, "has space in the key value"},
		{SourceAzureCostManagement, pollingKey},
		{SourceAzureCostManagement, `{"auth_mode":"other"}`},
		{SourceAzureCostManagement, `{"auth_mode":"azure_client_secret","tenant_id":"tenant"}`},
		{SourceAWS, pollingKey},
		{SourceAWS, `{"auth_mode":"other"}`},
		{SourceAWS, `{"auth_mode":"static","access_key_id":"BILLINGKEY1234567890"}`},
		{SourceGoogle, pollingKey},
		{SourceGoogle, `{"auth_mode":"other"}`},
		{SourceGoogle, `{"auth_mode":"service_account","service_account":{"type":"service_account","private_key":"invalid","client_email":"billing@fixture-project.iam.gserviceaccount.com","token_uri":"https://oauth2.googleapis.com/token"}}`},
	} {
		if err := ValidateCredential(tc.source, tc.input); err == nil {
			t.Fatalf("%s accepted %q", tc.source, tc.input)
		}
	}
}

func TestDecodeLegacyRawKey(t *testing.T) {
	for _, source := range []string{SourceOpenAI, SourceAnthropic} {
		got, err := decodeCredential(source, []byte(pollingKey))
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		if string(got) != pollingKey {
			t.Fatalf("%s: got %q", source, got)
		}
	}
	for _, source := range []string{SourceAzureCostManagement, SourceAWS} {
		if _, err := decodeCredential(source, []byte(pollingKey)); !errors.Is(err, errIncompatibleCredential) {
			t.Fatalf("%s: got %v, want incompatible", source, err)
		}
	}
	if _, err := decodeCredential(SourceGoogle, []byte(pollingKey)); err == nil {
		t.Fatal("raw key decoded as a Google credential")
	}
}

func TestDecodeCrossSource(t *testing.T) {
	openAIStored, err := encodeCredential(SourceOpenAI, pollingKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeCredential(SourceAnthropic, openAIStored); err != nil || string(got) != pollingKey {
		t.Fatalf("anthropic reading openai credential: %q, %v", got, err)
	}
	anthropicStored, err := encodeCredential(SourceAnthropic, pollingKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := decodeCredential(SourceOpenAI, anthropicStored); err != nil || string(got) != pollingKey {
		t.Fatalf("openai reading anthropic credential: %q, %v", got, err)
	}
	for _, source := range []string{SourceAzureCostManagement, SourceAWS, SourceGoogle} {
		if _, err := decodeCredential(source, openAIStored); err == nil {
			t.Fatalf("%s decoded a polling credential", source)
		}
	}
	azureStored, err := encodeCredential(SourceAzureCostManagement, `{"auth_mode":"azure_default"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredential(SourceOpenAI, azureStored); !errors.Is(err, errIncompatibleCredential) {
		t.Fatalf("openai reading azure credential: got %v, want incompatible", err)
	}
	awsStored, err := encodeCredential(SourceAWS, `{"auth_mode":"workload"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCredential(SourceAzureCostManagement, awsStored); !errors.Is(err, errIncompatibleCredential) {
		t.Fatalf("azure reading aws credential: got %v, want incompatible", err)
	}
	if _, err := decodeCredential(SourceOpenAI, []byte("first line\nsecond line")); err == nil {
		t.Fatal("garbage stored credential decoded")
	}
}

func TestReadCredentialInput(t *testing.T) {
	for _, tc := range []struct {
		source string
		input  string
		want   string
	}{
		{SourceOpenAI, `"sk-admin-key"`, "sk-admin-key"},
		{SourceAnthropic, `"sk-admin-key"`, "sk-admin-key"},
		{SourceAzureCostManagement, `{"auth_mode":"azure_default"}`, `{"auth_mode":"azure_default"}`},
		{SourceAWS, `{"auth_mode":"workload"}`, `{"auth_mode":"workload"}`},
		{SourceGoogle, `{"auth_mode":"adc"}`, `{"auth_mode":"adc"}`},
		{SourceGoogle, `"{\"auth_mode\":\"adc\"}"`, `{"auth_mode":"adc"}`},
	} {
		got, err := readCredentialInput(tc.source, json.RawMessage(tc.input))
		if err != nil {
			t.Fatalf("%s %s: %v", tc.source, tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.source, got, tc.want)
		}
	}
	if _, err := readCredentialInput(SourceOpenAI, json.RawMessage(`{"auth_mode":"adc"}`)); err == nil {
		t.Fatal("polling source accepted a bare object credential")
	}
	if _, err := readCredentialInput(SourceGoogle, json.RawMessage(`123`)); err == nil {
		t.Fatal("google source accepted a numeric credential")
	}
}
