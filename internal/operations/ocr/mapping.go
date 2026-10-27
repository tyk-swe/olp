package ocr

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func Mappings() []operations.Mapping {
	return []operations.Mapping{
		{Source: identity(Mistral), Target: identity(Vertex), Lower: lower, Project: project, Evidence: "ocr-mistral-vertex-document-processing/1"},
		{Source: identity(Mistral), Target: identity(AzureDocumentIntelligence), Lower: lowerAzure, Project: projectAzure, Evidence: "ocr-mistral-azure-document-intelligence/1"},
	}
}

func documentURL(v oif.Value) (string, error) {
	switch operations.String(member(v, "type")) {
	case "document_url":
		return operations.String(member(v, "document_url")), nil
	case "image_url":
		u := member(v, "image_url")
		if u.Kind() == oif.Object {
			u = member(u, "url")
		}
		return operations.String(u), nil
	default:
		return "", invalid("document.type")
	}
}

func mistralFetchedDocument(source oif.Request) bool {
	url, err := documentURL(member(source.Document().Root(), "document"))
	return err == nil && strings.HasPrefix(url, "https://")
}

func azureFetchedDocument(source oif.Request) bool {
	_, present := source.Document().Root().Lookup("urlSource")
	return present
}

func parseDocument(v any) (oif.Document, error) {
	return oif.ParseJSON(operations.Raw(v), oif.Limits{})
}

func lower(source oif.Request, view oif.View) (oif.Document, []oif.Provenance, error) {
	root := source.Document().Root()
	for _, m := range root.Members() {
		switch m.Name {
		case "model", "document", "pages":
		default:
			return oif.Document{}, nil, invalid(m.Name)
		}
	}
	content, mimeType, err := lowerDocument(member(root, "document"))
	if err != nil {
		return oif.Document{}, nil, err
	}
	out := map[string]any{"rawDocument": map[string]any{"content": content, "mimeType": mimeType}}
	if pages := member(root, "pages"); !operations.Optional(pages) {
		if len(pages.Elements()) == 0 {
			return oif.Document{}, nil, invalid("pages")
		}
		selected := []any{}
		for _, e := range pages.Elements() {
			n, _ := operations.Int(e)
			selected = append(selected, json.RawMessage(strconv.FormatInt(n+1, 10)))
		}
		out["processOptions"] = map[string]any{"individualPageSelector": map[string]any{"pages": selected}}
	}
	doc, err := parseDocument(out)
	return doc, nil, err
}

func lowerDocument(v oif.Value) (string, string, error) {
	if name := member(v, "document_name"); !operations.Optional(name) {
		return "", "", invalid("document.document_name")
	}
	var url oif.Value
	switch operations.String(member(v, "type")) {
	case "document_url":
		url = member(v, "document_url")
	case "image_url":
		url = member(v, "image_url")
		if url.Kind() == oif.Object {
			url = member(url, "url")
		}
	default:
		return "", "", invalid("document.type")
	}
	return dataURL(operations.String(url))
}

