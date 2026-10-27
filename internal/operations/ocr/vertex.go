package ocr

import (
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const Vertex = "vertex-document-processing-ocr"

func vertexDefinition() operations.Dialect {
	d := operations.Dialect{Identity: identity(Vertex), Operation: identity("ocr"), Surface: "native", Label: "Google Cloud Document AI document processing", Documentation: "https://docs.cloud.google.com/document-ai/docs/reference/rest/v1/projects.locations.processors/process", Address: operations.Address{FamilyPath: "documentai_process"}, Evidence: "vertex-document-processing-pages/1",
		Request:          parseVertexRequest,
		Result:           parseVertexResult,
		OutputText:       vertexOutputText,
		DocumentLocation: vertexDocumentLocation,
		Usage:            func(v oif.View) *operations.Usage { return v.(Result).Usage }}
	d.RequestSchema = operations.ObjectSchema(map[string]any{
		"rawDocument":    map[string]any{"type": "object"},
		"gcsDocument":    map[string]any{"type": "object"},
		"processOptions": map[string]any{"type": "object"},
	})
	d.ResultSchema = operations.ObjectSchema(map[string]any{
		"document": map[string]any{"type": "object"},
	}, "document")
	d.Probe = func(model string) []byte {
		content := probeImage[strings.Index(probeImage, ",")+1:]
		return operations.Raw(map[string]any{"rawDocument": map[string]any{"content": content, "mimeType": "image/png"}})
	}
	return d
}

var vertexMimeTypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"image/tiff":      true,
	"image/gif":       true,
	"image/bmp":       true,
	"image/webp":      true,
}

func vertexInvalid(p string) error {
	return operations.Invalid(p, "The field is outside the reviewed Google Cloud Document AI process contract.")
}

func parseVertexRequest(source oif.Request) (oif.View, error) {
	r := Request{source: source}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, vertexInvalid("request")
	}
	for _, m := range root.Members() {
		switch m.Name {
		case "rawDocument", "gcsDocument", "processOptions":
		default:
			return r, vertexInvalid(m.Name)
		}
	}
	raw, gcs := member(root, "rawDocument"), member(root, "gcsDocument")
	if operations.Optional(raw) == operations.Optional(gcs) {
		return r, vertexInvalid("rawDocument")
	}
	if !operations.Optional(raw) {
		if err := vertexRawDocument(raw); err != nil {
			return r, err
		}
	}
	if !operations.Optional(gcs) {
		if err := vertexGCSDocument(gcs); err != nil {
			return r, err
		}
	}
	if v := member(root, "processOptions"); !operations.Optional(v) {
		if err := vertexProcessOptions(v); err != nil {
			return r, err
		}
	}
	return r, nil
}

func vertexRawDocument(v oif.Value) error {
	if v.Kind() != oif.Object {
		return vertexInvalid("rawDocument")
	}
	for _, m := range v.Members() {
		if m.Name != "content" && m.Name != "mimeType" {
			return vertexInvalid("rawDocument." + m.Name)
		}
	}
	content := member(v, "content")
	if content.Kind() != oif.String || !base64Text(operations.String(content)) {
		return vertexInvalid("rawDocument.content")
	}
	if !vertexMimeTypes[operations.String(member(v, "mimeType"))] {
		return vertexInvalid("rawDocument.mimeType")
	}
	return nil
}

func base64Text(s string) bool {
	if s == "" || len(s)%4 != 0 {
		return false
	}
	padding := 0
	for i := len(s) - 1; i >= 0 && s[i] == '='; i-- {
		padding++
	}
	if padding > 2 {
		return false
	}
	for _, c := range s[:len(s)-padding] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}

func vertexGCSDocument(v oif.Value) error {
	if v.Kind() != oif.Object {
		return vertexInvalid("gcsDocument")
	}
	for _, m := range v.Members() {
		if m.Name != "gcsUri" && m.Name != "mimeType" {
			return vertexInvalid("gcsDocument." + m.Name)
		}
	}
	uri := member(v, "gcsUri")
	if uri.Kind() != oif.String || !gcsURI(operations.String(uri)) {
		return vertexInvalid("gcsDocument.gcsUri")
	}
	if !vertexMimeTypes[operations.String(member(v, "mimeType"))] {
		return vertexInvalid("gcsDocument.mimeType")
	}
	return nil
}

