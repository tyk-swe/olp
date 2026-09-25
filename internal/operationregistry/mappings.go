package operationregistry

import (
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func mappings() []operations.Mapping {
	return []operations.Mapping{
		{
			Source:            Identity("openai-embeddings"),
			Target:            Identity("voyage-embeddings"),
			Lower:             lowerEmbedding,
			ValidateEffective: validateEmbeddingMapping,
			Project:           projectEmbedding,
			Projected: []oif.Disposition{
				{Field: "/result/model", Disposition: "bound", Rule: "published model binding"},
				{Field: "/result/usage/prompt_tokens", Disposition: "introduced", Rule: "registered equivalent native control"},
			},
			Evidence: "qualified-openai-voyage-float-embedding/1",
		},
		{
			Source:  Identity("rerank"),
			Target:  Identity("voyage-rerank"),
			Lower:   lowerRerank,
			Project: projectRerank,
			Projected: []oif.Disposition{
				{Field: "/result/model", Disposition: "bound", Rule: "published model binding"},
				{Field: "/result/data", Disposition: "relocated", Rule: "qualified native control relocation"},
				{Field: "/result/results", Disposition: "relocated", Rule: "qualified native control relocation"},
			},
			Evidence: "qualified-rerank-voyage-selection/1",
		},
	}
}
func mappingChange(pointer, value string) oif.Change {
	return oif.Change{Pointer: pointer, Value: value, Origin: oif.QualifiedMapping, Reason: "registered equivalent native control"}
}

// relocationReason marks the removal and destination halves of one declared
// relocation so the planner can account both scopes under a single rule.
const relocationReason = "qualified native control relocation"

func relocate(from, to, value string) []oif.Change {
	return []oif.Change{
		{Pointer: from, Remove: true, Origin: oif.QualifiedMapping, Reason: relocationReason},
		{Pointer: to, Value: value, Origin: oif.QualifiedMapping, Reason: relocationReason},
	}
}

// The declared changes returned alongside each destination are the
// co-construction contract: the planner re-applies them, requires a
// byte-identical destination, and derives receipt provenance from them.
func lowerEmbedding(request oif.Request, _ oif.View) (oif.Document, []oif.Change, error) {
	root := request.Document().Root()
	changes := []oif.Change{mappingChange("/truncation", "false")}
	for _, member := range root.Members() {
		switch member.Name {
		case "model", "input":
		case "dimensions":
			changes = append(changes, relocate("/dimensions", "/output_dimension", member.Value.Raw())...)
		case "encoding_format":
			if member.Value.Raw() == `"float"` {
				changes = append(changes, mappingChange("/encoding_format", "null"))
			}
		default:
			return oif.Document{}, nil, operations.Invalid("/request", "A source control has no qualified target mapping.")
		}
	}
	input := operations.Member(root, "input")
	if input.Kind() != oif.String {
		if input.Kind() != oif.Array {
			return oif.Document{}, nil, operations.Invalid("/input", "This mapping requires native text inputs.")
		}
		for _, element := range input.Elements() {
			if element.Kind() != oif.String {
				return oif.Document{}, nil, operations.Invalid("/input", "Token IDs belong to a different tokenizer contract.")
			}
		}
	}
	doc, err := oif.Apply(request.Document(), changes)
	if err != nil {
		return oif.Document{}, nil, err
	}
	return doc, changes, nil
}
func projectEmbedding(_ oif.Request, result oif.Result, _ oif.View, route string) (oif.Document, []oif.Change, error) {
	doc := result.Source()
	changes, err := operations.ModelChanges(doc, route)
	if err != nil {
		return oif.Document{}, nil, err
	}
	usage := operations.Member(doc.Root(), "usage")
	total := operations.Member(usage, "total_tokens")
	if usage.Kind() == oif.Object && total.Kind() == oif.Number {
		if _, exists := usage.Lookup("prompt_tokens"); !exists {
			changes = append(changes, mappingChange("/usage/prompt_tokens", total.Raw()))
		}
	}
	out, err := oif.Apply(doc, changes)
	if err != nil {
		return oif.Document{}, nil, err
	}
	return out, changes, nil
}
func lowerRerank(request oif.Request, _ oif.View) (oif.Document, []oif.Change, error) {
	changes := []oif.Change{}
	for _, member := range request.Document().Root().Members() {
		switch member.Name {
		case "model", "query", "documents", "return_documents", "truncation":
		case "top_n":
			changes = append(changes, relocate("/top_n", "/top_k", member.Value.Raw())...)
		default:
			return oif.Document{}, nil, operations.Invalid("/request", "A source control has no qualified target rerank mapping.")
		}
	}
	for i, document := range operations.Member(request.Document().Root(), "documents").Elements() {
		if document.Kind() != oif.String {
			return oif.Document{}, nil, operations.Invalid("/documents/"+strconv.Itoa(i), "The target mapping cannot preserve structured document identity.")
		}
	}
	doc, err := oif.Apply(request.Document(), changes)
	if err != nil {
		return oif.Document{}, nil, err
	}
	return doc, changes, nil
}
func projectRerank(_ oif.Request, result oif.Result, _ oif.View, route string) (oif.Document, []oif.Change, error) {
	doc := result.Source()
	data, exists := doc.Root().Lookup("data")
	if !exists {
		return oif.Document{}, nil, operations.Violation("/data", "The target result omitted native ranking data.")
	}
	if _, exists := doc.Root().Lookup("results"); exists {
		return oif.Document{}, nil, operations.Violation("/results", "Native result collides with the projected result location.")
	}
	changes, err := operations.ModelChanges(doc, route)
	if err != nil {
		return oif.Document{}, nil, err
	}
	changes = append(changes, relocate("/data", "/results", data.Raw())...)
	out, err := oif.Apply(doc, changes)
	if err != nil {
		return oif.Document{}, nil, err
	}
	return out, changes, nil
}

func validateEmbeddingMapping(_ oif.Request, effective oif.Request) error {
	root := effective.Document().Root()
	dtype := operations.Member(root, "output_dtype")
	task := operations.Member(root, "input_type")
	if dtype.Kind() != oif.Absent && dtype.Raw() != `"float"` || task.Kind() != oif.Absent && task.Kind() != oif.Null {
		return operations.Error("target_capability", "/defaults", "embedding_contract", "The target defaults change vector dtype or input task outside this qualified mapping.")
	}
	return nil
}
