package decision

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
)

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
		if !validDecisionAnswer(answers, i, q, sys) {
			return fail()
		}
	}
	usage, ok := parseDecisionUsage(member(root, "usage"))
	if !ok {
		return fail()
	}
	r.Usage = usage
	return r, nil
}

func validDecisionAnswer(answers oif.Value, i int, q Question, sys bool) bool {
	var a oif.Value
	if sys {
		a = member(answers, q.Name)
	} else {
		a = answers.Elements()[i]
		name := member(a, "name")
		if !optionalText(name) {
			return false
		}
		if q.Name == "" {
			if !operations.Optional(name) && text(name) != "" {
				return false
			}
		} else if text(name) != q.Name {
			return false
		}
	}
	kind := text(member(a, "type"))
	if kind == "refusal" && !sys {
		return known(a, "type name") == nil
	}
	if kind != q.Kind {
		return false
	}
	names := "type name"
	if sys {
		names = "type"
	}
	if kind == "predicate" || kind == "noul" {
		key := "probability"
		if sys {
			key = "noul"
		}
		return known(a, names+" "+key) == nil && probability(member(a, key))
	}
	names += " confidence probabilities " + kind
	if sys && kind == "score" {
		names += " legend"
	}
	if known(a, names) != nil || !probability(member(a, "confidence")) {
		return false
	}
	if sys {
		return validSystemOneChoiceOrScore(a, q, kind)
	}
	return validOpenAIChoiceOrScore(a, q, kind)
}

func validSystemOneChoiceOrScore(a oif.Value, q Question, kind string) bool {
	ps := member(a, "probabilities")
	n := len(q.Labels)
	if ps.Kind() != oif.Object || len(ps.Members()) != n {
		return false
	}
	for j := 0; j < n; j++ {
		key := strconv.Itoa(j)
		if kind == "choice" {
			key = q.Labels[j]
		}
		if !probability(member(ps, key)) {
			return false
		}
	}
	if kind == "choice" {
		selected := member(a, "choice")
		if selected.Kind() != oif.String {
			return false
		}
		for _, v := range q.Labels {
			if text(selected) == v {
				return true
			}
		}
		return false
	}
	legend := member(a, "legend")
	if legend.Kind() != oif.Object || len(legend.Members()) != n || !bounded(member(a, "score"), 0, int64(n-1)) {
		return false
	}
	for j, label := range q.Labels {
		value := member(legend, strconv.Itoa(j))
		if value.Kind() != oif.String || text(value) != label {
			return false
		}
	}
	return true
}

func validOpenAIChoiceOrScore(a oif.Value, q Question, kind string) bool {
	ps := member(a, "probabilities")
	n := len(q.Labels)
	if kind == "choice" {
		n = len(q.Values)
	}
	if ps.Kind() != oif.Array || len(ps.Elements()) != n {
		return false
	}
	for j, p := range ps.Elements() {
		if !probability(member(p, "probability")) {
			return false
		}
		if kind == "choice" {
			if known(p, "value probability") != nil || !operations.SameValue(member(p, "value"), q.Values[j]) {
				return false
			}
		} else {
			if known(p, "label value probability") != nil || member(p, "label").Kind() != oif.String || text(member(p, "label")) != q.Labels[j] {
				return false
			}
			v, ok := operations.Int(member(p, "value"))
			if !ok || v != int64(j) {
				return false
			}
		}
	}
	if kind == "choice" {
		for _, v := range q.Values {
			if operations.SameValue(member(a, "choice"), v) {
				return true
			}
		}
		return false
	}
	return bounded(member(a, "score"), 0, int64(n-1))
}

func parseDecisionUsage(usage oif.Value) (*operations.Usage, bool) {
	in, ok := operations.Int(member(usage, "input_tokens"))
	if !ok || in < 0 {
		return nil, false
	}
	out, ok := operations.Int(member(usage, "output_tokens"))
	if !ok || out < 0 || out > math.MaxInt64-in {
		return nil, false
	}
	total := in + out
	if v, present := usage.Lookup("total_tokens"); present {
		n, ok := operations.Int(v)
		if !ok || n != total {
			return nil, false
		}
	}
	u := &operations.Usage{InputTokens: &in, OutputTokens: &out, TotalTokens: &total}
	if known(usage, "input_tokens output_tokens total_tokens input_tokens_details output_tokens_details") != nil {
		return nil, false
	}
	if details := member(usage, "input_tokens_details"); !operations.Optional(details) {
		if known(details, "cached_tokens cache_write_tokens") != nil {
			return nil, false
		}
		for _, field := range []string{"cached_tokens", "cache_write_tokens"} {
			if v, exists := details.Lookup(field); exists {
				n, ok := operations.Int(v)
				if !ok || n < 0 || n > in {
					return nil, false
				}
				if field == "cached_tokens" {
					u.CachedInputTokens = &n
				} else {
					u.CacheWriteInputTokens = &n
				}
			}
		}
	}
	if details := member(usage, "output_tokens_details"); !operations.Optional(details) {
		if known(details, "reasoning_tokens") != nil {
			return nil, false
		}
		if v, exists := details.Lookup("reasoning_tokens"); exists {
			n, ok := operations.Int(v)
			if !ok || n < 0 || n > out {
				return nil, false
			}
			u.ReasoningTokens = &n
		}
	}
	if u.CachedInputTokens != nil && u.CacheWriteInputTokens != nil && *u.CacheWriteInputTokens > in-*u.CachedInputTokens {
		return nil, false
	}
	return u, true
}

func inputText(r oif.Request) ([]operations.Text, error) {
	if _, err := parseRequest(r); err != nil {
		return nil, err
	}
	input := "input"
	if r.Descriptor().Dialect.ID == SystemOne {
		input = "state"
	}
	root := r.Document().Root()
	texts := collectText(member(root, input), "/"+input, false)
	return append(texts, collectText(member(root, "questions"), "/questions", true)...), nil
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
