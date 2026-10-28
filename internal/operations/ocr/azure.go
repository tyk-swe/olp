package ocr

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const (
	AzureDocumentIntelligence = "azure-document-intelligence-ocr"
	AzureAPIVersion           = "2024-11-30"
)

const probeImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAXklEQVR4nO3PMQ0AMAzAsPInvYLYYVWKESTzjhsd8KsBrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BrQGtAa0BbQHKU9LC7/CP1AAAAABJRU5ErkJggg=="

func azureDefinition() operations.Dialect {
	return operations.Dialect{
		Identity: identity(AzureDocumentIntelligence), Operation: identity("ocr"), Surface: "native", Label: "azure-document-intelligence-ocr document OCR", Documentation: "https://learn.microsoft.com/en-us/rest/api/aiservices/document-models/analyze-document", Address: operations.Address{RelativePath: "documentintelligence/documentModels/{model}:analyze"}, Evidence: "azure-document-intelligence-pages/1",
		Request:         parseAzureRequest,
		Result:          parseAzureResult,
		BindModel:       operations.ModelChanges,
		BindResultModel: azureBindResultModel,
		FetchedDocument: azureFetchedDocument,
		OutputText:      azureOutputText,
		Usage:           usageOf,
		Transport:       operations.TransportPoll202,
		Wire:            azureWire,
		Poll:            azurePollState,
		CancelPoll:      func(pollURL string) (string, string, bool) { return "DELETE", pollURL, true },
		RequestSchema: operations.ObjectSchema(map[string]any{
			"model":        map[string]any{"type": "string"},
			"urlSource":    map[string]any{"type": "string"},
			"base64Source": map[string]any{"type": "string"},
			"pages":        map[string]any{"type": "string"},
		}, "model"),
		ResultSchema: operations.ObjectSchema(map[string]any{
			"status":        map[string]any{"type": "string"},
			"analyzeResult": map[string]any{"type": "object"},
		}, "status", "analyzeResult"),
		Probe: func(model string) []byte {
			return operations.Raw(map[string]any{"model": model, "base64Source": probeImageBase64})
		},
	}
}

func azureInvalid(p string) error {
	return operations.Invalid(p, "The field is outside the reviewed Azure Document Intelligence analyze contract.")
}

type azureSpan struct{ offset, length int64 }

type azurePage struct {
	number   int64
	spans    []azureSpan
	hasSpans bool
}

type azureResult struct {
	content      string
	contentUnits []uint16
	pages        []azurePage
	hasPages     bool
	indexType    string
}

func parseAzureRequest(source oif.Request) (oif.View, error) {
	r := Request{source: source}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, azureInvalid("request")
	}
	for _, m := range root.Members() {
		switch m.Name {
		case "model", "urlSource", "base64Source", "pages":
		default:
			return r, azureInvalid(m.Name)
		}
	}
	if member(root, "model").Kind() != oif.String || operations.String(member(root, "model")) == "" {
		return r, azureInvalid("model")
	}
	urlValue, hasURL := root.Lookup("urlSource")
	b64Value, hasB64 := root.Lookup("base64Source")
	if hasURL == hasB64 {
		return r, azureInvalid("urlSource")
	}
	if hasURL && (urlValue.Kind() != oif.String || !strings.HasPrefix(operations.String(urlValue), "https://")) {
		return r, azureInvalid("urlSource")
	}
	if hasB64 && (b64Value.Kind() != oif.String || !validBase64(operations.String(b64Value))) {
		return r, azureInvalid("base64Source")
	}
	if v := member(root, "pages"); !operations.Optional(v) {
		if v.Kind() != oif.String || !validAzurePages(operations.String(v)) {
			return r, azureInvalid("pages")
		}
	}
	return r, nil
}

func validAzurePages(s string) bool {
	parts := strings.Split(s, ",")
	if len(parts) == 0 || len(parts) > 512 {
		return false
	}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return false
		}
		lo, hi, found := strings.Cut(part, "-")
		loN, err := strconv.ParseInt(lo, 10, 64)
		if err != nil || loN < 1 {
			return false
		}
		if found {
			hiN, err := strconv.ParseInt(hi, 10, 64)
			if err != nil || hiN < loN {
				return false
			}
		}
	}
	return true
}

