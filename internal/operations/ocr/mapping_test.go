package ocr

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/operations"
)

func mistralToAzure(t *testing.T) operations.Mapping {
	t.Helper()
	for _, m := range Mappings() {
		if m.Source.ID == Mistral && m.Target.ID == AzureDocumentIntelligence {
			return m
		}
	}
	t.Fatal("mistral to azure mapping not registered")
	return operations.Mapping{}
}

func TestOCRMappingLower(t *testing.T) {
	mapping := mistralToAzure(t)
	for _, tc := range []struct{ body, url, b64, pages string }{
		{`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/a.pdf"}}`, "https://example.com/a.pdf", "", ""},
		{`{"model":"route","document":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}`, "https://example.com/a.png", "", ""},
		{`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,QUJD"}}`, "", "QUJD", ""},
		{`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"pages":[3,0,2,2]}`, "https://x", "", "1,3-4"},
		{`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"table_format":"markdown","include_image_base64":false,"extract_header":false,"extract_footer":false}`, "https://x", "", ""},
	} {
		r := request(t, Mistral, tc.body)
		view, err := parseRequest(r, Mistral)
		if err != nil {
			t.Fatal(err)
		}
		doc, _, err := mapping.Lower(r, view)
		if err != nil {
			t.Fatalf("lower %s: %v", tc.body, err)
		}
		var out struct {
			Model        string  `json:"model"`
			URLSource    *string `json:"urlSource"`
			Base64Source *string `json:"base64Source"`
			Pages        *string `json:"pages"`
		}
		if json.Unmarshal(doc.Bytes(), &out) != nil || out.Model != "route" {
			t.Fatalf("lowered: %s", doc.Bytes())
		}
		if tc.url != "" && (out.URLSource == nil || *out.URLSource != tc.url) {
			t.Fatalf("urlSource: %s", doc.Bytes())
		}
		if tc.b64 != "" && (out.Base64Source == nil || *out.Base64Source != tc.b64) {
			t.Fatalf("base64Source: %s", doc.Bytes())
		}
		if tc.pages != "" && (out.Pages == nil || *out.Pages != tc.pages) {
			t.Fatalf("pages: %s", doc.Bytes())
		}
		if _, err := parseRequest(request(t, AzureDocumentIntelligence, string(doc.Bytes())), AzureDocumentIntelligence); err != nil {
			t.Fatalf("lowered document fails target contract: %v", err)
		}
	}
}

func TestOCRMappingLowerRefusals(t *testing.T) {
	mapping := mistralToAzure(t)
	doc := `"document":{"type":"document_url","document_url":"https://x"}`
	for _, body := range []string{
		`{"model":"route",` + doc + `,"table_format":"html"}`,
		`{"model":"route",` + doc + `,"include_image_base64":true}`,
		`{"model":"route",` + doc + `,"extract_header":true}`,
		`{"model":"route",` + doc + `,"extract_footer":true}`,
		`{"model":"route",` + doc + `,"image_limit":2}`,
		`{"model":"route",` + doc + `,"image_min_size":10}`,
		`{"model":"route",` + doc + `,"id":"job-1"}`,
		`{"model":"route","document":{"type":"document_url","document_url":"https://x","document_name":"a.pdf"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:text/plain,hello"}}`,
		`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,!!!"}}`,
	} {
		r := request(t, Mistral, body)
		view, err := parseRequest(r, Mistral)
		if err != nil {
			t.Fatalf("source parse %s: %v", body, err)
		}
		if _, _, err := mapping.Lower(r, view); err == nil {
			t.Fatalf("unmappable field admitted: %s", body)
		}
	}
}

func TestOCRMappingProject(t *testing.T) {
	mapping := mistralToAzure(t)
	source := request(t, Mistral, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	res := result(t, AzureDocumentIntelligence, azureSucceeded)
	view, err := parseResult(request(t, AzureDocumentIntelligence, `{"model":"prebuilt-layout","urlSource":"https://x"}`), res, AzureDocumentIntelligence)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := mapping.Project(source, res, view, "route")
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
	if json.Unmarshal(doc.Bytes(), &out) != nil {
		t.Fatalf("projection: %s", doc.Bytes())
	}
	if out.Model != "route" || out.UsageInfo.PagesProcessed != 2 || len(out.Pages) != 2 ||
		out.Pages[0].Index != 0 || out.Pages[0].Markdown != "# One" ||
		out.Pages[1].Index != 1 || out.Pages[1].Markdown != "# Two" {
		t.Fatalf("projection: %+v", out)
	}
	if _, err := parseResult(source, result(t, Mistral, string(doc.Bytes())), Mistral); err != nil {
		t.Fatalf("projection fails frozen contract: %v", err)
	}
}

func TestOCRMappingProjectMissingPages(t *testing.T) {
	mapping := mistralToAzure(t)
	source := request(t, Mistral, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	body := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# Only"}}`
	res := result(t, AzureDocumentIntelligence, body)
	view, err := parseResult(request(t, AzureDocumentIntelligence, `{"model":"prebuilt-layout","urlSource":"https://x"}`), res, AzureDocumentIntelligence)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := mapping.Project(source, res, view, "route")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(doc.Bytes(), &out) != nil {
		t.Fatal("bad projection")
	}
	pages, _ := out["pages"].([]any)
	if len(pages) != 1 {
		t.Fatalf("pages: %s", doc.Bytes())
	}
	if _, ok := out["usage_info"]; ok {
		t.Fatalf("missing units projected as observed: %s", doc.Bytes())
	}
}
