//go:build liveproviders

package connectors

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
)

func TestVertexDocumentProcessingLive(t *testing.T) {
	require := func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		t.Fatalf("%s is required", name)
		return ""
	}
	codec, ok := operationregistry.Lookup("vertex-document-processing-ocr")
	if !ok {
		t.Fatal("dialect not registered")
	}
	c := Config{
		Kind: "vertex_ai", AuthMode: "adc", VendorID: "google-vertex",
		ProfileID: "vertex-document-processing-ocr", ProfileRevision: ProfileRevision,
		CloudProject: require("OLP_LIVE_DOCUMENTAI_PROJECT"),
		CloudRegion:  require("OLP_LIVE_DOCUMENTAI_LOCATION"),
	}
	c.Endpoint = DefaultProfileEndpoint(c.Kind, c.ProfileID, c.CloudRegion, c.CloudProject)
	model := require("OLP_LIVE_MODEL")
	body := codec.Probe(model)
	endpoint, err := c.OperationURL(codec, model)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if _, err := NewAuth(&egress.Policy{}).Apply(t.Context(), req, c, nil, body); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s returned %d: %s", endpoint, resp.StatusCode, raw)
	}
	in, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := oif.NewRequest(oif.Descriptor{Operation: codec.Operation, Dialect: codec.Identity}, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := oif.ParseJSON(raw, oif.Limits{})
	if err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	result, err := oif.NewResult(oif.Descriptor{Operation: codec.Operation, Dialect: codec.Identity}, out, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Result(request, result); err != nil {
		t.Fatalf("live result violates the vertex document processing contract: %v", err)
	}
}
