package notifications

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestDestinationSecret(t *testing.T) {
	longWebhook, err := json.Marshal(strings.Repeat("k", 1025))
	if err != nil {
		t.Fatal(err)
	}
	largeObject, err := json.Marshal(map[string]string{"credential": strings.Repeat("x", 16384)})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		typ     string
		raw     json.RawMessage
		want    []byte
		wantErr bool
	}{
		{name: "webhook string", typ: "webhook", raw: json.RawMessage(`"k"`), want: []byte("k")},
		{name: "empty webhook string", typ: "webhook", raw: json.RawMessage(`""`), wantErr: true},
		{name: "long webhook string", typ: "webhook", raw: longWebhook, wantErr: true},
		{name: "webhook object", typ: "webhook", raw: json.RawMessage(`{}`), wantErr: true},
		{name: "slack object", typ: "slack", raw: json.RawMessage(`{"webhook_url":"x"}`), want: []byte(`{"webhook_url":"x"}`)},
		{name: "slack string", typ: "slack", raw: json.RawMessage(`"s"`), wantErr: true},
		{name: "oversized slack object", typ: "slack", raw: largeObject, wantErr: true},
		{name: "webhook null", typ: "webhook", raw: json.RawMessage(`null`)},
		{name: "slack null", typ: "slack", raw: json.RawMessage(`null`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := destinationSecret(test.typ, test.raw)
			if (err != nil) != test.wantErr {
				t.Fatalf("destinationSecret() error = %v, want error %v", err, test.wantErr)
			}
			if err == nil && !bytes.Equal(got, test.want) {
				t.Errorf("destinationSecret() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestChatDestinationBindsEquivalentDefaultPorts(t *testing.T) {
	for _, typ := range []string{"slack", "msteams", "discord"} {
		t.Run(typ, func(t *testing.T) {
			for _, tc := range []struct {
				origin, hook string
				valid        bool
			}{
				{"https://hooks.example.invalid:443", "https://hooks.example.invalid/path", true},
				{"https://hooks.example.invalid", "https://hooks.example.invalid:443/path", true},
				{"https://hooks.example.invalid:8443", "https://hooks.example.invalid:8443/path", true},
				{"http://hooks.example.invalid:80", "http://hooks.example.invalid/path", true},
				{"http://hooks.example.invalid", "http://hooks.example.invalid:80/path", true},
				{"https://hooks.example.invalid:8443", "https://hooks.example.invalid/path", false},
				{"https://hooks.example.invalid", "https://other.example.invalid/path", false},
				{"https://hooks.example.invalid", "http://hooks.example.invalid/path", false},
			} {
				raw, err := json.Marshal(map[string]string{"webhook_url": tc.hook})
				if err != nil {
					t.Fatal(err)
				}
				s := &Server{egress: egress.Policy{PlainHTTPHosts: []string{"hooks.example.invalid"}}}
				err = s.checkDestination(destination{Type: typ, URL: tc.origin}, raw, true, true)
				if (err == nil) != tc.valid {
					t.Errorf("%s binding %s valid=%t: %v", tc.origin, tc.hook, tc.valid, err)
				}
			}
		})
	}
}
