package ocr

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func vertexRequest(t *testing.T, body string) oif.Request {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewRequest(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(Vertex)}, d)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func vertexResult(t *testing.T, body string) oif.Result {
	t.Helper()
	d, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := oif.NewResult(oif.Descriptor{Operation: identity("ocr"), Dialect: identity(Vertex)}, d, oif.Complete)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const vertexProcessResponse = `{"document":{"mimeType":"application/pdf","text":"Hello page one.\nSecond page text.","pages":[{"pageNumber":1,"dimension":{"width":612,"height":792,"unit":"point"},"layout":{"textAnchor":{"textSegments":[{"startIndex":"0","endIndex":"15"}]},"orientation":"PAGE_UP"}},{"pageNumber":2,"layout":{"textAnchor":{"textSegments":[{"startIndex":"16","endIndex":"33"}]}}}]}}`

func TestVertexOCRRequestFields(t *testing.T) {
	for _, body := range []string{
		`{"rawDocument":{"content":"JVBERi0xLjQ=","mimeType":"application/pdf"}}`,
		`{"rawDocument":{"content":"iVBORw0KGgo=","mimeType":"image/png"},"processOptions":{"individualPageSelector":{"pages":[1,3]}}}`,
		`{"gcsDocument":{"gcsUri":"gs://docs-bucket/reports/a.pdf","mimeType":"application/pdf"}}`,
	} {
		if _, err := parseVertexRequest(vertexRequest(t, body)); err != nil {
			t.Fatalf("valid request refused: %s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"model":"processor-1","rawDocument":{"content":"AAA","mimeType":"application/pdf"}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"gcsDocument":{"gcsUri":"gs://b/a.pdf","mimeType":"application/pdf"}}`,
		`{}`,
		`{"inlineDocument":{"text":"x"}}`,
		`{"rawDocument":{"content":"not base64!","mimeType":"application/pdf"}}`,
		`{"rawDocument":{"content":"","mimeType":"application/pdf"}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"text/html"}}`,
		`{"rawDocument":{"content":"AAA"}}`,
		`{"gcsDocument":{"gcsUri":"https://example.com/a.pdf","mimeType":"application/pdf"}}`,
		`{"gcsDocument":{"gcsUri":"gs://bucket/../secret.pdf","mimeType":"application/pdf"}}`,
		`{"gcsDocument":{"gcsUri":"gs://bucket/a.pdf?generation=1","mimeType":"application/pdf"}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"processOptions":{"individualPageSelector":{"pages":[0]}}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"processOptions":{"individualPageSelector":{"pages":[]}}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"processOptions":{"fromStart":2}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"fieldMask":"text"}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf"},"labels":{"team":"a"}}`,
		`{"rawDocument":{"content":"AAA","mimeType":"application/pdf","extra":1}}`,
	} {
		if _, err := parseVertexRequest(vertexRequest(t, body)); err == nil {
			t.Fatalf("refused request admitted: %s", body)
		}
	}
}

func TestVertexOCRResultAndUsage(t *testing.T) {
	r := vertexRequest(t, `{"gcsDocument":{"gcsUri":"gs://docs-bucket/reports/a.pdf","mimeType":"application/pdf"}}`)
	v, err := parseVertexResult(r, vertexResult(t, vertexProcessResponse))
	if err != nil {
		t.Fatal(err)
	}
	if *v.(Result).Usage.MediaUnits != "2" {
		t.Fatalf("media units: %+v", v.(Result).Usage)
	}
	for _, body := range []string{
		`{"document":{"text":"only text"}}`,
		`{"document":{"pages":[]}}`,
		`{"document":{"text":"x","pages":[]}}`,
	} {
		v, err := parseVertexResult(r, vertexResult(t, body))
		if err != nil {
			t.Fatalf("result without observed pages refused: %s: %v", body, err)
		}
		if v.(Result).Usage != nil {
			t.Fatalf("missing pages invented usage: %s %+v", body, v.(Result).Usage)
		}
	}
	for _, body := range []string{
		`{}`,
		`{"document":"x"}`,
		`{"document":{"text":1}}`,
		`{"document":{"pages":"x"}}`,
		`{"document":{"pages":[{"pageNumber":0}]}}`,
		`{"document":{"pages":[{"pageNumber":"1"}]}}`,
	} {
		if _, err := parseVertexResult(r, vertexResult(t, body)); err == nil {
			t.Fatalf("bad result admitted: %s", body)
		}
	}
}

func TestVertexOCROutputTextCoverage(t *testing.T) {
	texts, err := vertexOutputText(vertexResult(t, vertexProcessResponse))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tx := range texts {
		if tx.Pointer == "/document/text" && tx.Value == "Hello page one.\nSecond page text." {
			found = true
		}
	}
	if !found {
		t.Fatalf("document text not covered: %v", texts)
	}
	for _, body := range []string{
		`{"document":{"text":"x","content":"QUJD"}}`,
		`{"document":{"text":"x","pages":[{"pageNumber":1,"image":{"content":"QUJD","mimeType":"image/png"}}]}}`,
	} {
		if _, err := vertexOutputText(vertexResult(t, body)); err == nil {
			t.Fatalf("embedded bytes admitted: %s", body)
		}
	}
	texts, err = vertexOutputText(vertexResult(t, `{"document":{"text":"seen","entities":[{"mentionText":"also seen"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tx := range texts {
		got = append(got, tx.Value)
	}
	if !strings.Contains(strings.Join(got, "|"), "also seen") {
		t.Fatalf("entity text not inspected: %v", got)
	}
}
