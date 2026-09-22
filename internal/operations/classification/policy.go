package classification

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"slices"
	"strconv"
)

func policyConflict(path string) error {
	return operations.Error("policy_conflict", path, "native_text_coverage", "The policy cannot inspect this native input scope.")
}
func known(root oif.Value, names ...string) error {
	for _, member := range root.Members() {
		if !slices.Contains(names, member.Name) {
			return policyConflict(operations.Pointer("", member.Name))
		}
	}
	return nil
}
func collect(value oif.Value, path string, keys bool) ([]operations.Text, error) {
	var texts []operations.Text
	switch value.Kind() {
	case oif.String:
		texts = append(texts, operations.Text{Pointer: path, Value: operations.String(value)})
	case oif.Array:
		for i, item := range value.Elements() {
			items, err := collect(item, path+"/"+strconv.Itoa(i), keys)
			if err != nil {
				return nil, err
			}
			texts = append(texts, items...)
		}
	case oif.Object:
		for _, member := range value.Members() {
			p := operations.Pointer(path, member.Name)
			if slices.Contains([]string{"encrypted_content", "encryptedContent", "data", "bytes", "file_id", "image_url"}, member.Name) {
				return nil, policyConflict(p)
			}
			if keys {
				texts = append(texts, operations.Text{Pointer: p, Value: member.Name})
			}
			items, err := collect(member.Value, p, keys)
			if err != nil {
				return nil, err
			}
			texts = append(texts, items...)
		}
	}
	return texts, nil
}
func moderationText(request oif.Request) ([]operations.Text, error) {
	view, err := moderationRequest(request)
	if err != nil {
		return nil, err
	}
	root := request.Document().Root()
	if err := known(root, "model", "input"); err != nil {
		return nil, err
	}
	if view.(ModerationRequest).scope.Kind == "joint-multimodal" {
		for _, item := range operations.Member(root, "input").Elements() {
			if operations.String(operations.Member(item, "type")) != "text" {
				return nil, policyConflict("/input")
			}
			if err := known(item, "type", "text"); err != nil {
				return nil, err
			}
		}
	}
	return collect(operations.Member(root, "input"), "/input", false)
}
func predictText(request oif.Request) ([]operations.Text, error) {
	if _, err := predictRequest(request); err != nil {
		return nil, err
	}
	root := request.Document().Root()
	if err := known(root, "inputs", "truncate", "truncation_direction", "raw_scores"); err != nil {
		return nil, err
	}
	return collect(operations.Member(root, "inputs"), "/inputs", false)
}
func scoringText(request oif.Request) ([]operations.Text, error) {
	if _, err := scoringRequest(request); err != nil {
		return nil, err
	}
	root := request.Document().Root()
	if err := known(root, "inputs", "parameters"); err != nil {
		return nil, err
	}
	if err := known(operations.Member(root, "inputs"), "source_sentence", "sentences"); err != nil {
		return nil, err
	}
	if err := known(operations.Member(root, "parameters"), "truncate", "truncation_direction", "prompt_name"); err != nil {
		return nil, err
	}
	if prompt, present := operations.Member(root, "parameters").Lookup("prompt_name"); present && prompt.Kind() != oif.Null {
		return nil, policyConflict("/parameters/prompt_name")
	}
	return collect(root, "", false)
}
func outputText(result oif.Result) ([]operations.Text, error) {
	return collect(result.Source().Root(), "", true)
}
