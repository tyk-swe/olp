package ocr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/operations"
)

func TestOCRMappingsRejectUnrepresentablePageSelections(t *testing.T) {
	for _, target := range []string{Vertex, AzureDocumentIntelligence} {
		for _, pages := range []string{`[]`, `[9223372036854775807]`} {
			t.Run(target+pages, func(t *testing.T) {
				source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,QUJD"},"pages":`+pages+`}`)
				view, err := parseRequest(source)
				if err != nil {
					t.Fatal(err)
				}
				for _, mapping := range Mappings() {
					if mapping.Source.ID == Mistral && mapping.Target.ID == target {
						if _, _, err := mapping.Lower(source, view); err == nil {
							t.Fatalf("%s accepted an unrepresentable page selection %s", target, pages)
						}
						return
					}
				}
				t.Fatal("mapping missing")
			})
		}
	}
}

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

func TestOCRMappingLowerAzure(t *testing.T) {
	mapping := mistralToAzure(t)
	for _, tc := range []struct{ body, url, b64, pages string }{
		{`{"model":"route","document":{"type":"document_url","document_url":"https://example.com/a.pdf"}}`, "https://example.com/a.pdf", "", ""},
		{`{"model":"route","document":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}`, "https://example.com/a.png", "", ""},
		{`{"model":"route","document":{"type":"document_url","document_url":"data:application/pdf;base64,QUJD"}}`, "", "QUJD", ""},
		{`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"pages":[3,0,2,2]}`, "https://x", "", "1,3-4"},
		{`{"model":"route","document":{"type":"document_url","document_url":"https://x"},"include_image_base64":false,"extract_header":false,"extract_footer":false}`, "https://x", "", ""},
	} {
		r := request(t, tc.body)
		view, err := parseRequest(r)
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
		if _, err := parseAzureRequest(azureRequestDoc(t, string(doc.Bytes()))); err != nil {
			t.Fatalf("lowered document fails target contract: %v", err)
		}
	}
}

func TestOCRMappingLowerAzureRefusals(t *testing.T) {
	mapping := mistralToAzure(t)
	doc := `"document":{"type":"document_url","document_url":"https://x"}`
	for _, body := range []string{
		`{"model":"route",` + doc + `,"table_format":"html"}`,
		`{"model":"route",` + doc + `,"table_format":"markdown"}`,
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
		r := request(t, body)
		view, err := parseRequest(r)
		if err != nil {
			t.Fatalf("source parse %s: %v", body, err)
		}
		if _, _, err := mapping.Lower(r, view); err == nil {
			t.Fatalf("unmappable field admitted: %s", body)
		}
	}
}

func TestOCRMappingProjectAzure(t *testing.T) {
	mapping := mistralToAzure(t)
	source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	res := azureResultDoc(t, azureSucceeded)
	view, err := parseAzureResult(azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://x"}`), res)
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
	if _, err := parseResult(source, result(t, string(doc.Bytes()))); err != nil {
		t.Fatalf("projection fails frozen contract: %v", err)
	}
}

func TestOCRMappingProjectAzureBlankPage(t *testing.T) {
	mapping := mistralToAzure(t)
	source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	body := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# One","pages":[{"pageNumber":1,"spans":[{"offset":0,"length":5}]},{"pageNumber":2,"spans":[]}]}}`
	res := azureResultDoc(t, body)
	view, err := parseAzureResult(azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://x"}`), res)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := mapping.Project(source, res, view, "route")
	if err != nil {
		t.Fatalf("blank page refused projection: %v", err)
	}
	var out struct {
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
	if len(out.Pages) != 2 || out.Pages[0].Markdown != "# One" || out.Pages[1].Markdown != "" || out.Pages[1].Index != 1 || out.UsageInfo.PagesProcessed != 2 {
		t.Fatalf("projection: %+v", out)
	}
}

func TestOCRMappingProjectAzureMissingPages(t *testing.T) {
	mapping := mistralToAzure(t)
	source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	body := `{"status":"succeeded","analyzeResult":{"apiVersion":"2024-11-30","modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","contentFormat":"markdown","content":"# Only"}}`
	res := azureResultDoc(t, body)
	view, err := parseAzureResult(azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://x"}`), res)
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

func TestOCRProjectRejectsForeignView(t *testing.T) {
	source := request(t, `{"model":"route","document":{"type":"document_url","document_url":"https://x"}}`)
	res := azureResultDoc(t, azureSucceeded)
	foreign, err := parseRequest(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []func() error{
		func() error { _, err := project(source, res, foreign, "route"); return err },
		func() error { _, err := projectAzure(source, res, foreign, "route"); return err },
	} {
		if err := project(); err == nil {
			t.Fatal("foreign result view projected instead of refused")
		}
	}
	if usage := usageOf(foreign); usage != nil {
		t.Fatalf("foreign view produced usage: %+v", usage)
	}
	if _, err := azureWire(foreign); err == nil {
		t.Fatal("foreign request view lowered to a wire request")
	}
}

func TestOCRAzurePageProjectionBoundsAllocations(t *testing.T) {
	const pageCount = 128
	content := strings.Repeat("A", 32<<10)
	pages := make([]string, pageCount)
	for i := range pages {
		pages[i] = fmt.Sprintf(`{"pageNumber":%d,"spans":[{"offset":%d,"length":1}]}`, i+1, i)
	}
	response := azureResultDoc(t, `{"status":"succeeded","analyzeResult":{"modelId":"prebuilt-layout","stringIndexType":"utf16CodeUnit","content":`+strconv.Quote(content)+`,"pages":[`+strings.Join(pages, ",")+`]}}`)
	view, err := parseAzureResult(azureRequestDoc(t, `{"model":"prebuilt-layout","urlSource":"https://x"}`), response)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := resultView(view)
	if err != nil {
		t.Fatal(err)
	}
	allocations := testing.AllocsPerRun(3, func() {
		result, err := azurePages(parsed.azure)
		if err != nil || len(result) != pageCount || result[pageCount-1].Markdown != "A" {
			t.Fatalf("page projection changed: pages=%d err=%v", len(result), err)
		}
	})
	if budget := float64(2*pageCount + 1); allocations > budget {
		t.Fatalf("page projection exceeded its allocation budget: got %.0f want at most %.0f", allocations, budget)
	}
}
