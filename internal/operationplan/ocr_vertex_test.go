package operationplan_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
)

const vertexProcessResponse = `{"document":{"mimeType":"application/pdf","text":"Hello page one.\nSecond page text.","pages":[{"pageNumber":1,"layout":{"textAnchor":{"textSegments":[{"startIndex":"0","endIndex":"15"}]}}},{"pageNumber":2,"layout":{"textAnchor":{"textSegments":[{"startIndex":"16","endIndex":"33"}]}}}]}}`

func vertexTemplate(t *testing.T, prefixes []string) *operationplan.Template {
	t.Helper()
	template, err := operationplan.Compile(operationplan.Config{
		Provider: connectors.Config{
			Kind: "vertex_ai", AuthMode: "service_account", VendorID: "google-vertex",
			ProfileID: "vertex-document-processing-ocr", ProfileRevision: "1",
			Endpoint:    "https://us-documentai.googleapis.com/v1/projects/example-project/locations/us",
			CloudRegion: "us", CloudProject: "example-project",
			DocumentURIPrefixes: prefixes,
		},
		Operation: "ocr", Model: "processor-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func TestVertexOCRPlanBindsProcessorAndProjectsPages(t *testing.T) {
	template := vertexTemplate(t, nil)
	source, err := operationplan.Parse("mistral-ocr", []byte(`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,JVBERi0xLjQ="},"pages":[0,2]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := plan.Endpoint()
	if err != nil {
		t.Fatal(err)
	}
	want := "https://us-documentai.googleapis.com/v1/projects/example-project/locations/us/processors/processor-1:process"
	if endpoint != want {
		t.Fatalf("endpoint %q want %q", endpoint, want)
	}
	var body map[string]any
	if err := json.Unmarshal(plan.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if _, named := body["model"]; named {
		t.Fatalf("dispatch body carries a gateway model field: %s", plan.Body())
	}
	raw, _ := body["rawDocument"].(map[string]any)
	if raw["content"] != "JVBERi0xLjQ=" || raw["mimeType"] != "application/pdf" {
		t.Fatalf("dispatch body: %s", plan.Body())
	}
	decoded, err := plan.Decode([]byte(vertexProcessResponse))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Usage == nil || decoded.Usage.MediaUnits == nil || *decoded.Usage.MediaUnits != "2" {
		t.Fatalf("usage: %+v", decoded.Usage)
	}
	var out struct {
		Model string `json:"model"`
		Pages []struct {
			Index    int    `json:"index"`
			Markdown string `json:"markdown"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(decoded.Body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "route" || len(out.Pages) != 2 || out.Pages[1].Markdown != "Second page text." {
		t.Fatalf("projected result: %s", decoded.Body)
	}
	decoded, err = plan.Decode([]byte(`{"document":{"text":"only text"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Usage != nil {
		t.Fatalf("missing pages settled as priced usage: %+v", decoded.Usage)
	}
}

func TestVertexOCRPlanRefusesDocumentLocationsOutsidePrefixes(t *testing.T) {
	body := []byte(`{"gcsDocument":{"gcsUri":"gs://docs-bucket/reports/a.pdf","mimeType":"application/pdf"}}`)
	for _, prefixes := range [][]string{nil, {"gs://other-bucket/"}, {"gs://docs-bucket/reports2/"}} {
		template := vertexTemplate(t, prefixes)
		source, err := operationplan.Parse("vertex-document-processing-ocr", body, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := template.Bind(source, operationplan.Context{Route: "route"}); err == nil {
			t.Fatalf("document location bound under prefixes %v", prefixes)
		}
	}
	template := vertexTemplate(t, []string{"gs://docs-bucket/reports/"})
	source, err := operationplan.Parse("vertex-document-processing-ocr", body, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "route"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plan.Body()), "gs://docs-bucket/reports/a.pdf") {
		t.Fatalf("allowed document not in dispatch body: %s", plan.Body())
	}
	if _, err := plan.Decode([]byte(vertexProcessResponse)); err != nil {
		t.Fatal(err)
	}
}

func TestVertexOCRProbeBindsForCertification(t *testing.T) {
	template := vertexTemplate(t, nil)
	codec, ok := operationregistry.Lookup("vertex-document-processing-ocr")
	if !ok {
		t.Fatal("dialect not registered")
	}
	source, err := operationplan.Parse(codec.Identity.ID, codec.Probe("certification"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := template.Bind(source, operationplan.Context{Route: "certification"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Decode([]byte(vertexProcessResponse)); err != nil {
		t.Fatal(err)
	}
}

func TestVertexOCRPlanRefusesUnmappableSourceFields(t *testing.T) {
	template := vertexTemplate(t, []string{"gs://docs-bucket/"})
	for _, body := range []string{
		`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"table_format":"html"}`,
	} {
		source, err := operationplan.Parse("mistral-ocr", []byte(body), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := template.Bind(source, operationplan.Context{Route: "route"}); err == nil {
			t.Fatalf("unmappable source bound: %s", body)
		}
	}
}