func dataURL(s string) (string, string, error) {
	meta, content, found := strings.Cut(s, ",")
	if !found || !strings.HasPrefix(meta, "data:") || !strings.HasSuffix(meta, ";base64") {
		return "", "", invalid("document")
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64")
	if !vertexMimeTypes[mimeType] || !base64Text(content) {
		return "", "", invalid("document")
	}
	return content, mimeType, nil
}

func project(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	document := member(result.Source().Root(), "document")
	text := operations.String(member(document, "text"))
	pages := []any{}
	for i, page := range member(document, "pages").Elements() {
		index := i
		if number, ok := operations.Int(member(page, "pageNumber")); ok && number >= 1 {
			index = int(number) - 1
		}
		pages = append(pages, map[string]any{"index": json.RawMessage(strconv.Itoa(index)), "markdown": vertexPageText(text, page)})
	}
	out := map[string]any{"model": route, "pages": pages}
	if usage := view.(Result).Usage; usage != nil && usage.MediaUnits != nil {
		processed, err := strconv.Atoi(*usage.MediaUnits)
		if err != nil {
			return oif.Document{}, operations.Violation("/document/pages", "ocr_pages_processed")
		}
		out["usage_info"] = map[string]any{"pages_processed": json.RawMessage(strconv.Itoa(processed))}
	}
	return parseDocument(out)
}

func lowerAzure(source oif.Request, view oif.View) (oif.Document, []oif.Provenance, error) {
	root := source.Document().Root()
	for _, m := range root.Members() {
		switch m.Name {
		case "model", "document", "pages", "table_format", "include_image_base64", "extract_header", "extract_footer":
		default:
			return oif.Document{}, nil, invalid(m.Name)
		}
	}
	if v := member(root, "table_format"); !operations.Optional(v) && operations.String(v) != "markdown" {
		return oif.Document{}, nil, invalid("table_format")
	}
	for _, name := range []string{"include_image_base64", "extract_header", "extract_footer"} {
		if v := member(root, name); !operations.Optional(v) && v.Raw() != "false" {
			return oif.Document{}, nil, invalid(name)
		}
	}
	doc := member(root, "document")
	if _, present := doc.Lookup("document_name"); present {
		return oif.Document{}, nil, invalid("document.document_name")
	}
	var rawURL string
	switch operations.String(member(doc, "type")) {
	case "document_url":
		rawURL = operations.String(member(doc, "document_url"))
	case "image_url":
		u := member(doc, "image_url")
		if u.Kind() == oif.Object {
			u = member(u, "url")
		}
		rawURL = operations.String(u)
	default:
		return oif.Document{}, nil, invalid("document.type")
	}
	out := map[string]any{"model": operations.String(member(root, "model"))}
	if strings.HasPrefix(rawURL, "https://") {
		out["urlSource"] = rawURL
	} else if strings.HasPrefix(rawURL, "data:") {
		_, payload, found := strings.Cut(rawURL, ";base64,")
		if !found || !validBase64(payload) {
			return oif.Document{}, nil, invalid("document")
		}
		out["base64Source"] = payload
	} else {
		return oif.Document{}, nil, invalid("document")
	}
	if pages := member(root, "pages"); pages.Kind() == oif.Array && len(pages.Elements()) > 0 {
		numbers := []int64{}
		for _, e := range pages.Elements() {
			n, ok := operations.Int(e)
			if !ok || n < 0 {
				return oif.Document{}, nil, invalid("pages")
			}
			numbers = append(numbers, n+1)
		}
		out["pages"] = azurePageRanges(numbers)
	}
	doc2, err := parseDocument(out)
	return doc2, nil, err
}

func azurePageRanges(numbers []int64) string {
	slices.Sort(numbers)
	numbers = slices.Compact(numbers)
	parts := []string{}
	for start := 0; start < len(numbers); {
		end := start
		for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
			end++
		}
		if end > start {
			parts = append(parts, strconv.FormatInt(numbers[start], 10)+"-"+strconv.FormatInt(numbers[end], 10))
		} else {
			parts = append(parts, strconv.FormatInt(numbers[start], 10))
		}
		start = end + 1
	}
	return strings.Join(parts, ",")
}

func projectAzure(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	parsed := view.(Result).azure
	if parsed == nil {
		return oif.Document{}, operations.Violation("/analyzeResult", "ocr_analyze_result")
	}
	if parsed.indexType != "" && parsed.indexType != "utf16CodeUnit" {
		return oif.Document{}, operations.Violation("/analyzeResult/stringIndexType", "ocr_string_index_type")
	}
	pages := []any{}
	if len(parsed.pages) == 0 {
		pages = append(pages, map[string]any{"index": jsonNumber(0), "markdown": parsed.content})
	} else {
		for _, page := range parsed.pages {
			markdown := parsed.content
			if len(parsed.pages) > 1 || page.hasSpans {
				if !page.hasSpans || len(page.spans) == 0 {
					return oif.Document{}, operations.Violation("/analyzeResult/pages", "ocr_page_spans")
				}
				text, ok := utf16Slice(parsed.content, page.spans)
				if !ok {
					return oif.Document{}, operations.Violation("/analyzeResult/pages", "ocr_page_spans")
				}
				markdown = text
			}
			pages = append(pages, map[string]any{"index": jsonNumber(page.number - 1), "markdown": markdown})
		}
	}
	out := map[string]any{"model": route, "pages": pages}
	if usage := view.(Result).Usage; usage != nil && usage.MediaUnits != nil {
		processed, err := strconv.ParseInt(*usage.MediaUnits, 10, 64)
		if err != nil {
			return oif.Document{}, operations.Violation("/analyzeResult/pages", "ocr_pages_processed")
		}
		out["usage_info"] = map[string]any{"pages_processed": jsonNumber(processed)}
	}
	return parseDocument(out)
}

func jsonNumber(n int64) any {
	return json.RawMessage(strconv.FormatInt(n, 10))
}
