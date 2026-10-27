package ocr

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func Mappings() []operations.Mapping {
	return []operations.Mapping{
		{Source: identity(Mistral), Target: identity(Vertex), Lower: lower, Project: project, Evidence: "ocr-mistral-vertex-document-processing/1"},
	}
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
