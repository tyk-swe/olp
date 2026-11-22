package decision

import (
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
)

const OpenAI = "openai-decisions"
const SystemOne = "systemone"

func identity(id string) oif.Identity        { return oif.Identity{ID: id, Revision: operations.Revision} }
func member(v oif.Value, k string) oif.Value { return operations.Member(v, k) }
func text(v oif.Value) string                { return operations.String(v) }
func invalid(p string) error {
	return operations.Invalid(p, "The field is outside the frozen text-only decision contract.")
}
func known(v oif.Value, names string) error {
	if v.Kind() != oif.Object {
		return invalid("request")
	}
	allowed := strings.Fields(names)
	for _, m := range v.Members() {
		if !slices.Contains(allowed, m.Name) {
			return invalid(m.Name)
		}
	}
	return nil
}
func optionalText(v oif.Value) bool { return operations.Optional(v) || v.Kind() == oif.String }

type Question struct {
	Name, Kind string
	Source     oif.Value
	Values     []oif.Value
	Labels     []string
}
type Request struct {
	source    oif.Request
	Questions []Question
}

func (r Request) Schema() oif.Identity { return identity("decision") }

type Result struct {
	Source oif.Result
	Usage  *operations.Usage
}

func (r Result) Schema() oif.Identity { return identity("decision") }

func Definitions() []operations.Dialect {
	out := []operations.Dialect{}
	for _, name := range []string{OpenAI, SystemOne} {
		path, input, surface := "decisions", "input", "openai"
		docs := "https://github.com/openai/openai-go/blob/v3.78.0/decision.go"
		if name == SystemOne {
			path, input = "systemone", "state"
			docs = "https://api.typesafe.ai/redoc"
		}
		d := operations.Dialect{Identity: identity(name), Operation: identity("decision"), Surface: surface, Label: name + " text decisions", Documentation: docs, Address: operations.Address{FamilyPath: path}, Evidence: name + "-text-native/1", Request: parseRequest, Result: parseResult, BindModel: bindModel, BindResultModel: operations.ModelChanges, InputText: inputText, OutputText: outputText, Estimate: func(v oif.View) int64 { return estimate.HeuristicBytesTokens(v.(Request).source.Document().Len()) }, Usage: func(v oif.View) *operations.Usage { return v.(Result).Usage }}
		d.RequestSchema = operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, input: map[string]any{"type": []string{"string", "array"}}, "questions": map[string]any{"type": "array"}}, "model", input, "questions")
		if name == SystemOne {
			d.RequestSchema = operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, input: map[string]any{"type": []string{"string", "array"}}, "questions": map[string]any{"type": "object"}}, "model", input, "questions")
		}
		d.ResultSchema = operations.ObjectSchema(map[string]any{"model": map[string]any{"type": "string"}, "answers": map[string]any{}, "usage": map[string]any{"type": "object"}}, "model", "answers", "usage")
		d.Probe = func(model string) []byte {
			if name == SystemOne {
				return operations.Raw(map[string]any{"model": model, "state": "A neutral test.", "questions": map[string]any{"check": map[string]any{"type": "noul", "instructions": "Is this text?"}}})
			}
			return operations.Raw(map[string]any{"model": model, "input": "A neutral test.", "questions": []any{map[string]any{"name": "check", "type": "predicate", "instructions": "Is this text?"}}})
		}
		out = append(out, d)
	}
	return append(out, geminiDefinition())
}

func bindModel(d oif.Document, model string) ([]oif.Change, error) {
	return []oif.Change{{Pointer: "/model", Value: string(operations.Raw(model)), Origin: oif.IdentityBinding, Reason: "published model binding"}}, nil
}

func parseRequest(source oif.Request) (oif.View, error) {
	r := Request{source: source}
	root := source.Document().Root()
	sys := source.Descriptor().Dialect.ID == SystemOne
	allowed, input := "model input questions safety_identifier", "input"
	if sys {
		allowed, input = "model state questions", "state"
	}
	if err := known(root, allowed); err != nil {
		return r, err
	}
	if member(root, "model").Kind() != oif.String {
		return r, invalid("model")
	}
	if !optionalText(member(root, "safety_identifier")) {
		return r, invalid("safety_identifier")
	}
	if err := validateInput(member(root, input), sys); err != nil {
		return r, err
	}
	qs := member(root, "questions")
	if sys {
		if qs.Kind() != oif.Object {
			return r, invalid("questions")
		}
		for _, m := range qs.Members() {
			q, err := question(m.Value, m.Name, sys)
			if err != nil {
				return r, err
			}
			r.Questions = append(r.Questions, q)
		}
	} else {
		if qs.Kind() != oif.Array {
			return r, invalid("questions")
		}
		for _, v := range qs.Elements() {
			q, err := question(v, text(member(v, "name")), sys)
			if err != nil {
				return r, err
			}
			r.Questions = append(r.Questions, q)
		}
	}
	if len(r.Questions) < 1 || len(r.Questions) > 256 {
		return r, invalid("questions")
	}
	return r, nil
}

