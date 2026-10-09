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
