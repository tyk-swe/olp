package connectors

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/operationregistry"
)

func TestDocumentURIAuthority(t *testing.T) {
	prefixes, err := NormalizeDocumentURIPrefixes("vertex_ai", []string{"gs://docs-bucket/reports", "gs://docs-bucket/reports/"})
	if err != nil || !reflect.DeepEqual(prefixes, []string{"gs://docs-bucket/reports/"}) {
		t.Fatalf("prefixes=%v %v", prefixes, err)
	}
	config := Config{DocumentURIPrefixes: prefixes}
	for uri, want := range map[string]bool{
		"gs://docs-bucket/reports/a.pdf":     true,
		"gs://docs-bucket/reports2/a.pdf":    false,
		"gs://other-bucket/reports/a.pdf":    false,
		"gs://docs-bucket/reports/../secret": false,
		"gs://docs-bucket/reports/%2e%2e/x":  false,
		"https://docs-bucket/reports/a.pdf":  false,
		"gs://docs-bucket/reports/a.pdf?x=1": false,
	} {
		if got := config.AllowsDocumentURI(uri); got != want {
			t.Errorf("%q authority=%v want=%v", uri, got, want)
		}
	}
	if _, err := NormalizeDocumentURIPrefixes("openai", prefixes); err == nil {
		t.Fatal("document authority added to another connector")
	}
	clone := config.Clone()
	clone.DocumentURIPrefixes[0] = "gs://other-bucket/"
	if config.DocumentURIPrefixes[0] != "gs://docs-bucket/reports/" {
		t.Fatal("mutable document authority escaped revision clone")
	}
	if (Config{}).AllowsDocumentURI("gs://docs-bucket/reports/a.pdf") {
		t.Fatal("missing operator grant authorized a document location")
	}
}

func TestDocumentAIAddressingAndDestination(t *testing.T) {
	config := Config{
		Kind: "vertex_ai", AuthMode: "service_account", VendorID: "google-vertex",
		ProfileID: "vertex-document-processing-ocr", ProfileRevision: "1",
		CloudRegion: "eu", CloudProject: "example-project",
		Endpoint: DefaultProfileEndpoint("vertex_ai", "vertex-document-processing-ocr", "eu", "example-project"),
	}
	if config.Endpoint != "https://eu-documentai.googleapis.com/v1/projects/example-project/locations/eu" {
		t.Fatalf("endpoint: %q", config.Endpoint)
	}
	dialect, ok := operationregistry.Lookup("vertex-document-processing-ocr")
	if !ok {
		t.Fatal("dialect not registered")
	}
	endpoint, err := config.OperationURL(dialect, "processor-1")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://eu-documentai.googleapis.com/v1/projects/example-project/locations/eu/processors/processor-1:process"
	if endpoint != want {
		t.Fatalf("operation url %q want %q", endpoint, want)
	}
	for raw, allowed := range map[string]bool{
		want: true,
		"https://aiplatform.eu.rep.googleapis.com/v1/projects/example-project/locations/eu/processors/processor-1:process": false,
		"https://us-documentai.googleapis.com/v1/projects/example-project/locations/eu/processors/processor-1:process":     false,
		"https://eu-documentai.googleapis.com.evil.example/v1/projects/example-project/locations/eu/processors/p:process":  false,
		"http://eu-documentai.googleapis.com/v1/projects/example-project/locations/eu/processors/processor-1:process":      false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := config.vertexDestination(u); got != allowed {
			t.Errorf("destination %q = %v want %v", raw, got, allowed)
		}
	}
}
