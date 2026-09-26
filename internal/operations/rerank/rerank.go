// Package rerank preserves candidate identity, exact native scores and order.
// It never normalizes scores, reorders ties or splits the document set.
package rerank

import (
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

var identity = oif.Identity{ID: "rerank", Revision: operations.Revision}

func Identity() oif.Identity { return identity }

type Request struct {
	source          oif.Request
	dialect         string
	documents       oif.Value
	top             int
	returnDocuments bool
	estimate        int64
}

func (r Request) Schema() oif.Identity { return identity }
func (r Request) Source() oif.Request  { return r.source }
func (r Request) Documents() oif.Value { return r.documents }

type Ranked struct {
	Position, InputIndex     int
	Score, Document, InputID oif.Value
}
type Result struct {
	source oif.Result
	rows   []Ranked
	usage  *operations.Usage
}

func (r Result) Schema() oif.Identity { return identity }
func (r Result) Source() oif.Result   { return r.source }
func (r Result) Rows() []Ranked       { return slices.Clone(r.rows) }

func Definitions() []operations.Dialect {
	out := []operations.Dialect{}
	for _, id := range []string{"rerank", "voyage-rerank", "tei-rerank", "cohere-rerank-v2"} {
		d := operations.Dialect{Identity: oif.Identity{ID: id, Revision: operations.Revision}, Operation: identity, Surface: "openai", Label: id, Evidence: "native-rerank-identity-scores/1", Address: operations.Address{FamilyPath: "rerank"}}
		if id == "voyage-rerank" || id == "tei-rerank" || id == "cohere-rerank-v2" {
			d.Surface = "native"
		}
		if id == "tei-rerank" {
			d.Address = operations.Address{RelativePath: "rerank"}
		}
		if id == "cohere-rerank-v2" {
			d.Address = operations.Address{RelativePath: "rerank"}
			d.Documentation = "https://docs.cohere.com/reference/rerank"
		}
		d.Request = func(source oif.Request) (oif.View, error) { return liftRequest(source, id) }
		d.Result = func(source oif.Request, result oif.Result) (oif.View, error) { return liftResult(source, result, id) }
		d.Estimate = func(view oif.View) int64 { return view.(Request).estimate }
		d.Usage = func(view oif.View) *operations.Usage {
			usage := view.(Result).usage
			if usage == nil {
				return nil
			}
			out := *usage
			return &out
		}
		if id != "tei-rerank" {
			d.BindModel = operations.ModelChanges
		}
		if id != "tei-rerank" && id != "cohere-rerank-v2" {
			d.BindResultModel = operations.ModelChanges
		}
		d.Probe = func(model string) []byte {
			if id == "tei-rerank" {
				return operations.Raw(map[string]any{"query": "rank probe", "texts": []string{"first", "second"}, "raw_scores": true})
			}
			return operations.Raw(map[string]any{"model": model, "query": "rank probe", "documents": []string{"first", "second"}})
		}
		d.InputText = func(request oif.Request) ([]operations.Text, error) { return inputText(request, id) }
		d.OutputText = outputText
		d.Defaults = defaults(id)
		inputs := "documents"
		if id == "tei-rerank" {
			inputs = "texts"
		}
		d.RequestSchema = operations.ObjectSchema(map[string]any{"query": map[string]any{"type": "string"}}, "query", inputs)
		d.ResultSchema = operations.Raw(map[string]any{"description": "Native ranked records with unchanged scores, order and document correspondence."})
		out = append(out, d)
	}
	return out
}

func liftRequest(source oif.Request, id string) (Request, error) {
	r := Request{source: source, dialect: id, estimate: 1}
	root := source.Document().Root()
	if id == "cohere-rerank-v2" {
		// The native identity contract may forward new provider-owned fields.
		// Refuse only known foreign aliases that would change the v2 request.
		for _, name := range []string{"top_k", "return_documents", "truncation", "truncate", "raw_scores", "return_text"} {
			if _, present := root.Lookup(name); present {
				return r, operations.Invalid(name, "This foreign rerank control has no Cohere native v2 meaning.")
			}
		}
		for _, name := range []string{"max_tokens_per_doc", "priority"} {
			if value, present := root.Lookup(name); present {
				n, ok := operations.Int(value)
				if !ok || n < 0 || name == "max_tokens_per_doc" && n == 0 || name == "priority" && n > 999 {
					return r, operations.Invalid(name, "Use a supported native Cohere rerank v2 integer control.")
				}
			}
		}
	}
	if id != "tei-rerank" && operations.Member(root, "model").Kind() != oif.String {
		return r, operations.Invalid("model", "Use the native model identity.")
	}
	if root.Kind() != oif.Object || operations.Member(root, "query").Kind() != oif.String {
		return r, operations.Invalid("query", "Use a native query string.")
	}
	name := "documents"
	if id == "tei-rerank" {
		name = "texts"
	}
	r.documents = operations.Member(root, name)
	if r.documents.Kind() != oif.Array || len(r.documents.Elements()) == 0 {
		return r, operations.Invalid(name, "Use a non-empty document collection.")
	}
	for _, doc := range r.documents.Elements() {
		if doc.Kind() != oif.String && (id != "rerank" || doc.Kind() != oif.Object) {
			return r, operations.Invalid(name, "This dialect cannot execute the requested document representation.")
		}
		r.estimate += int64((len(doc.Raw()) + 3) / 4)
	}
	r.estimate += int64((len(operations.String(operations.Member(root, "query"))) + 3) / 4)
	topName := "top_n"
	returnName := "return_documents"
	if id == "voyage-rerank" {
		topName = "top_k"
		if _, found := root.Lookup("top_n"); found {
			return r, operations.Invalid("top_n", "Use the native top_k control, without a colliding alias.")
		}
	}
	if id == "rerank" {
		if _, found := root.Lookup("top_k"); found {
			return r, operations.Invalid("top_k", "Use the declared top_n control or the native Voyage dialect.")
		}
	}
	if id == "tei-rerank" {
		returnName = "return_text"
		for _, name := range []string{"top_n", "top_k", "return_documents", "documents"} {
			if _, found := root.Lookup(name); found {
				return r, operations.Invalid(name, "The native rerank operation has no mapping for this control.")
			}
		}
	}
	if top, present := root.Lookup(topName); present && !operations.Optional(top) {
		value, ok := operations.Int(top)
		if !ok || value < 1 || value > 1<<31 {
			return r, operations.Invalid(topName, "Use a positive bounded native result count.")
		}
		r.top = int(value)
	}
	if value, present := root.Lookup(returnName); present {
		if value.Kind() != oif.Boolean {
			return r, operations.Invalid(returnName, "Use a native boolean control.")
		}
		r.returnDocuments = value.Raw() == "true"
	}
	for _, name := range []string{"raw_scores", "truncation", "truncate"} {
		if value, present := root.Lookup(name); present && !operations.Optional(value) && value.Kind() != oif.Boolean {
			return r, operations.Invalid(name, "Use a native boolean control.")
		}
	}
	return r, nil
}

func liftResult(source oif.Request, native oif.Result, id string) (Result, error) {
	request, err := liftRequest(source, id)
	out := Result{source: native}
	if err != nil {
		return out, err
	}
	root := native.Source().Root()
	items := root
	scoreName := "relevance_score"
	documentName := "document"
	if id == "rerank" || id == "cohere-rerank-v2" {
		items = operations.Member(root, "results")
	}
	if id == "voyage-rerank" {
		items = operations.Member(root, "data")
	}
	if id == "tei-rerank" {
		scoreName = "score"
		documentName = "text"
	}
	if items.Kind() != oif.Array {
		return out, operations.Violation("/results", "ranked_collection")
	}
	want := len(request.documents.Elements())
	if request.top > 0 {
		want = min(request.top, want)
	}
	if len(items.Elements()) != want {
		return out, operations.Violation("/results", "ranked_candidate_count")
	}
	seen := map[int]bool{}
	for position, item := range items.Elements() {
		index, ok := operations.Int(operations.Member(item, "index"))
		if !ok || index < 0 || index >= int64(len(request.documents.Elements())) || seen[int(index)] {
			return out, operations.Violation("/results/index", "document_identity")
		}
		seen[int(index)] = true
		score := operations.Member(item, scoreName)
		if !operations.Number(score) {
			return out, operations.Violation("/results/score", "native_score")
		}
		document := operations.Member(item, documentName)
		original := request.documents.Elements()[index]
		if request.returnDocuments && document.Kind() == oif.Absent {
			return out, operations.Violation("/results/document", "required_document_observation")
		}
		if document.Kind() != oif.Absent {
			if original.Kind() == oif.String {
				text := document
				if document.Kind() == oif.Object {
					text = operations.Member(document, "text")
				}
				if text.Kind() != oif.String || operations.String(text) != operations.String(original) {
					return out, operations.Violation("/results/document", "original_document_bytes")
				}
			} else {
				if !operations.SameValue(document, original) {
					return out, operations.Violation("/results/document", "original_document_identity")
				}

			}
		}
		out.rows = append(out.rows, Ranked{Position: position, InputIndex: int(index), Score: score, Document: document, InputID: operations.Member(original, "id")})
	}
	if usage, present := root.Lookup("usage"); present {
		tokens, ok := operations.Int(operations.Member(usage, "total_tokens"))
		if !ok || tokens < 0 {
			return out, operations.Violation("/usage", "native_usage")
		}
		out.usage = &operations.Usage{InputTokens: &tokens, TotalTokens: &tokens}
	}
	if billed := operations.Member(operations.Member(root, "meta"), "billed_units"); billed.Kind() == oif.Object {
		units := operations.Member(billed, "search_units")
		if units.Kind() != oif.Absent && units.Kind() != oif.Null {
			if !operations.Number(units) || strings.HasPrefix(units.Raw(), "-") {
				return out, operations.Violation("/meta/billed_units", "native_usage")
			}
			raw := units.Raw()
			out.usage = &operations.Usage{MediaUnits: &raw}
		}
	}
	return out, nil
}

func defaults(id string) map[string]operations.Field {
	fields := map[string]operations.Field{}
	boolean := operations.FieldSchema(map[string]any{"type": "boolean"}, func(v oif.Value) error {
		if v.Kind() != oif.Boolean {
			return operations.Invalid("default", "Use a native boolean.")
		}
		return nil
	})
	if id == "tei-rerank" {
		fields["raw_scores"] = boolean
		fields["return_text"] = boolean
		fields["truncate"] = operations.FieldSchema(map[string]any{"type": []string{"boolean", "null"}}, operations.NullableBool)
		fields["truncation_direction"] = operations.FieldSchema(map[string]any{"enum": []string{"left", "right", "Left", "Right"}}, func(v oif.Value) error {
			if !slices.Contains([]string{"left", "right", "Left", "Right"}, operations.String(v)) {
				return operations.Invalid("truncation_direction", "Use a native direction.")
			}
			return nil
		})
		return fields
	}
	if id == "cohere-rerank-v2" {
		fields["top_n"] = operations.FieldSchema(map[string]any{"type": "integer", "minimum": 1}, operations.PositiveInt)
		fields["max_tokens_per_doc"] = operations.FieldSchema(map[string]any{"type": "integer", "minimum": 1}, operations.PositiveInt)
		fields["priority"] = operations.FieldSchema(map[string]any{"type": "integer", "minimum": 0, "maximum": 999}, func(v oif.Value) error {
			n, ok := operations.Int(v)
			if !ok || n < 0 || n > 999 {
				return operations.Invalid("priority", "Use a native priority between 0 and 999.")
			}
			return nil
		})
		return fields
	}
	name := "top_n"
	if id == "voyage-rerank" {
		name = "top_k"
		fields["truncation"] = boolean
	}
	fields[name] = operations.FieldSchema(map[string]any{"type": []string{"integer", "null"}, "minimum": 1}, operations.PositiveInt)
	fields["return_documents"] = boolean
	return fields
}

func inputText(source oif.Request, id string) ([]operations.Text, error) {
	root := source.Document().Root()
	name := "documents"
	if id == "tei-rerank" {
		name = "texts"
	}
	known := map[string]bool{"model": true, "query": true, name: true}
	for field := range defaults(id) {
		known[field] = true
	}
	for _, field := range root.Members() {
		if !known[field.Name] {
			return nil, operations.Error("policy_conflict", "/native_extension", "input_policy_coverage", "The input policy cannot inspect an unknown native rank control.")
		}
	}
	out := []operations.Text{{Pointer: "/query", Value: operations.String(operations.Member(root, "query"))}}
	for index, document := range operations.Member(root, name).Elements() {
		path := "/" + name + "/" + strconv.Itoa(index)
		if document.Kind() == oif.String {
			out = append(out, operations.Text{Pointer: path, Value: operations.String(document)})
			continue
		}
		for _, field := range document.Members() {
			if field.Value.Kind() != oif.String {
				return nil, operations.Error("policy_conflict", path, "input_policy_coverage", "The input policy cannot inspect this native document metadata.")
			}
			out = append(out, operations.Text{Pointer: operations.Pointer(path, field.Name), Value: operations.String(field.Value)})
		}
	}
	return out, nil
}

func outputText(result oif.Result) ([]operations.Text, error) {
	root := result.Source().Root()
	items := root
	path := ""
	if root.Kind() == oif.Object {
		for _, field := range root.Members() {
			if !slices.Contains([]string{"results", "data", "id", "model", "object", "usage", "meta"}, field.Name) {
				return nil, operations.Error("policy_conflict", "/native_extension", "output_policy_coverage", "The output policy cannot inspect an unknown native rank result.")
			}
		}
		items = operations.Member(root, "results")
		path = "/results"
		if items.Kind() == oif.Absent {
			items = operations.Member(root, "data")
			path = "/data"
		}
	}
	out := []operations.Text{}
	for index, item := range items.Elements() {
		for _, field := range item.Members() {
			if field.Name == "index" || field.Name == "score" || field.Name == "relevance_score" {
				continue
			}
			p := path + "/" + strconv.Itoa(index) + "/" + field.Name
			if field.Value.Kind() != oif.String {
				return nil, operations.Error("policy_conflict", p, "output_policy_coverage", "The output policy cannot inspect this native document result.")
			}
			out = append(out, operations.Text{Pointer: p, Value: operations.String(field.Value)})
		}
	}
	return out, nil
}
