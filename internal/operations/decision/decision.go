// Package decision owns the frozen, text-only Decisions and System One
// contracts. It never invokes a chat model to synthesize probabilities.
package decision

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
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
		found := false
		for _, n := range allowed {
			found = found || n == m.Name
		}
		if !found {
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
	return out
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
	var values []oif.Value
	max := 255
	if q.Kind == "score" {
		max = 10
	}
	if sys {
		criteria := member(v, "criteria")
		if q.Kind == "choice" {
			if criteria.Kind() != oif.Object {
				return q, invalid("criteria")
			}
			if len(criteria.Members()) < 2 || len(criteria.Members()) > max {
				return q, invalid("criteria")
			}
			for _, m := range criteria.Members() {
				if !optionalText(m.Value) {
					return q, invalid("criteria")
				}
				q.Labels = append(q.Labels, m.Name)
			}
		} else {
			if criteria.Kind() != oif.Array {
				return q, invalid("criteria")
			}
			if len(criteria.Elements()) < 2 || len(criteria.Elements()) > max {
				return q, invalid("criteria")
			}
			for _, v := range criteria.Elements() {
				if v.Kind() != oif.String {
					return q, invalid("criteria")
				}
				q.Labels = append(q.Labels, text(v))
			}
		}
	} else {
		field := "choices"
		if q.Kind == "score" {
			field = "levels"
		}
		vs := member(v, field)
		if vs.Kind() != oif.Array {
			return q, invalid(field)
		}
		values = vs.Elements()
		if len(values) < 2 || len(values) > max {
			return q, invalid(field)
		}
		for _, v := range values {
			if q.Kind == "choice" {
				if err := known(v, "value description"); err != nil {
					return q, err
				}
				value := member(v, "value")
				if value.Kind() != oif.String && value.Kind() != oif.Boolean {
					return q, invalid("choices.value")
				}
				for _, prior := range q.Values {
					if operations.SameValue(prior, value) {
						return q, invalid("choices.value")
					}
				}
				q.Values = append(q.Values, value)
			} else {
				if err := known(v, "label description"); err != nil {
					return q, err
				}
				if member(v, "label").Kind() != oif.String {
					return q, invalid("levels.label")
				}
				q.Labels = append(q.Labels, text(member(v, "label")))
			}
			if !optionalText(member(v, "description")) {
				return q, invalid("description")
			}
		}
	}
	n := len(q.Labels)
	if !sys && q.Kind == "choice" {
		n = len(q.Values)
	}
	if n < 2 || n > max {
		return q, invalid("criteria")
	}
	return q, nil
}

