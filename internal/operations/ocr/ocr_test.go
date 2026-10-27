package ocr

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func request(t *testing.T, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(Mistral)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func result(t *testing.T, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(Mistral)}, d, oif.Complete)
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
		if _, err := parseRequest(request(t, body)); err != nil {
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
		if _, err := parseRequest(request(t, body)); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

func TestOCRResultAndUsage(t *testing.T) {
	r := request(t, `{"model":"m","document":{"type":"document_url","document_url":"https://x"}}`)
	good := `{"model":"mistral-ocr-latest","pages":[{"index":0,"markdown":"# A","images":[{"id":"img","image_base64":"AAA"}],"dimensions":{"dpi":72,"height":10,"width":10},"header":"h","footer":"f","tables":[{"id":"t","content":"c"}]}],"usage_info":{"pages_processed":1,"doc_size_bytes":9},"document_annotation":null}`
	v, err := parseResult(r, result(t, good))
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
		if _, err := parseResult(r, result(t, body)); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestOCROutputTextCoverage(t *testing.T) {
	res := result(t, `{"model":"m","pages":[{"index":0,"markdown":"# A","header":"h","footer":"f","tables":[{"id":"t","content":"cell","format":"markdown"}]}],"usage_info":{"pages_processed":1}}`)
	texts, err := outputText(res)
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
		if _, err := outputText(result(t, body)); err == nil {
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
		if _, err := outputText(result(t, body)); err != nil {
			t.Fatalf("content-free fields refused: %s: %v", body, err)
		}
	}
}