func validateInput(v oif.Value, sys bool) error {
	if v.Kind() == oif.String {
		return nil
	}
	if v.Kind() != oif.Array || len(v.Elements()) == 0 {
		return invalid("input")
	}
	for _, item := range v.Elements() {
		if sys {
			if item.Kind() != oif.String {
				return invalid("state")
			}
			continue
		}
		if err := known(item, "type role content"); err != nil {
			return err
		}
		if text(member(item, "role")) != "user" || !operations.Optional(member(item, "type")) && text(member(item, "type")) != "message" {
			return invalid("input.role")
		}
		content := member(item, "content")
		if content.Kind() == oif.String {
			continue
		}
		if content.Kind() != oif.Array || len(content.Elements()) == 0 {
			return invalid("input.content")
		}
		for _, part := range content.Elements() {
			if err := known(part, "type text"); err != nil {
				return err
			}
			if text(member(part, "type")) != "input_text" || member(part, "text").Kind() != oif.String {
				return invalid("input.content")
			}
		}
	}
	return nil
}

func question(v oif.Value, name string, sys bool) (Question, error) {
	q := Question{Name: name, Kind: text(member(v, "type")), Source: v}
	allowed := "type name instructions"
	if sys {
		allowed = "type instructions"
	}
	switch q.Kind {
	case "predicate":
		if sys {
			return q, invalid("type")
		}
	case "noul":
		if !sys {
			return q, invalid("type")
		}
		allowed += " criteria"
	case "choice", "score":
		if sys {
			allowed += " criteria"
		} else if q.Kind == "choice" {
			allowed += " choices"
		} else {
			allowed += " levels"
		}
	default:
		return q, invalid("type")
	}
	if err := known(v, allowed); err != nil {
		return q, err
	}
	if !optionalText(member(v, "name")) || member(v, "instructions").Kind() != oif.String {
		return q, invalid("instructions")
	}
	if sys && name == "" {
		return q, invalid("questions")
	}
	if q.Kind == "predicate" {
		return q, nil
	}
	if q.Kind == "noul" {
		c := member(v, "criteria")
		if !operations.Optional(c) {
			if err := known(c, "true false"); err != nil {
				return q, err
			}
			for _, m := range c.Members() {
				if !optionalText(m.Value) {
					return q, invalid("criteria")
				}
			}
		}
		return q, nil
	}
	limit := 255
	if q.Kind == "score" {
		limit = 10
	}
	if sys {
		if err := parseSystemOneQuestionOptions(v, &q, limit); err != nil {
			return q, err
		}
	} else if err := parseOpenAIQuestionOptions(v, &q, limit); err != nil {
		return q, err
	}
	n := len(q.Labels)
	if !sys && q.Kind == "choice" {
		n = len(q.Values)
	}
	if n < 2 || n > limit {
		return q, invalid("criteria")
	}
	return q, nil
}

func parseSystemOneQuestionOptions(v oif.Value, q *Question, limit int) error {
	criteria := member(v, "criteria")
	if q.Kind == "choice" {
		if criteria.Kind() != oif.Object || len(criteria.Members()) < 2 || len(criteria.Members()) > limit {
			return invalid("criteria")
		}
		for _, m := range criteria.Members() {
			if !optionalText(m.Value) {
				return invalid("criteria")
			}
			q.Labels = append(q.Labels, m.Name)
		}
		return nil
	}
	if criteria.Kind() != oif.Array || len(criteria.Elements()) < 2 || len(criteria.Elements()) > limit {
		return invalid("criteria")
	}
	for _, elem := range criteria.Elements() {
		if elem.Kind() != oif.String {
			return invalid("criteria")
		}
		q.Labels = append(q.Labels, text(elem))
	}
	return nil
}

func parseOpenAIQuestionOptions(v oif.Value, q *Question, limit int) error {
	field := "choices"
	if q.Kind == "score" {
		field = "levels"
	}
	vs := member(v, field)
	if vs.Kind() != oif.Array {
		return invalid(field)
	}
	values := vs.Elements()
	if len(values) < 2 || len(values) > limit {
		return invalid(field)
	}
	for _, entry := range values {
		if q.Kind == "choice" {
			if err := known(entry, "value description"); err != nil {
				return err
			}
			value := member(entry, "value")
			if value.Kind() != oif.String && value.Kind() != oif.Boolean {
				return invalid("choices.value")
			}
			for _, prior := range q.Values {
				if operations.SameValue(prior, value) {
					return invalid("choices.value")
				}
			}
			q.Values = append(q.Values, value)
		} else {
			if err := known(entry, "label description"); err != nil {
				return err
			}
			if member(entry, "label").Kind() != oif.String {
				return invalid("levels.label")
			}
			q.Labels = append(q.Labels, text(member(entry, "label")))
		}
		if !optionalText(member(entry, "description")) {
			return invalid("description")
		}
	}
	return nil
}
