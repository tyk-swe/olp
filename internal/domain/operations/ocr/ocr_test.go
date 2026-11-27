package ocr

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func ocrRequest(t *testing.T, raw string) oif.Request {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	req, err := oif.NewRequest(oif.Descriptor{
		Operation: identity("ocr"),
		Dialect:   identity(Mistral),
	}, doc)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func ocrResult(t *testing.T, raw string) oif.Result {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 64 << 10, MaxDepth: 32, MaxNodes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	res, err := oif.NewResult(oif.Descriptor{
		Operation: identity("ocr"),
		Dialect:   identity(Mistral),
	}, doc, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMistralParseRequestAcceptsValidDocumentsAndRejectsContractViolations(t *testing.T) {
	t.Parallel()
	valid := []string{
		`{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/doc.pdf","document_name":"doc.pdf"},"pages":[0,1],"table_format":"markdown","include_image_base64":false}`,
		`{"model":"ocr-1","document":{"type":"image_url","image_url":"data:image/png;base64,AAAA"},"image_limit":2,"image_min_size":64,"table_format":"html","extract_header":true,"extract_footer":false}`,
		`{"model":"ocr-1","document":{"type":"image_url","image_url":{"url":"https://example.test/page.png"}}}`,
	}
	for _, body := range valid {
		if _, err := parseRequest(ocrRequest(t, body)); err != nil {
			t.Errorf("parseRequest rejected valid body %s: %v", body, err)
		}
	}

	invalid := map[string]string{
		"non-object root":            `[]`,
		"unknown top-level field":    `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"},"bbox_annotation_format":{}}`,
		"missing model":              `{"document":{"type":"document_url","document_url":"https://example.test/a.pdf"}}`,
		"http document url rejected": `{"model":"ocr-1","document":{"type":"document_url","document_url":"http://example.test/a.pdf"}}`,
		"provider file id rejected":  `{"model":"ocr-1","document":{"type":"file","file_id":"file_123"}}`,
		"extra image_url object key": `{"model":"ocr-1","document":{"type":"image_url","image_url":{"url":"https://example.test/a.png","detail":"high"}}}`,
		"negative page index":        `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"},"pages":[-1]}`,
		"negative image_limit":       `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"},"image_limit":-1}`,
		"unsupported table_format":   `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"},"table_format":"csv"}`,
		"non-boolean extract_header": `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"},"extract_header":"yes"}`,
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseRequest(ocrRequest(t, body)); err == nil {
				t.Fatalf("parseRequest accepted invalid %s", name)
			}
		})
	}
}

func TestMistralParseResultExtractsPageUsageAndRejectsMalformedResults(t *testing.T) {
	t.Parallel()
	req := ocrRequest(t, `{"model":"ocr-1","document":{"type":"document_url","document_url":"https://example.test/a.pdf"}}`)
	view, err := parseResult(req, ocrResult(t, `{"model":"ocr-1","pages":[{"index":0,"markdown":"# Hello"},{"index":1,"markdown":"World"}],"usage_info":{"pages_processed":2}}`))
	if err != nil {
		t.Fatalf("parseResult returned error: %v", err)
	}
	got := view.(Result)
	if got.Usage == nil || got.Usage.MediaUnits == nil || *got.Usage.MediaUnits != "2" {
		t.Fatalf("usage = %+v, want MediaUnits=2", got.Usage)
	}

	malformed := map[string]string{
		"missing model":            `{"pages":[],"usage_info":{"pages_processed":0}}`,
		"pages not array":          `{"model":"ocr-1","pages":{},"usage_info":{"pages_processed":0}}`,
		"negative page index":      `{"model":"ocr-1","pages":[{"index":-1,"markdown":"a"}],"usage_info":{"pages_processed":1}}`,
		"missing page markdown":    `{"model":"ocr-1","pages":[{"index":0}],"usage_info":{"pages_processed":1}}`,
		"negative pages_processed": `{"model":"ocr-1","pages":[{"index":0,"markdown":"a"}],"usage_info":{"pages_processed":-1}}`,
	}
	for name, body := range malformed {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseResult(req, ocrResult(t, body)); err == nil {
				t.Fatalf("parseResult accepted malformed %s", name)
			}
		})
	}
}

func TestMistralOutputTextExtractsReadableContentAndFailsClosed(t *testing.T) {
	t.Parallel()
	clean := ocrResult(t, `{
		"model":"ocr-1",
		"usage_info":{"pages_processed":1},
		"document_annotation":null,
		"extra_inert":42,
		"pages":[{
			"index":0,
			"markdown":"Body text",
			"header":"Top header",
			"footer":"Bottom footer",
			"dimensions":{"dpi":200,"width":800,"height":600},
			"images":[{"id":"img-0","image_base64":null,"top_left_x":0}],
			"tables":[{"id":"tbl-0","format":"markdown","content":"|a|b|"}]
		}]
	}`)
	texts, err := outputText(clean)
	if err != nil {
		t.Fatalf("outputText returned error on valid result: %v", err)
	}
	want := map[string]string{
		"/pages/0/markdown":         "Body text",
		"/pages/0/header":           "Top header",
		"/pages/0/footer":           "Bottom footer",
		"/pages/0/tables/0/content": "|a|b|",
	}
	if len(texts) != len(want) {
		t.Fatalf("outputText returned %d segments, want %d: %+v", len(texts), len(want), texts)
	}
	for _, seg := range texts {
		if want[seg.Pointer] != seg.Value {
			t.Errorf("segment %s = %q, want %q", seg.Pointer, seg.Value, want[seg.Pointer])
		}
	}

	conflicts := map[string]string{
		"document_annotation present": `{"model":"ocr-1","document_annotation":"{\"k\":\"v\"}","pages":[],"usage_info":{"pages_processed":0}}`,
		"non-inert top-level field":   `{"model":"ocr-1","unexpected":"secret","pages":[],"usage_info":{"pages_processed":0}}`,
		"image_base64 present":        `{"model":"ocr-1","pages":[{"index":0,"markdown":"ok","images":[{"id":"i","image_base64":"AAAA"}]}],"usage_info":{"pages_processed":1}}`,
		"image_annotation present":    `{"model":"ocr-1","pages":[{"index":0,"markdown":"ok","images":[{"id":"i","image_annotation":"note"}]}],"usage_info":{"pages_processed":1}}`,
		"unknown string in page":      `{"model":"ocr-1","pages":[{"index":0,"markdown":"ok","extra_text":"hidden"}],"usage_info":{"pages_processed":1}}`,
		"non-inert table field":       `{"model":"ocr-1","pages":[{"index":0,"markdown":"ok","tables":[{"id":"t","content":"ok","caption":"hidden"}]}],"usage_info":{"pages_processed":1}}`,
	}
	for name, body := range conflicts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := outputText(ocrResult(t, body))
			var incompatible *oif.Incompatibility
			if !errors.As(err, &incompatible) || incompatible.Code != "policy_conflict" {
				t.Fatalf("outputText(%s) = %v, want policy_conflict incompatibility", name, err)
			}
		})
	}
}
