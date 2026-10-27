package ocr

import (
	"encoding/base64"
	"encoding/json"
	"maps"
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

func resultView(v oif.View) (Result, error) {
	result, ok := v.(Result)
	if !ok {
		return Result{}, operations.Violation("/result", "ocr_result_view")
	}
	return result, nil
}

func usageOf(v oif.View) *operations.Usage {
	result, ok := v.(Result)
	if !ok {
		return nil
	}
	return result.Usage
}

func validBase64(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(s)
	return err == nil
}

func collectText(v oif.Value, path string, skip map[string]bool) []operations.Text {
	switch v.Kind() {
	case oif.String:
		return []operations.Text{{Pointer: path, Value: operations.String(v)}}
	case oif.Array:
		out := []operations.Text{}
		for i, e := range v.Elements() {
			out = append(out, collectText(e, path+"/"+strconv.Itoa(i), skip)...)
		}
		return out
	case oif.Object:
		out := []operations.Text{}
		for _, m := range v.Members() {
			if !skip[m.Name] {
				out = append(out, collectText(m.Value, operations.Pointer(path, m.Name), skip)...)
			}
		}
		return out
	}
	return []operations.Text{}
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

func parseDocument(v map[string]any) (oif.Document, error) {
	return oif.ParseJSON(operations.Raw(v), oif.Limits{})
}

func jsonNumber(n int64) json.RawMessage {
	return json.RawMessage(strconv.FormatInt(n, 10))
}

func withMember(document map[string]any, name string, value any) map[string]any {
	merged := maps.Clone(document)
	merged[name] = value
	return merged
}

func pageNumbers(root oif.Value) ([]int64, bool, error) {
	pages := member(root, "pages")
	if operations.Optional(pages) {
		return nil, false, nil
	}
	if pages.Kind() != oif.Array {
		return nil, false, invalid("pages")
	}
	elements := pages.Elements()
	numbers := make([]int64, len(elements))
	for i, e := range elements {
		n, ok := operations.Int(e)
		if !ok || n < 0 {
			return nil, false, invalid("pages")
		}
		numbers[i] = n + 1
	}
	return numbers, true, nil
}

func withUsageInfo(document map[string]any, usage *operations.Usage, pointer string) (map[string]any, error) {
	if usage == nil || usage.MediaUnits == nil {
		return document, nil
	}
	processed, err := strconv.ParseInt(*usage.MediaUnits, 10, 64)
	if err != nil {
		return nil, operations.Violation(pointer, "ocr_pages_processed")
	}
	return withMember(document, "usage_info", map[string]any{"pages_processed": jsonNumber(processed)}), nil
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
	numbers, present, err := pageNumbers(root)
	if err != nil {
		return oif.Document{}, nil, err
	}
	if present && len(numbers) == 0 {
		return oif.Document{}, nil, invalid("pages")
	}
	selected := make([]any, len(numbers))
	for i, n := range numbers {
		selected[i] = jsonNumber(n)
	}
	document := map[string]any{"rawDocument": map[string]any{"content": content, "mimeType": mimeType}}
	if present {
		document = withMember(document, "processOptions", map[string]any{"individualPageSelector": map[string]any{"pages": selected}})
	}
	doc, err := parseDocument(document)
	return doc, nil, err
}

func lowerDocument(v oif.Value) (string, string, error) {
	if name := member(v, "document_name"); !operations.Optional(name) {
		return "", "", invalid("document.document_name")
	}
	url, err := documentURL(v)
	if err != nil {
		return "", "", err
	}
	return dataURL(url)
}

func dataURL(s string) (string, string, error) {
	meta, content, found := strings.Cut(s, ",")
	if !found || !strings.HasPrefix(meta, "data:") || !strings.HasSuffix(meta, ";base64") {
		return "", "", invalid("document")
	}
	mimeType := strings.TrimSuffix(strings.TrimPrefix(meta, "data:"), ";base64")
	if !vertexMimeTypes[mimeType] || !validBase64(content) {
		return "", "", invalid("document")
	}
	return content, mimeType, nil
}

func project(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	parsed, err := resultView(view)
	if err != nil {
		return oif.Document{}, err
	}
	document := member(result.Source().Root(), "document")
	text := operations.String(member(document, "text"))
	elements := member(document, "pages").Elements()
	pages := make([]any, len(elements))
	for i, page := range elements {
		index := i
		if number, ok := operations.Int(member(page, "pageNumber")); ok && number >= 1 {
			index = int(number) - 1
		}
		pages[i] = map[string]any{"index": jsonNumber(int64(index)), "markdown": vertexPageText(text, page)}
	}
	out, err := withUsageInfo(map[string]any{"model": route, "pages": pages}, parsed.Usage, "/document/pages")
	if err != nil {
		return oif.Document{}, err
	}
	return parseDocument(out)
}

func azureSourceMember(rawURL string) (string, string, error) {
	switch {
	case strings.HasPrefix(rawURL, "https://"):
		return "urlSource", rawURL, nil
	case strings.HasPrefix(rawURL, "data:"):
		_, payload, found := strings.Cut(rawURL, ";base64,")
		if !found || !validBase64(payload) {
			return "", "", invalid("document")
		}
		return "base64Source", payload, nil
	default:
		return "", "", invalid("document")
	}
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
	rawURL, err := documentURL(doc)
	if err != nil {
		return oif.Document{}, nil, err
	}
	sourceName, sourceValue, err := azureSourceMember(rawURL)
	if err != nil {
		return oif.Document{}, nil, err
	}
	numbers, present, err := pageNumbers(root)
	if err != nil {
		return oif.Document{}, nil, err
	}
	document := map[string]any{"model": operations.String(member(root, "model")), sourceName: sourceValue}
	if present && len(numbers) > 0 {
		document = withMember(document, "pages", azurePageRanges(numbers))
	}
	parsed, err := parseDocument(document)
	return parsed, nil, err
}

func azurePageRanges(numbers []int64) string {
	sorted := slices.Clone(numbers)
	slices.Sort(sorted)
	unique := slices.Compact(sorted)
	parts := []string{}
	for start := 0; start < len(unique); {
		end := start
		for end+1 < len(unique) && unique[end+1] == unique[end]+1 {
			end++
		}
		if end > start {
			parts = append(parts, strconv.FormatInt(unique[start], 10)+"-"+strconv.FormatInt(unique[end], 10))
		} else {
			parts = append(parts, strconv.FormatInt(unique[start], 10))
		}
		start = end + 1
	}
	return strings.Join(parts, ",")
}

func azurePages(parsed *azureResult) ([]any, error) {
	if len(parsed.pages) == 0 {
		return []any{map[string]any{"index": jsonNumber(0), "markdown": parsed.content}}, nil
	}
	pages := make([]any, len(parsed.pages))
	for i, page := range parsed.pages {
		markdown := parsed.content
		if len(parsed.pages) > 1 || page.hasSpans {
			if !page.hasSpans || len(page.spans) == 0 {
				return nil, operations.Violation("/analyzeResult/pages", "ocr_page_spans")
			}
			text, ok := utf16Slice(parsed.content, page.spans)
			if !ok {
				return nil, operations.Violation("/analyzeResult/pages", "ocr_page_spans")
			}
			markdown = text
		}
		pages[i] = map[string]any{"index": jsonNumber(page.number - 1), "markdown": markdown}
	}
	return pages, nil
}

func projectAzure(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	parsedView, err := resultView(view)
	if err != nil {
		return oif.Document{}, err
	}
	parsed := parsedView.azure
	if parsed == nil {
		return oif.Document{}, operations.Violation("/analyzeResult", "ocr_analyze_result")
	}
	if parsed.indexType != "" && parsed.indexType != "utf16CodeUnit" {
		return oif.Document{}, operations.Violation("/analyzeResult/stringIndexType", "ocr_string_index_type")
	}
	pages, err := azurePages(parsed)
	if err != nil {
		return oif.Document{}, err
	}
	out, err := withUsageInfo(map[string]any{"model": route, "pages": pages}, parsedView.Usage, "/analyzeResult/pages")
	if err != nil {
		return oif.Document{}, err
	}
	return parseDocument(out)
}
