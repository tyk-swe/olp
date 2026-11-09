package access

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
)

type uniqueInner struct {
	Name string `json:"name"`
}

type uniqueDocument struct {
	Zone  nullable.Nullable[string]      `json:"zone,omitempty"`
	Inner nullable.Nullable[uniqueInner] `json:"inner,omitempty"`
	At    time.Time                      `json:"at,omitempty"`
	ID    uuid.UUID                      `json:"id,omitempty"`
}

func decodeUniqueBody(body string) (uniqueDocument, error) {
	request, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	var document uniqueDocument
	err := DecodeUnique(request, &document, 1<<10)
	return document, err
}

func TestDecodeUniqueGeneratedContractShapes(t *testing.T) {
	document, err := decodeUniqueBody(`{"zone":"Europe/Berlin","inner":{"name":"a"},"at":"2026-01-02T03:04:05Z","id":"6f1c1a52-6d1f-4f5c-9a51-0d8f1b5c2e10"}`)
	if err != nil {
		t.Fatalf("contract values: %v", err)
	}
	zone, _ := document.Zone.Get()
	inner, _ := document.Inner.Get()
	if zone != "Europe/Berlin" || inner.Name != "a" || document.At.IsZero() || document.ID == uuid.Nil {
		t.Fatalf("decoded %+v", document)
	}
	cleared, err := decodeUniqueBody(`{"zone":null}`)
	if err != nil || !cleared.Zone.IsNull() {
		t.Fatalf("explicit null: %v %+v", err, cleared)
	}
	for name, body := range map[string]string{
		"alias inside nullable": `{"inner":{"Name":"a"}}`,
		"null scalar":           `{"at":null}`,
		"null uuid":             `{"id":null}`,
	} {
		if _, err := decodeUniqueBody(body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
