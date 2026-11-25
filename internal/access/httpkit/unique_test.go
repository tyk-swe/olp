package httpkit

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

func TestDecodeRequiresExactJSONMediaType(t *testing.T) {
	const body = `{"zone":"Europe/Berlin"}`
	for _, decode := range []struct {
		name string
		run  func(*http.Request, *uniqueDocument) error
	}{
		{"Decode", func(request *http.Request, document *uniqueDocument) error { return Decode(request, document) }},
		{"DecodeUnique", func(request *http.Request, document *uniqueDocument) error {
			return DecodeUnique(request, document, 1<<10)
		}},
	} {
		t.Run(decode.name+"/case-insensitive with charset", func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			request.Header.Set("Content-Type", "Application/JSON; charset=utf-8")
			var document uniqueDocument
			if err := decode.run(request, &document); err != nil {
				t.Fatalf("Application/JSON with charset: %v", err)
			}
			zone, _ := document.Zone.Get()
			if zone != "Europe/Berlin" {
				t.Fatalf("decoded zone = %q", zone)
			}
		})
		t.Run(decode.name+"/jsonx rejected", func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/jsonx")
			var document uniqueDocument
			err := decode.run(request, &document)
			problem, ok := ProblemFor(err)
			if err == nil || !ok || problem.Status != 415 || problem.Code != "unsupported_media_type" {
				t.Fatalf("application/jsonx err = %v, want 415 unsupported_media_type", err)
			}
		})
	}
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

func TestDecodeUniqueTellsEachRefusalApart(t *testing.T) {
	for name, refusal := range map[string]struct{ body, detail string }{
		"over the byte limit": {`{"zone":"` + strings.Repeat("a", 1<<10) + `"}`, "exceeds"},
		"duplicate member":    {`{"zone":"a","zone":"b"}`, "unambiguous"},
		"nested too deep":     {strings.Repeat("[", 200) + strings.Repeat("]", 200), "unambiguous"},
		"second document":     {`{"zone":"a"} {"zone":"b"}`, "unambiguous"},
		"undeclared member":   {`{"Zone":"a"}`, "declared names"},
		"mistyped member":     {`{"zone":7}`, "not valid for this operation"},
	} {
		_, err := decodeUniqueBody(refusal.body)
		problem, ok := ProblemFor(err)
		if err == nil || !ok || problem.Status != 400 || problem.Code != "invalid_json" || !strings.Contains(problem.Detail, refusal.detail) {
			t.Errorf("%s: problem = %#v, want 400 invalid_json mentioning %q", name, problem, refusal.detail)
		}
	}
}
