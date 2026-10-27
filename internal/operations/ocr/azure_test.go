package ocr

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestAzureProbeUsesSupportedImage(t *testing.T) {
	var probe struct {
		Base64Source string `json:"base64Source"`
	}
	if err := json.Unmarshal(azureDefinition().Probe("model"), &probe); err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(probe.Base64Source)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if image.Width < 50 || image.Height < 50 || image.Width > 10000 || image.Height > 10000 {
		t.Fatalf("probe image %dx%d violates https://learn.microsoft.com/en-us/azure/ai-services/document-intelligence/model-overview#input-requirements, verified 2026-10-27", image.Width, image.Height)
	}
}

func azureRequestDoc(t *testing.T, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(AzureDocumentIntelligence)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func azureResultDoc(t *testing.T, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(AzureDocumentIntelligence)}, d, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAzureRequestFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"prebuilt-layout","urlSource":"https://example.com/a.pdf"}`,
		`{"model":"prebuilt-read","base64Source":"QUJD","pages":"1-2,4"}`,
		`{"model":"m","base64Source":"QUJD"}`,
	} {
		if _, err := parseAzureRequest(azureRequestDoc(t, body)); err != nil {
			t.Fatalf("valid request refused: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"model":"m","urlSource":"https://x","base64Source":"QUJD"}`,
		`{"model":"m"}`,
		`{"model":"m","urlSource":"http://example.com/a.pdf"}`,
		`{"model":"m","urlSource":"data:application/pdf;base64,QUJD"}`,
		`{"model":"m","base64Source":"not base64!"}`,
		`{"model":"m","base64Source":""}`,
		`{"model":"m","urlSource":"https://x","pages":"0"}`,
		`{"model":"m","urlSource":"https://x","pages":"3-2"}`,
		`{"model":"m","urlSource":"https://x","pages":"1,,2"}`,
		`{"model":"m","urlSource":"https://x","locale":"en-US"}`,
		`{"model":"m","urlSource":"https://x","outputContentFormat":"text"}`,
		`{"urlSource":"https://x"}`,
	} {
		if _, err := parseAzureRequest(azureRequestDoc(t, body)); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

const azureSucceeded = `{"status":"succeeded","createdDateTime":"2026-01-01T00:00:00Z","lastUpdatedDateTime":"2026-01-01T00:00:01Z","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One\n\n# Two","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":5}]},{"pageNumber":2,"spans":[{"offset":7,"length":5}]}]}}`

func TestAzureResultAndUsage(t *testing.T) {
	r := azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://x"}`)
	v, err := parseAzureResult(r, azureResultDoc(t, azureSucceeded))
	if err != nil {
		t.Fatal(err)
	}
	if *v.(Result).Usage.MediaUnits != "2" {
		t.Fatalf("media units: %+v", v.(Result).Usage)
	}
	missing := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One"}}`
	v, err = parseAzureResult(r, azureResultDoc(t, missing))
	if err != nil {
		t.Fatal(err)
	}
	if v.(Result).Usage != nil {
		t.Fatalf("missing pages invented usage: %+v", v.(Result).Usage)
	}
	textResult := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-read","stringIndexType":"utf16CodeUnit","contentFormat":"text","content":"One","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":3}]}]}}`
	if _, err = parseAzureResult(r, azureResultDoc(t, textResult)); err != nil {
		t.Fatalf("text content format refused: %v", err)
	}
	for _, body := range []string{
		`{"status":"running","analyzeResult":{"modelId":"m","content":"x"}}`,
		`{"status":"failed","error":{"code":"x","message":"y"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"html","content":"x"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown","content":"x","pages":[{"pageNumber":0}]}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown","content":"x","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":9}]}]}}`,
		`{"status":"succeeded"}`,
	} {
		if _, err := parseAzureResult(r, azureResultDoc(t, body)); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestAzureOutputTextCoverage(t *testing.T) {
	texts, err := azureOutputText(azureResultDoc(t, azureSucceeded))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tx := range texts {
		if tx.Value == "# One\n\n# Two" {
			found = true
		}
	}
	if !found {
		t.Fatalf("content not inspected: %+v", texts)
	}
}

func TestAzurePollState(t *testing.T) {
	for body, want := range map[string]string{
		`{"status":"notStarted"}`: "pending",
		`{"status":"running"}`:    "pending",
		`{"status":"succeeded"}`:  "succeeded",
		`{"status":"failed"}`:     "failed",
	} {
		d, err := oif.ParseJSON([]byte(body), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := azurePollState(d)
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", body, got, err)
		}
	}
	d, _ := oif.ParseJSON([]byte(`{"status":"mystery"}`), oif.Limits{})
	if _, err := azurePollState(d); err == nil {
		t.Fatal("unknown poll status admitted")
	}
}

func TestAzureWire(t *testing.T) {
	r := azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://example.com/a.pdf","pages":"1,3-4"}`)
	view, err := parseAzureRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := azureWire(view)
	if err != nil {
		t.Fatal(err)
	}
	if wire.Query.Get("api-version") != "2024-11-30" || wire.Query.Get("outputContentFormat") != "markdown" || wire.Query.Get("stringIndexType") != "utf16CodeUnit" || wire.Query.Get("pages") != "1,3-4" {
		t.Fatalf("query: %v", wire.Query)
	}
	if string(wire.Body) != `{"urlSource":"https://example.com/a.pdf"}` {
		t.Fatalf("body: %s", wire.Body)
	}
	readView, err := parseAzureRequest(azureRequestDoc(t, `{"model":"prebuilt-read","base64Source":"QUJD"}`))
	if err != nil {
		t.Fatal(err)
	}
	readWire, err := azureWire(readView)
	if err != nil {
		t.Fatal(err)
	}
	if readWire.Query.Get("outputContentFormat") != "text" {
		t.Fatalf("read query format = %q, want text (markdown is layout-only)", readWire.Query.Get("outputContentFormat"))
	}
}
