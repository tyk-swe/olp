package operationplan_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operations"
)

func azureTemplate(t *testing.T) *operationplan.Template {
	t.Helper()
	c := config("azure-document-intelligence-ocr", "ocr")
	c.Provider.VendorID, c.Provider.AuthMode = "azure-document-intelligence", "api_key"
	template, err := operationplan.Compile(c)
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func TestOCRMappingRefusals(t *testing.T) {
	template := azureTemplate(t)
	for _, body := range []string{
		`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"table_format":"html"}`,
		`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"include_image_base64":true}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:text/plain,hello"}}`,
	} {
		source, err := operationplan.Parse("mistral-ocr", []byte(body), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient}); err == nil {
			t.Fatalf("refused source bound: %s", body)
		}
	}
}

func TestAzureSubmitAndPoll(t *testing.T) {
	template := azureTemplate(t)
	source, err := operationplan.Parse("mistral-ocr", []byte(`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/a.pdf"},"pages":[0,2]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route", ClientContract: operations.RawVectorClient})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Transport() != operations.TransportPoll202 {
		t.Fatalf("transport: %q", plan.Transport())
	}
	method, submit, body, err := plan.Submit()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(submit)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || u.Path != "/v1/documentintelligence/documentModels/vendor-model:analyze" {
		t.Fatalf("submit: %s %s", method, submit)
	}
	q := u.Query()
	if q.Get("api-version") != "2024-11-30" || q.Get("outputContentFormat") != "markdown" || q.Get("pages") != "1,3" {
		t.Fatalf("query: %s", u.RawQuery)
	}
	if string(body) != `{"urlSource":"https://example.com/a.pdf"}` {
		t.Fatalf("body: %s", body)
	}
	header := http.Header{"Operation-Location": {"https://example.com/v1/documentintelligence/documentModels/vendor-model/analyzeResults/abc?api-version=2024-11-30"}}
	loc, err := plan.PollLocation(submit, header)
	if err != nil || !strings.Contains(loc, "analyzeResults/abc") {
		t.Fatalf("poll location: %q %v", loc, err)
	}
	for _, bad := range []string{
		"https://evil.example.net/v1/documentintelligence/documentModels/vendor-model/analyzeResults/abc?api-version=2024-11-30",
		"http://example.com/v1/documentintelligence/documentModels/vendor-model/analyzeResults/abc",
		"https://user:pass@example.com/v1/x",
		"https://example.com/v1/x#frag",
		"/relative/path",
		"",
	} {
		h := http.Header{}
		if bad != "" {
			h.Set("Operation-Location", bad)
		}
		if _, err := plan.PollLocation(submit, h); err == nil {
			t.Fatalf("foreign poll location admitted: %q", bad)
		}
	}
	if state, err := plan.PollState([]byte(`{"status":"running"}`)); err != nil || state != operations.PollPending {
		t.Fatalf("poll state: %q %v", state, err)
	}
	if state, err := plan.PollState([]byte(`{"status":"succeeded"}`)); err != nil || state != operations.PollSucceeded {
		t.Fatalf("poll state: %q %v", state, err)
	}
	method, cancelURL, ok := plan.CancelPoll(loc)
	if !ok || method != http.MethodDelete || cancelURL != loc {
		t.Fatalf("cancel: %s %s %v", method, cancelURL, ok)
	}
	decoded, err := plan.Decode([]byte(`{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"vendor-model","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One\n\n# Two","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":5}]},{"pageNumber":3,"spans":[{"offset":7,"length":5}]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Usage == nil || decoded.Usage.MediaUnits == nil || *decoded.Usage.MediaUnits != "2" {
		t.Fatalf("usage: %+v", decoded.Usage)
	}
	if !strings.Contains(string(decoded.Body), `"pages_processed":2`) || !strings.Contains(string(decoded.Body), `"index":2`) {
		t.Fatalf("projected: %s", decoded.Body)
	}
}
