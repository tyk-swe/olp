package operationregistry

import (
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

func mappings() []operations.Mapping {
	return []operations.Mapping{
		{Source: Identity("openai-embeddings"), Target: Identity("voyage-embeddings"), Lower: lowerEmbedding, ValidateEffective: validateEmbeddingMapping, Project: projectEmbedding, Evidence: "qualified-openai-voyage-float-embedding/1"},
		{Source: Identity("rerank"), Target: Identity("voyage-rerank"), Lower: lowerRerank, Project: projectRerank, Evidence: "qualified-rerank-voyage-selection/1"},
	}
}
func mappingChange(pointer, value string) oif.Change {
	return oif.Change{Pointer: pointer, Value: value, Origin: oif.QualifiedMapping, Reason: "registered equivalent native control"}
}
func remove(pointer string) oif.Change {
	return oif.Change{Pointer: pointer, Remove: true, Origin: oif.QualifiedMapping, Reason: "registered native control location mapping"}
}
func lowerEmbedding(request oif.Request, _ oif.View) (oif.Document, []oif.Provenance, error) {
	root := request.Document().Root()
	changes := []oif.Change{mappingChange("/truncation", "false")}
	for _, member := range root.Members() {
		switch member.Name {
		case "model", "input":
		case "dimensions":
			changes = append(changes, remove("/dimensions"), mappingChange("/output_dimension", member.Value.Raw()))
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
	return doc, nil, err
}
func projectEmbedding(_ oif.Request, result oif.Result, _ oif.View, route string) (oif.Document, error) {
	doc := result.Source()
	changes, err := operations.ModelChanges(doc, route)
	if err != nil {
		return oif.Document{}, err
	}
	usage := operations.Member(doc.Root(), "usage")
	total := operations.Member(usage, "total_tokens")
	if usage.Kind() == oif.Object && total.Kind() == oif.Number {
		if _, exists := usage.Lookup("prompt_tokens"); !exists {
			changes = append(changes, mappingChange("/usage/prompt_tokens", total.Raw()))
		}
	}
	return oif.Apply(doc, changes)
}
func lowerRerank(request oif.Request, _ oif.View) (oif.Document, []oif.Provenance, error) {
	changes := []oif.Change{}
	for _, member := range request.Document().Root().Members() {
		switch member.Name {
		case "model", "query", "documents", "return_documents", "truncation":
		case "top_n":
			changes = append(changes, remove("/top_n"), mappingChange("/top_k", member.Value.Raw()))
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
	return doc, nil, err
}
func projectRerank(_ oif.Request, result oif.Result, _ oif.View, route string) (oif.Document, error) {
	doc := result.Source()
	data, exists := doc.Root().Lookup("data")
	if !exists {
		return oif.Document{}, operations.Violation("/data", "The target result omitted native ranking data.")
	}
	if _, exists := doc.Root().Lookup("results"); exists {
		return oif.Document{}, operations.Violation("/results", "Native result collides with the projected result location.")
	}
	changes, err := operations.ModelChanges(doc, route)
	if err != nil {
		return oif.Document{}, err
	}
	changes = append(changes, remove("/data"), mappingChange("/results", data.Raw()))
	return oif.Apply(doc, changes)
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
