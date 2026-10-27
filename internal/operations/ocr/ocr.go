// Package ocr owns the frozen Mistral OCR document contract. Document bytes are
// fetched by the provider; annotations, provider-retained file documents and
// block-level extraction carry separate pricing or contracts and stay refused.
package ocr

import (
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

const Mistral = "mistral-ocr"

// probeImage is a 1x1 transparent PNG the certification probe reads back.
const probeImage = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func identity(id string) oif.Identity        { return oif.Identity{ID: id, Revision: operations.Revision} }
func member(v oif.Value, k string) oif.Value { return operations.Member(v, k) }
func invalid(p string) error {
	return operations.Invalid(p, "The field is outside the frozen Mistral OCR contract.")
}
func conflict(p string) error {
	return operations.Error("policy_conflict", p, "output_policy_coverage", "The output policy cannot inspect this native OCR result.")
}

type Request struct{ source oif.Request }

func (r Request) Schema() oif.Identity { return identity("ocr") }

type Result struct {
	Source oif.Result
	Usage  *operations.Usage
	azure  *azureResult
}

func (r Result) Schema() oif.Identity { return identity("ocr") }

func Definitions() []operations.Dialect {
	return []operations.Dialect{
		{
			Identity: identity(Mistral), Operation: identity("ocr"), Surface: "openai", Label: "mistral-ocr document OCR", Documentation: "https://docs.mistral.ai/api/endpoint/ocr", Address: operations.Address{RelativePath: "ocr"}, Evidence: "mistral-ocr-pages/1",
			Request:         parseRequest,
			Result:          parseResult,
			BindModel:       operations.ModelChanges,
			BindResultModel: operations.ModelChanges,
			FetchedDocument: mistralFetchedDocument,
			OutputText:      outputText,
			Usage:           usageOf,
			RequestSchema: operations.ObjectSchema(map[string]any{
				"model":    map[string]any{"type": "string"},
				"document": map[string]any{"type": "object"},
				"pages":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
			}, "model", "document"),
			ResultSchema: operations.ObjectSchema(map[string]any{
				"model":      map[string]any{"type": "string"},
				"pages":      map[string]any{"type": "array"},
				"usage_info": map[string]any{"type": "object"},
			}, "model", "pages", "usage_info"),
			Probe: func(model string) []byte {
				return operations.Raw(map[string]any{"model": model, "document": map[string]any{"type": "image_url", "image_url": probeImage}})
			},
		},
		vertexDefinition(),
		azureDefinition(),
	}
}

func parseRequest(source oif.Request) (oif.View, error) {
	r := Request{source: source}
	root := source.Document().Root()
	if root.Kind() != oif.Object {
		return r, invalid("request")
	}
	for _, m := range root.Members() {
		switch m.Name {
		case "model", "document", "id", "pages", "include_image_base64", "image_limit", "image_min_size", "table_format", "extract_header", "extract_footer":
		default:
			return r, invalid(m.Name)
		}
	}
	if member(root, "model").Kind() != oif.String {
		return r, invalid("model")
	}
	if v := member(root, "id"); !operations.Optional(v) && v.Kind() != oif.String {
		return r, invalid("id")
	}
	if err := document(member(root, "document")); err != nil {
		return r, err
	}
	if v := member(root, "pages"); !operations.Optional(v) {
		if v.Kind() != oif.Array {
			return r, invalid("pages")
		}
		for _, e := range v.Elements() {
			if n, ok := operations.Int(e); !ok || n < 0 {
				return r, invalid("pages")
			}
		}
	}
	for _, name := range []string{"image_limit", "image_min_size"} {
		if v := member(root, name); !operations.Optional(v) {
			if n, ok := operations.Int(v); !ok || n < 0 {
				return r, invalid(name)
			}
		}
	}
	for _, name := range []string{"include_image_base64", "extract_header", "extract_footer"} {
		if v := member(root, name); !operations.Optional(v) && v.Kind() != oif.Boolean {
			return r, invalid(name)
		}
	}
	if v := member(root, "table_format"); !operations.Optional(v) {
		if v.Kind() != oif.String || (operations.String(v) != "markdown" && operations.String(v) != "html") {
			return r, invalid("table_format")
		}
	}
	return r, nil
}

// document admits only inline document and image URLs. Provider-retained
// file IDs, embedded base64 documents and URL-less forms have no contract.
func document(v oif.Value) error {
	if v.Kind() != oif.Object {
		return invalid("document")
	}
	for _, m := range v.Members() {
		switch m.Name {
		case "type", "document_url", "document_name", "image_url":
		default:
			return invalid("document." + m.Name)
		}
	}
	switch operations.String(member(v, "type")) {
	case "document_url":
		if v := member(v, "document_name"); !operations.Optional(v) && v.Kind() != oif.String {
			return invalid("document.document_name")
		}
		if !remoteURL(member(v, "document_url")) {
			return invalid("document.document_url")
		}
	case "image_url":
		u := member(v, "image_url")
		if u.Kind() == oif.Object {
			for _, m := range u.Members() {
				if m.Name != "url" {
					return invalid("document.image_url." + m.Name)
				}
			}
			u = member(u, "url")
		}
		if !remoteURL(u) {
			return invalid("document.image_url")
		}
	default:
		return invalid("document.type")
	}
	return nil
}

func remoteURL(v oif.Value) bool {
	if v.Kind() != oif.String {
		return false
	}
	s := operations.String(v)
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "data:")
}