func probability(v oif.Value) bool { return bounded(v, 0, 1) }
func bounded(v oif.Value, low, high int64) bool {
	if v.Kind() != oif.Number || len(v.Raw()) > 128 {
		return false
	}
	// Bound decimal exponents before math/big expands them. Wire numbers
	// remain exact, including tiny probabilities, without unbounded allocation.
	if i := strings.IndexAny(v.Raw(), "eE"); i >= 0 {
		exponent, err := strconv.Atoi(v.Raw()[i+1:])
		if err != nil || exponent < -512 || exponent > 512 {
			return false
		}
	}
	n, ok := new(big.Rat).SetString(v.Raw())
	return ok && n.Cmp(big.NewRat(low, 1)) >= 0 && n.Cmp(big.NewRat(high, 1)) <= 0
}
func parseResult(request oif.Request, source oif.Result) (oif.View, error) {
	rv, err := parseRequest(request)
	if err != nil {
		return nil, err
	}
	r := Result{Source: source}
	root := source.Source().Root()
	sys := request.Descriptor().Dialect.ID == SystemOne
	fail := func() (oif.View, error) {
		return r, operations.Violation("answers", "decision_identity_and_probability_contract")
	}
	if known(root, "model answers usage") != nil || member(root, "model").Kind() != oif.String {
		return fail()
	}
	qs := rv.(Request).Questions
	answers := member(root, "answers")
	if sys && answers.Kind() != oif.Object || !sys && answers.Kind() != oif.Array {
		return fail()
	}
	count := len(answers.Elements())
	if sys {
		count = len(answers.Members())
	}
	if count != len(qs) {
		return fail()
	}
	for i, q := range qs {
		var a oif.Value
		if sys {
			a = member(answers, q.Name)
		} else {
			a = answers.Elements()[i]
			name := member(a, "name")
			if !optionalText(name) {
				return fail()
			}
			if q.Name == "" {
				if !operations.Optional(name) && text(name) != "" {
					return fail()
				}
			} else if text(name) != q.Name {
				return fail()
			}
		}
		kind := text(member(a, "type"))
		if kind == "refusal" && !sys {
			if known(a, "type name") != nil {
				return fail()
			}
			continue
		}
		if kind != q.Kind {
			return fail()
		}
		names := "type name"
		if sys {
			names = "type"
		}
		switch kind {
		case "predicate", "noul":
			key := "probability"
			if sys {
				key = "noul"
			}
			if known(a, names+" "+key) != nil || !probability(member(a, key)) {
				return fail()
			}
		case "choice", "score":
			names += " confidence probabilities " + kind
			if sys && kind == "score" {
				names += " legend"
			}
			if known(a, names) != nil || !probability(member(a, "confidence")) {
				return fail()
			}
			ps := member(a, "probabilities")
			n := len(q.Labels)
			if kind == "choice" && !sys {
				n = len(q.Values)
			}
			if sys {
				if ps.Kind() != oif.Object || len(ps.Members()) != n {
					return fail()
				}
				for j := 0; j < n; j++ {
					key := strconv.Itoa(j)
					if kind == "choice" {
						key = q.Labels[j]
					}
					if !probability(member(ps, key)) {
						return fail()
					}
				}
				if kind == "choice" {
					selected := member(a, "choice")
					if selected.Kind() != oif.String {
						return fail()
					}
					found := false
					for _, v := range q.Labels {
						found = found || text(selected) == v
					}
					if !found {
						return fail()
					}
				} else {
					legend := member(a, "legend")
					if legend.Kind() != oif.Object || len(legend.Members()) != n || !bounded(member(a, "score"), 0, int64(n-1)) {
						return fail()
					}
					for j, label := range q.Labels {
						value := member(legend, strconv.Itoa(j))
						if value.Kind() != oif.String || text(value) != label {
							return fail()
						}
					}
				}
			} else {
				if ps.Kind() != oif.Array || len(ps.Elements()) != n {
					return fail()
				}
				for j, p := range ps.Elements() {
					if !probability(member(p, "probability")) {
						return fail()
					}
					if kind == "choice" {
						if known(p, "value probability") != nil || !operations.SameValue(member(p, "value"), q.Values[j]) {
							return fail()
						}
					} else {
						if known(p, "label value probability") != nil || member(p, "label").Kind() != oif.String || text(member(p, "label")) != q.Labels[j] {
							return fail()
						}
						v, ok := operations.Int(member(p, "value"))
						if !ok || v != int64(j) {
							return fail()
						}
					}
				}
				if kind == "choice" {
					found := false
					for _, v := range q.Values {
						found = found || operations.SameValue(member(a, "choice"), v)
					}
					if !found {
						return fail()
					}
				} else if !bounded(member(a, "score"), 0, int64(n-1)) {
					return fail()
				}
			}
		}
	}
	usage := member(root, "usage")
	var in, out int64
	var ok bool
	if in, ok = operations.Int(member(usage, "input_tokens")); !ok || in < 0 {
		return fail()
	}
	if out, ok = operations.Int(member(usage, "output_tokens")); !ok || out < 0 || out > math.MaxInt64-in {
		return fail()
	}
	total := in + out
	if v, present := usage.Lookup("total_tokens"); present {
		n, ok := operations.Int(v)
		if !ok || n != total {
			return fail()
		}
	}
	r.Usage = &operations.Usage{InputTokens: &in, OutputTokens: &out, TotalTokens: &total}
	if known(usage, "input_tokens output_tokens total_tokens input_tokens_details output_tokens_details") != nil {
		return fail()
	}
	if details := member(usage, "input_tokens_details"); !operations.Optional(details) {
		if known(details, "cached_tokens cache_write_tokens") != nil {
			return fail()
		}
		for _, field := range []string{"cached_tokens", "cache_write_tokens"} {
			if v, exists := details.Lookup(field); exists {
				n, ok := operations.Int(v)
				if !ok || n < 0 || n > in {
					return fail()
				}
				if field == "cached_tokens" {
					r.Usage.CachedInputTokens = &n
				} else {
					r.Usage.CacheWriteInputTokens = &n
				}
			}
		}
	}
	if details := member(usage, "output_tokens_details"); !operations.Optional(details) {
		if known(details, "reasoning_tokens") != nil {
			return fail()
		}
		if v, exists := details.Lookup("reasoning_tokens"); exists {
			n, ok := operations.Int(v)
			if !ok || n < 0 || n > out {
				return fail()
			}
			r.Usage.ReasoningTokens = &n
		}
	}
	if r.Usage.CachedInputTokens != nil && r.Usage.CacheWriteInputTokens != nil && *r.Usage.CacheWriteInputTokens > in-*r.Usage.CachedInputTokens {
		return fail()
	}
	return r, nil
}
func inputText(r oif.Request) ([]operations.Text, error) {
	if _, err := parseRequest(r); err != nil {
		return nil, err
	}
	return collectText(r.Document().Root(), "", true), nil
}
func outputText(r oif.Result) ([]operations.Text, error) {
	return collectText(member(r.Source().Root(), "answers"), "/answers", true), nil
}
func collectText(v oif.Value, path string, keys bool) []operations.Text {
	out := []operations.Text{}
	switch v.Kind() {
	case oif.String:
		out = append(out, operations.Text{Pointer: path, Value: text(v)})
	case oif.Array:
		for i, v := range v.Elements() {
			out = append(out, collectText(v, path+"/"+strconv.Itoa(i), keys)...)
		}
	case oif.Object:
		for _, m := range v.Members() {
			p := oif.Pointer(path, m.Name)
			if keys {
				out = append(out, operations.Text{Pointer: p, Value: m.Name})
			}
			out = append(out, collectText(m.Value, p, keys)...)
		}
	}
	return out
}
