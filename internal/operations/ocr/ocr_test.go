package ocr

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func request(t *testing.T, dialect, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(dialect)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func result(t *testing.T, dialect, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(dialect)}, d, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOCRRequestFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","document":{"type":"document_url","document_url":"https://example.com/a.pdf"}}`,
		`{"model":"m","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA","document_name":"a.pdf"},"pages":[0,2],"include_image_base64":true,"image_limit":4,"image_min_size":0,"table_format":"html","extract_header":true,"extract_footer":false,"id":"job-1"}`,
		`{"model":"m","document":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}`,
		`{"model":"m","document":{"type":"image_url","image_url":"data:image/png;base64,AAA"}}`,
	} {
		if _, err := parseRequest(request(t, Mistral, body), Mistral); err != nil {
			t.Fatalf("valid request refused: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"model":"m","document":{"type":"file","file_id":"f-1"}}`,
		`{"model":"m","document":{"type":"document_url","document_url":"http://example.com/a.pdf"}}`,
		`{"model":"m","document":{"type":"image_url","image_url":{"url":"ftp://x"}}}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x","bbox_annotation_format":{}}}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"document_annotation":"x"}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"image_annotation":"x"}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"confidence_scores_granularity":"line"}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"include_blocks":true}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"pages":[-1]}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"table_format":"csv"}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"include_image_base64":"yes"}`,
		`{"model":"m","document":{"type":"document_url","document_url":"https://x"},"stream":true}`,
		`{"document":{"type":"document_url","document_url":"https://x"}}`,
	} {
		if _, err := parseRequest(request(t, Mistral, body), Mistral); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

func TestOCRResultAndUsage(t *testing.T) {
	r := request(t, Mistral, `{"model":"m","document":{"type":"document_url","document_url":"https://x"}}`)
	good := `{"model":"mistral-ocr-latest","pages":[{"index":0,"markdown":"# A","images":[{"id":"img","image_base64":"AAA"}],"dimensions":{"dpi":72,"height":10,"width":10},"header":"h","footer":"f","tables":[{"id":"t","content":"c"}]}],"usage_info":{"pages_processed":1,"doc_size_bytes":9},"document_annotation":null}`
	v, err := parseResult(r, result(t, Mistral, good), Mistral)
	if err != nil {
		t.Fatal(err)
	}
	if *v.(Result).Usage.MediaUnits != "1" {
		t.Fatalf("media units: %+v", v.(Result).Usage)
	}
	for _, body := range []string{
		`{"pages":[],"usage_info":{"pages_processed":0}}`,
		`{"model":"m","pages":[{"index":"0","markdown":"x"}],"usage_info":{"pages_processed":1}}`,
		`{"model":"m","pages":[{"index":0,"markdown":1}],"usage_info":{"pages_processed":1}}`,
		`{"model":"m","pages":[],"usage_info":{"pages_processed":-1}}`,
		`{"model":"m","pages":[],"usage_info":{}}`,
	} {
		if _, err := parseResult(r, result(t, Mistral, body), Mistral); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestOCROutputTextCoverage(t *testing.T) {
	res := result(t, Mistral, `{"model":"m","pages":[{"index":0,"markdown":"# A","header":"h","footer":"f","tables":[{"id":"t","content":"cell","format":"markdown"}]}],"usage_info":{"pages_processed":1}}`)
	texts, err := outputText(res, Mistral)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range texts {
		got = append(got, tx.Value)
	}
	if strings.Join(got, ",") != "# A,h,f,cell" {
		t.Fatalf("coverage: %v", got)
	}
	// Every uninspected field fails closed unless it carries no text.
	for _, body := range []string{
		`{"model":"m","pages":[{"index":0,"markdown":"x","images":[{"id":"i","image_base64":"AAA"}]}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","images":[{"id":"i","image_annotation":"seen"}]}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","extra":"surprise"}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","extra":{"nested":"s"}}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","images":"bogus"}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","dimensions":{"label":"seen"}}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","tables":[{"content":{"cells":["a"]}}]}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","tables":[{"caption":"uninspected"}]}]}`,
		`{"model":"m","pages":[],"document_annotation":{"author":"x"}}`,
		`{"model":"m","pages":[],"extra":"surprise"}`,
		`{"model":"m","pages":[],"extra":["s"]}`,
	} {
		if _, err := outputText(result(t, Mistral, body), Mistral); err == nil {
			t.Fatalf("uninspectable output admitted: %s", body)
		}
	}
	for _, body := range []string{
		`{"model":"m","pages":[{"index":0,"markdown":"x","images":[{"id":"i","top_left_x":1,"top_left_y":2,"bottom_right_x":3,"bottom_right_y":4}]}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","dimensions":{"dpi":72,"width":10,"height":10}}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","dimensions":null}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","extra":[]}]}`,
		`{"model":"m","pages":[{"index":0,"markdown":"x","extra":{}}]}`,
		`{"model":"m","pages":[],"document_annotation":null,"extra":42,"other":false}`,
	} {
		if _, err := outputText(result(t, Mistral, body), Mistral); err != nil {
			t.Fatalf("content-free fields refused: %s: %v", body, err)
		}
	}
}

func TestAzureRequestFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"prebuilt-layout","urlSource":"https://example.com/a.pdf"}`,
		`{"model":"prebuilt-read","base64Source":"QUJD","pages":"1-2,4"}`,
		`{"model":"m","base64Source":"QUJD"}`,
	} {
		if _, err := parseRequest(request(t, AzureDocumentIntelligence, body), AzureDocumentIntelligence); err != nil {
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
		if _, err := parseRequest(request(t, AzureDocumentIntelligence, body), AzureDocumentIntelligence); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

const azureSucceeded = `{"status":"succeeded","createdDateTime":"2026-01-01T00:00:00Z","lastUpdatedDateTime":"2026-01-01T00:00:01Z","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One\n\n# Two","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":5}]},{"pageNumber":2,"spans":[{"offset":7,"length":5}]}]}}`

func TestAzureResultAndUsage(t *testing.T) {
	r := request(t, AzureDocumentIntelligence, `{"model":"prebuilt-layout","urlSource":"https://x"}`)
	v, err := parseResult(r, result(t, AzureDocumentIntelligence, azureSucceeded), AzureDocumentIntelligence)
	if err != nil {
		t.Fatal(err)
	}
	if *v.(Result).Usage.MediaUnits != "2" {
		t.Fatalf("media units: %+v", v.(Result).Usage)
	}
	missing := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One"}}`
	v, err = parseResult(r, result(t, AzureDocumentIntelligence, missing), AzureDocumentIntelligence)
	if err != nil {
		t.Fatal(err)
	}
	if v.(Result).Usage != nil {
		t.Fatalf("missing pages invented usage: %+v", v.(Result).Usage)
	}
	for _, body := range []string{
		`{"status":"running","analyzeResult":{"modelId":"m","content":"x"}}`,
		`{"status":"failed","error":{"code":"x","message":"y"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"text","content":"x"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown"}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown","content":"x","pages":[{"pageNumber":0}]}}`,
		`{"status":"succeeded","analyzeResult":{"modelId":"m","contentFormat":"markdown","content":"x","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":9}]}]}}`,
		`{"status":"succeeded"}`,
	} {
		if _, err := parseResult(r, result(t, AzureDocumentIntelligence, body), AzureDocumentIntelligence); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestAzureOutputTextCoverage(t *testing.T) {
	texts, err := outputText(result(t, AzureDocumentIntelligence, azureSucceeded), AzureDocumentIntelligence)
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
	r := request(t, AzureDocumentIntelligence, `{"model":"prebuilt-layout","urlSource":"https://example.com/a.pdf","pages":"1,3-4"}`)
	view, err := parseRequest(r, AzureDocumentIntelligence)
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
}
