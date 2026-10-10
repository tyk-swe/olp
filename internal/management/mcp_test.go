package management

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMCPRedactsSecretsAtEveryDepth(t *testing.T) {
	var result any
	json.Unmarshal([]byte(`{"id":"key-id","secret":"one-time-key","items":[{"access_token":"oauth","csrf_token":"csrf","private_key":"pem","secret_bindings":{"a":"secret"},"input_tokens":12}],"credential_id":"version","client_secret":"oauth-client"}`), &result)
	redactMCP(result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"one-time-key", "oauth", "csrf", "pem", "secret_bindings"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("result contains %s: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), `"input_tokens":12`) || !strings.Contains(string(encoded), `"credential_id":"version"`) {
		t.Fatalf("safe metadata lost: %s", encoded)
	}
}

func TestMCPWriterBoundsResultsAndPreservesFirstStatus(t *testing.T) {
	writer := &mcpWriter{header: http.Header{}, status: 200}
	writer.WriteHeader(201)
	writer.WriteHeader(409)
	if writer.status != 201 {
		t.Fatal("status overwritten")
	}
	if _, err := writer.Write(make([]byte, (16<<20)+1)); err == nil || !writer.overflow || writer.body.Len() != 0 {
		t.Fatal("unbounded response")
	}
}

func TestMCPRedactionPreservesExactSchemaNumbers(t *testing.T) {
	value, err := decodeMCPBody([]byte(`{"catalog":{"minimum":9007199254740993,"multipleOf":0.1234567890123456789},"items":[{"input_tokens":9007199254740993,"access_token":"private"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	redactMCP(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, exact := range []string{`"minimum":9007199254740993`, `"multipleOf":0.1234567890123456789`, `"input_tokens":9007199254740993`} {
		if !strings.Contains(string(encoded), exact) {
			t.Fatalf("management result changed: %s", encoded)
		}
	}
	if strings.Contains(string(encoded), "private") {
		t.Fatal("credential survived redaction")
	}
	if _, err := decodeMCPBody([]byte(`{} {}`)); err == nil {
		t.Fatal("accepted multiple management responses")
	}
}
