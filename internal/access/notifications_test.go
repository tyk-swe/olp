package access

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
