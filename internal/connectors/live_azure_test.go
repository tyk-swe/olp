//go:build liveproviders

package connectors

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
)

func TestLiveAzureDocumentIntelligenceOperation(t *testing.T) {
	if os.Getenv("OLP_LIVE_OPERATION_DIALECT") != "azure-document-intelligence-ocr" {
		return
	}
	require := func(name string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		t.Fatalf("%s is required", name)
		return ""
	}
	codec, ok := operationregistry.Lookup("azure-document-intelligence-ocr")
	if !ok {
		t.Fatal("azure-document-intelligence-ocr not registered")
	}
	c := Config{Kind: "openai_compatible", AuthMode: "api_key", ProfileID: "azure-document-intelligence-ocr", ProfileRevision: ProfileRevision, VendorID: "azure-document-intelligence", Endpoint: require("OLP_LIVE_ENDPOINT")}
	secret := require("OLP_LIVE_AZURE_DOCUMENT_INTELLIGENCE_API_KEY")
	model := os.Getenv("OLP_LIVE_MODEL")
	if model == "" {
		model = "prebuilt-read"
	}
	body := codec.Probe(model)
	in, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := oif.NewRequest(oif.Descriptor{Operation: codec.Operation, Dialect: codec.Identity}, in)
	if err != nil {
		t.Fatal(err)
	}
	view, err := codec.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	wireRequest, err := codec.Wire(view)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := c.OperationURL(codec, model)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	for name, values := range wireRequest.Query {
		query[name] = values
	}
	parsed.RawQuery = query.Encode()
	client := &http.Client{Timeout: 60 * time.Second}
	do := func(method, target string, payload []byte) (int, http.Header, []byte) {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, target, reader)
		if err != nil {
			t.Fatal(err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if _, err := NewAuth(&egress.Policy{}).Apply(t.Context(), req, c, []byte(secret), payload); err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Clone(), raw
	}
	status, header, raw := do(http.MethodPost, parsed.String(), wireRequest.Body)
	if status != http.StatusAccepted && status != http.StatusOK {
		t.Fatalf("%s returned %d: %s", parsed.String(), status, raw)
	}
	if status == http.StatusAccepted {
		location := header.Get("Operation-Location")
		pollURL, err := url.Parse(location)
		if location == "" || err != nil || pollURL.Scheme != parsed.Scheme || pollURL.Host != parsed.Host {
			t.Fatalf("poll location leaves the bound origin: %q", location)
		}
		raw = nil
		for range 60 {
			pollStatus, _, pollRaw := do(http.MethodGet, location, nil)
			if pollStatus != http.StatusOK {
				t.Fatalf("poll returned %d: %s", pollStatus, pollRaw)
			}
			doc, err := oif.ParseJSON(pollRaw, oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			state, err := codec.Poll(doc)
			if err != nil {
				t.Fatal(err)
			}
			if state == operations.PollSucceeded {
				raw = pollRaw
				break
			}
			if state == operations.PollFailed {
				t.Fatalf("live operation failed: %s", pollRaw)
			}
			time.Sleep(time.Second)
		}
		if raw == nil {
			t.Fatal("live operation did not finish")
		}
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
		t.Fatalf("live result violates the %s contract: %v", codec.Identity.ID, err)
	}
}
