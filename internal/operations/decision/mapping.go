package decision

import (
	"encoding/json"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
)

// System One has no refusal variant in 0.2.0. Its requests cannot be sent to
// OpenAI, which may refuse, until a source-side refusal contract is qualified.
// This direction receives only System One answers and preserves each one.
func Mappings() []operations.Mapping {
	return []operations.Mapping{{Source: identity(OpenAI), Target: identity(SystemOne), Lower: lowerSystemOne, Project: projectSystemOne, Evidence: "decisions-systemone-text/1"}}
}
func choiceKey(v oif.Value) string {
	if v.Kind() == oif.String {
		return text(v)
	}
	return v.Raw()
}
func parseDocument(v any) (oif.Document, error) {
	return oif.ParseJSON(operations.Raw(v), oif.Limits{})
}
func lowerSystemOne(source oif.Request, view oif.View) (oif.Document, []oif.Provenance, error) {
	r := view.(Request)
	root := source.Document().Root()
	if member(root, "input").Kind() != oif.String {
		return oif.Document{}, nil, invalid("input")
	}
	if !operations.Optional(member(root, "safety_identifier")) {
		return oif.Document{}, nil, invalid("safety_identifier")
	}
	questions := map[string]any{}
	for i, q := range r.Questions {
		native := map[string]any{"type": q.Kind, "instructions": json.RawMessage(member(q.Source, "instructions").Raw())}
		switch q.Kind {
		case "predicate":
			native["type"] = "noul"
		case "choice":
			criteria := map[string]any{}
			for j, v := range q.Values {
				key := choiceKey(v)
				if key == "" {
					return oif.Document{}, nil, invalid("choices.value")
				}
				if _, exists := criteria[key]; exists {
					return oif.Document{}, nil, invalid("choices.value")
				}
				entry := member(q.Source, "choices").Elements()[j]
				description := member(entry, "description")
				var d any
				if !operations.Optional(description) {
					d = json.RawMessage(description.Raw())
				}
				criteria[key] = d
			}
			native["criteria"] = criteria
		case "score":
			for _, v := range member(q.Source, "levels").Elements() {
				if !operations.Optional(member(v, "description")) {
					return oif.Document{}, nil, invalid("levels.description")
				}
			}
			native["criteria"] = q.Labels
		}
		questions["q"+strconv.Itoa(i)] = native
	}
	doc, err := parseDocument(map[string]any{"model": text(member(root, "model")), "state": json.RawMessage(member(root, "input").Raw()), "questions": questions})
	return doc, nil, err
}
func projectSystemOne(source oif.Request, result oif.Result, view oif.View, route string) (oif.Document, error) {
	requestView, err := parseRequest(source)
	if err != nil {
		return oif.Document{}, err
	}
	r := requestView.(Request)
	root := result.Source().Root()
	answers := []any{}
	for i, q := range r.Questions {
		a := member(member(root, "answers"), "q"+strconv.Itoa(i))
		var name any
		if value := member(q.Source, "name"); value.Kind() == oif.String {
			name = json.RawMessage(value.Raw())
		}
		answer := map[string]any{"type": q.Kind, "name": name}
		switch q.Kind {
		case "predicate":
			answer["probability"] = json.RawMessage(member(a, "noul").Raw())
		case "choice":
			probabilities := []any{}
			selected := text(member(a, "choice"))
			found := false
			for _, v := range q.Values {
				key := choiceKey(v)
				probabilities = append(probabilities, map[string]any{"value": json.RawMessage(v.Raw()), "probability": json.RawMessage(member(member(a, "probabilities"), key).Raw())})
				if selected == key {
					answer["choice"] = json.RawMessage(v.Raw())
					found = true
				}
			}
			if !found {
				return oif.Document{}, operations.Violation("choice", "original_choice_identity")
			}
			answer["probabilities"] = probabilities
			answer["confidence"] = json.RawMessage(member(a, "confidence").Raw())
		case "score":
			probabilities := []any{}
			for j, label := range q.Labels {
				probabilities = append(probabilities, map[string]any{"value": j, "label": label, "probability": json.RawMessage(member(member(a, "probabilities"), strconv.Itoa(j)).Raw())})
			}
			answer["probabilities"] = probabilities
			answer["score"] = json.RawMessage(member(a, "score").Raw())
			answer["confidence"] = json.RawMessage(member(a, "confidence").Raw())
		}
		answers = append(answers, answer)
	}
	usage := map[string]any{}
	for _, m := range member(root, "usage").Members() {
		usage[m.Name] = json.RawMessage(m.Value.Raw())
	}
	usage["total_tokens"] = *view.(Result).Usage.TotalTokens
	return parseDocument(map[string]any{"model": route, "answers": answers, "usage": usage})
}
