package ocr

import (
	"encoding/json"
	"testing"
)

func lowerVertex(t *testing.T, body string) (map[string]any, error) {
	t.Helper()
	source := request(t, body)
	view, err := parseRequest(source)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := lower(source, view)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(doc.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, err := parseVertexRequest(vertexRequest(t, string(doc.Bytes()))); err != nil {
		t.Fatalf("lowered document is not a valid native request: %s: %v", doc.Bytes(), err)
	}
	return out, nil
}

func TestOCRMappingLower(t *testing.T) {
	out, err := lowerVertex(t, `{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,JVBERi0xLjQ="},"pages":[0,2]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, named := out["model"]; named {
		t.Fatalf("lowered document carries a model: %v", out)
	}
	raw, _ := out["rawDocument"].(map[string]any)
	if raw["content"] != "JVBERi0xLjQ=" || raw["mimeType"] != "application/pdf" {
		t.Fatalf("raw document: %v", raw)
	}
	options, _ := out["processOptions"].(map[string]any)
	selector, _ := options["individualPageSelector"].(map[string]any)
	pages, _ := selector["pages"].([]any)
	if len(pages) != 2 || pages[0] != float64(1) || pages[1] != float64(3) {
		t.Fatalf("page selector is not the requested pages in provider numbering: %v", out)
	}
	out, err = lowerVertex(t, `{"model":"route","document":{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}}`)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = out["rawDocument"].(map[string]any)
	if raw["mimeType"] != "image/png" {
		t.Fatalf("image document: %v", out)
	}
	if _, set := out["processOptions"]; set {
		t.Fatalf("selector invented without requested pages: %v", out)
	}
}

func TestOCRMappingLowerRefusals(t *testing.T) {
	for _, body := range []string{
		`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/report.pdf"}}`,
		`{"model":"route","document":{"type":"image_url","image_url":"https://example.com/a.png"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA","document_name":"a.pdf"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf,rawbytes"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:text/html;base64,AAA"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:;base64,AAA"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"id":"job-1"}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"table_format":"markdown"}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"include_image_base64":false}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"extract_header":true}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"image_limit":2}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,AAA"},"pages":[]}`,
	} {
		if _, err := lowerVertex(t, body); err == nil {
			t.Fatalf("unmappable field admitted: %s", body)
		}
	}
}

func TestOCRMappingProject(t *testing.T) {
	source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,JVBERi0xLjQ="}}`)
	res := vertexResult(t, vertexProcessResponse)
	view, err := parseVertexResult(vertexRequest(t, `{"rawDocument":{"content":"JVBERi0xLjQ=","mimeType":"application/pdf"}}`), res)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := project(source, res, view, "route")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Model string `json:"model"`
		Pages []struct {
			Index    int    `json:"index"`
			Markdown string `json:"markdown"`
		} `json:"pages"`
		UsageInfo struct {
			PagesProcessed int `json:"pages_processed"`
		} `json:"usage_info"`
	}
	if err := json.Unmarshal(doc.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Model != "route" || out.UsageInfo.PagesProcessed != 2 {
		t.Fatalf("projection identity or usage: %+v", out)
	}
	if len(out.Pages) != 2 || out.Pages[0].Index != 0 || out.Pages[0].Markdown != "Hello page one." || out.Pages[1].Index != 1 || out.Pages[1].Markdown != "Second page text." {
		t.Fatalf("projection pages: %+v", out.Pages)
	}
	res = vertexResult(t, `{"document":{"text":"only text"}}`)
	view, err = parseVertexResult(vertexRequest(t, `{"rawDocument":{"content":"JVBERi0xLjQ=","mimeType":"application/pdf"}}`), res)
	if err != nil {
		t.Fatal(err)
	}
	doc, err = project(source, res, view, "route")
	if err != nil {
		t.Fatal(err)
	}
	var bare map[string]any
	if err := json.Unmarshal(doc.Bytes(), &bare); err != nil {
		t.Fatal(err)
	}
	if _, claimed := bare["usage_info"]; claimed {
		t.Fatalf("projection claimed pages it never observed: %v", bare)
	}
}