func gcsURI(s string) bool {
	rest, ok := strings.CutPrefix(s, "gs://")
	if !ok || len(s) > 1024 || strings.ContainsAny(s, "\\%?# \t\r\n") {
		return false
	}
	bucket, object, found := strings.Cut(rest, "/")
	if !found || bucket == "" || object == "" {
		return false
	}
	for _, segment := range strings.Split(rest, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func vertexProcessOptions(v oif.Value) error {
	if v.Kind() != oif.Object {
		return vertexInvalid("processOptions")
	}
	for _, m := range v.Members() {
		if m.Name != "individualPageSelector" {
			return vertexInvalid("processOptions." + m.Name)
		}
	}
	selector := member(v, "individualPageSelector")
	if selector.Kind() != oif.Object {
		return vertexInvalid("processOptions.individualPageSelector")
	}
	for _, m := range selector.Members() {
		if m.Name != "pages" {
			return vertexInvalid("processOptions.individualPageSelector." + m.Name)
		}
	}
	pages := member(selector, "pages")
	if pages.Kind() != oif.Array || len(pages.Elements()) == 0 {
		return vertexInvalid("processOptions.individualPageSelector.pages")
	}
	for _, e := range pages.Elements() {
		if n, ok := operations.Int(e); !ok || n < 1 {
			return vertexInvalid("processOptions.individualPageSelector.pages")
		}
	}
	return nil
}

func vertexDocumentLocation(request oif.Request) (string, bool) {
	uri := member(member(request.Document().Root(), "gcsDocument"), "gcsUri")
	if uri.Kind() != oif.String {
		return "", false
	}
	return operations.String(uri), true
}

func parseVertexResult(request oif.Request, source oif.Result) (oif.View, error) {
	if _, err := parseVertexRequest(request); err != nil {
		return nil, err
	}
	r := Result{Source: source}
	root := source.Source().Root()
	fail := func(field, requirement string) (oif.View, error) {
		return r, operations.Violation(field, requirement)
	}
	document := member(root, "document")
	if document.Kind() != oif.Object {
		return fail("/document", "ocr_document")
	}
	if text := member(document, "text"); !operations.Optional(text) && text.Kind() != oif.String {
		return fail("/document/text", "ocr_document_text")
	}
	pages := member(document, "pages")
	if operations.Optional(pages) {
		return r, nil
	}
	if pages.Kind() != oif.Array {
		return fail("/document/pages", "ocr_pages")
	}
	for i, page := range pages.Elements() {
		if page.Kind() != oif.Object {
			return fail("/document/pages/"+strconv.Itoa(i), "ocr_page")
		}
		if number := member(page, "pageNumber"); !operations.Optional(number) {
			if n, ok := operations.Int(number); !ok || n < 1 {
				return fail("/document/pages/"+strconv.Itoa(i)+"/pageNumber", "ocr_page_number")
			}
		}
	}
	if len(pages.Elements()) > 0 {
		units := strconv.Itoa(len(pages.Elements()))
		r.Usage = &operations.Usage{MediaUnits: &units}
	}
	return r, nil
}

func vertexOutputText(result oif.Result) ([]operations.Text, error) {
	root := result.Source().Root()
	document := member(root, "document")
	if content := member(document, "content"); content.Kind() == oif.String && operations.String(content) != "" {
		return nil, conflict("/document/content")
	}
	for i, page := range member(document, "pages").Elements() {
		if content := member(member(page, "image"), "content"); content.Kind() == oif.String && operations.String(content) != "" {
			return nil, conflict("/document/pages/" + strconv.Itoa(i) + "/image/content")
		}
	}
	out := []operations.Text{}
	collectText(root, "", &out)
	return out, nil
}

func collectText(v oif.Value, path string, out *[]operations.Text) {
	switch v.Kind() {
	case oif.Object:
		for _, m := range v.Members() {
			collectText(m.Value, operations.Pointer(path, m.Name), out)
		}
	case oif.Array:
		for i, e := range v.Elements() {
			collectText(e, path+"/"+strconv.Itoa(i), out)
		}
	case oif.String:
		*out = append(*out, operations.Text{Pointer: path, Value: operations.String(v)})
	}
}

func vertexPageText(text string, page oif.Value) string {
	segments := member(member(member(page, "layout"), "textAnchor"), "textSegments")
	if segments.Kind() != oif.Array || len(segments.Elements()) == 0 {
		parts := []string{}
		for _, paragraph := range member(page, "paragraphs").Elements() {
			if part := anchorText(text, member(member(paragraph, "layout"), "textAnchor")); part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return anchorText(text, member(member(page, "layout"), "textAnchor"))
}

func anchorText(text string, anchor oif.Value) string {
	runes := []rune(text)
	parts := []string{}
	for _, segment := range member(anchor, "textSegments").Elements() {
		start := segmentIndex(member(segment, "startIndex"))
		end := segmentIndex(member(segment, "endIndex"))
		if start < 0 || end < start || start >= int64(len(runes)) {
			continue
		}
		if end > int64(len(runes)) {
			end = int64(len(runes))
		}
		parts = append(parts, string(runes[start:end]))
	}
	return strings.Join(parts, "")
}

func segmentIndex(v oif.Value) int64 {
	switch v.Kind() {
	case oif.String:
		n, err := strconv.ParseInt(operations.String(v), 10, 64)
		if err != nil {
			return -1
		}
		return n
	case oif.Number:
		n, ok := operations.Int(v)
		if !ok {
			return -1
		}
		return n
	case oif.Absent, oif.Null:
		return 0
	}
	return -1
}