func parseResult(request oif.Request, source oif.Result) (oif.View, error) {
	if _, err := parseRequest(request); err != nil {
		return nil, err
	}
	r := Result{Source: source}
	root := source.Source().Root()
	fail := func(field, requirement string) (oif.View, error) {
		return r, operations.Violation(field, requirement)
	}
	if member(root, "model").Kind() != oif.String {
		return fail("/model", "ocr_model")
	}
	pages := member(root, "pages")
	if pages.Kind() != oif.Array {
		return fail("/pages", "ocr_pages")
	}
	for i, page := range pages.Elements() {
		path := "/pages/" + strconv.Itoa(i)
		index, ok := operations.Int(member(page, "index"))
		if !ok || index < 0 {
			return fail(path+"/index", "ocr_page_index")
		}
		if member(page, "markdown").Kind() != oif.String {
			return fail(path+"/markdown", "ocr_page_markdown")
		}
	}
	processed, ok := operations.Int(member(member(root, "usage_info"), "pages_processed"))
	if !ok || processed < 0 {
		return fail("/usage_info/pages_processed", "ocr_pages_processed")
	}
	units := strconv.FormatInt(processed, 10)
	r.Usage = &operations.Usage{MediaUnits: &units}
	return r, nil
}

// outputText covers the text the OCR result carries: page markdown and its
// header, footer and table content. Reviewed structural fields pass without
// inspection; anything the contract does not name fails closed unless it
// carries no text.
func outputText(result oif.Result) ([]operations.Text, error) {
	out := []operations.Text{}
	root := result.Source().Root()
	for _, f := range root.Members() {
		switch f.Name {
		case "pages", "model", "usage_info":
		case "document_annotation":
			// Annotation output comes from a second model pass; the policy
			// cannot inspect it.
			if f.Value.Kind() != oif.Null && f.Value.Kind() != oif.Absent {
				return nil, conflict("/document_annotation")
			}
		default:
			if !inert(f.Value) {
				return nil, conflict(operations.Pointer("", f.Name))
			}
		}
	}
	for i, page := range member(root, "pages").Elements() {
		path := "/pages/" + strconv.Itoa(i)
		for _, f := range page.Members() {
			p := operations.Pointer(path, f.Name)
			switch f.Name {
			case "markdown", "header", "footer":
				if f.Value.Kind() == oif.String {
					out = append(out, operations.Text{Pointer: p, Value: operations.String(f.Value)})
				} else if !inert(f.Value) {
					return nil, conflict(p)
				}
			case "index":
			case "dimensions":
				if !inert(f.Value) {
					if f.Value.Kind() != oif.Object {
						return nil, conflict(p)
					}
					for _, m := range f.Value.Members() {
						if !inert(m.Value) {
							return nil, conflict(operations.Pointer(p, m.Name))
						}
					}
				}
			case "images":
				if f.Value.Kind() != oif.Array && !inert(f.Value) {
					return nil, conflict(p)
				}
				for j, image := range f.Value.Elements() {
					for _, m := range image.Members() {
						mp := operations.Pointer(p+"/"+strconv.Itoa(j), m.Name)
						switch m.Name {
						case "id":
						case "image_base64", "image_annotation":
							if m.Value.Kind() != oif.Null && m.Value.Kind() != oif.Absent {
								return nil, conflict(mp)
							}
						default:
							if !inert(m.Value) {
								return nil, conflict(mp)
							}
						}
					}
				}
			case "tables":
				if f.Value.Kind() != oif.Array && !inert(f.Value) {
					return nil, conflict(p)
				}
				for j, table := range f.Value.Elements() {
					tp := p + "/" + strconv.Itoa(j)
					for _, m := range table.Members() {
						mp := operations.Pointer(tp, m.Name)
						switch m.Name {
						case "content":
							if m.Value.Kind() == oif.String {
								out = append(out, operations.Text{Pointer: mp, Value: operations.String(m.Value)})
							} else if !inert(m.Value) {
								return nil, conflict(mp)
							}
						case "id", "format":
						default:
							if !inert(m.Value) {
								return nil, conflict(mp)
							}
						}
					}
				}
			default:
				if !inert(f.Value) {
					return nil, conflict(p)
				}
			}
		}
	}
	return out, nil
}

// inert reports whether a field carries no text the output policy could miss:
// null, scalar and empty-container values only.
func inert(v oif.Value) bool {
	switch v.Kind() {
	case oif.Absent, oif.Null, oif.Number, oif.Boolean:
		return true
	case oif.Array:
		return len(v.Elements()) == 0
	case oif.Object:
		return len(v.Members()) == 0
	}
	return false
}