func parseAzureResult(request oif.Request, source oif.Result) (oif.View, error) {
	if _, err := parseAzureRequest(request); err != nil {
		return nil, err
	}
	r := Result{Source: source}
	root := source.Source().Root()
	fail := func(field, requirement string) (oif.View, error) {
		return r, operations.Violation(field, requirement)
	}
	if operations.String(member(root, "status")) != "succeeded" || member(root, "status").Kind() != oif.String {
		return fail("/status", "ocr_analysis_status")
	}
	analyze := member(root, "analyzeResult")
	if analyze.Kind() != oif.Object {
		return fail("/analyzeResult", "ocr_analyze_result")
	}
	if member(analyze, "modelId").Kind() != oif.String {
		return fail("/analyzeResult/modelId", "ocr_model")
	}
	if v := member(analyze, "contentFormat"); !operations.Optional(v) && (v.Kind() != oif.String || (operations.String(v) != "markdown" && operations.String(v) != "text")) {
		return fail("/analyzeResult/contentFormat", "ocr_content_format")
	}
	content := member(analyze, "content")
	if content.Kind() != oif.String {
		return fail("/analyzeResult/content", "ocr_content")
	}
	parsed := &azureResult{content: operations.String(content), contentUnits: utf16.Encode([]rune(operations.String(content))), indexType: operations.String(member(analyze, "stringIndexType"))}
	units := int64(len(parsed.contentUnits))
	pages := member(analyze, "pages")
	if pages.Kind() != oif.Absent {
		if pages.Kind() != oif.Array {
			return fail("/analyzeResult/pages", "ocr_pages")
		}
		parsed.hasPages = true
		for i, page := range pages.Elements() {
			path := "/analyzeResult/pages/" + strconv.Itoa(i)
			number, ok := operations.Int(member(page, "pageNumber"))
			if !ok || number < 1 {
				return fail(path+"/pageNumber", "ocr_page_number")
			}
			entry := azurePage{number: number}
			spans := member(page, "spans")
			if spans.Kind() != oif.Absent {
				if spans.Kind() != oif.Array {
					return fail(path+"/spans", "ocr_page_spans")
				}
				entry.hasSpans = true
				for _, span := range spans.Elements() {
					offset, okOffset := operations.Int(member(span, "offset"))
					length, okLength := operations.Int(member(span, "length"))
					if !okOffset || !okLength || offset < 0 || length < 0 || offset > units || length > units-offset {
						return fail(path+"/spans", "ocr_page_spans")
					}
					entry.spans = append(entry.spans, azureSpan{offset: offset, length: length})
				}
			}
			parsed.pages = append(parsed.pages, entry)
		}
	}
	r.azure = parsed
	if parsed.hasPages && len(parsed.pages) > 0 {
		unitsText := strconv.Itoa(len(parsed.pages))
		r.Usage = &operations.Usage{MediaUnits: &unitsText}
	}
	return r, nil
}

func azurePollState(doc oif.Document) (operations.PollState, error) {
	switch operations.String(member(doc.Root(), "status")) {
	case "notStarted", "running":
		return operations.PollPending, nil
	case "succeeded":
		return operations.PollSucceeded, nil
	case "failed":
		return operations.PollFailed, nil
	}
	return "", operations.Violation("/status", "ocr_poll_status")
}

func azureWire(view oif.View) (operations.WireRequest, error) {
	request, ok := view.(Request)
	if !ok {
		return operations.WireRequest{}, azureInvalid("request")
	}
	root := request.source.Document().Root()
	format := "text"
	if operations.String(member(root, "model")) == "prebuilt-layout" {
		format = "markdown"
	}
	query := url.Values{
		"api-version":         {AzureAPIVersion},
		"outputContentFormat": {format},
		"stringIndexType":     {"utf16CodeUnit"},
	}
	if v := member(root, "pages"); v.Kind() == oif.String {
		query.Set("pages", operations.String(v))
	}
	body, err := azureWireBody(root)
	if err != nil {
		return operations.WireRequest{}, err
	}
	return operations.WireRequest{Query: query, Body: operations.Raw(body)}, nil
}

func azureWireBody(root oif.Value) (map[string]any, error) {
	if v, ok := root.Lookup("urlSource"); ok {
		return map[string]any{"urlSource": operations.String(v)}, nil
	}
	if v, ok := root.Lookup("base64Source"); ok {
		return map[string]any{"base64Source": operations.String(v)}, nil
	}
	return nil, azureInvalid("urlSource")
}

func azureBindResultModel(doc oif.Document, model string) ([]oif.Change, error) {
	analyze := member(doc.Root(), "analyzeResult")
	if _, present := analyze.Lookup("modelId"); !present {
		return nil, nil
	}
	encoded, _ := json.Marshal(model)
	return []oif.Change{{Pointer: operations.Pointer("/analyzeResult", "modelId"), Value: string(encoded), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}

var azureMetadata = map[string]bool{"apiVersion": true, "modelId": true, "stringIndexType": true, "contentFormat": true, "unit": true}

func azureOutputText(result oif.Result) ([]operations.Text, error) {
	analyze := member(result.Source().Root(), "analyzeResult")
	if analyze.Kind() != oif.Object {
		return nil, conflict("/analyzeResult")
	}
	return collectText(analyze, "/analyzeResult", azureMetadata), nil
}

func utf16Slice(units []uint16, spans []azureSpan) (string, bool) {
	var b strings.Builder
	for _, span := range spans {
		if span.offset < 0 || span.length < 0 || span.offset > int64(len(units)) || span.length > int64(len(units))-span.offset {
			return "", false
		}
		if span.length == 0 {
			continue
		}
		end := span.offset + span.length
		if units[span.offset] >= 0xdc00 && units[span.offset] <= 0xdfff || units[end-1] >= 0xd800 && units[end-1] <= 0xdbff {
			return "", false
		}
		b.WriteString(string(utf16.Decode(units[span.offset:end])))
	}
	return b.String(), true
}
