package decision

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
)

const Gemini = "gemini-decisions"

var geminiQuestionKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func geminiDefinition() operations.Dialect {
	return operations.Dialect{
		Identity: identity(Gemini), Operation: identity("decision"), Surface: "gemini",
		Label: "Gemini Decisions", Documentation: "https://aistudio.google.com/api-reference/decisions/create-decision",
		Address: operations.Address{FamilyPath: "gemini_decisions"}, Evidence: "gemini-decisions-v1beta/1",
		Request: parseGeminiRequest, Result: parseGeminiResult, BindModel: bindModel,
		InputText: geminiInputText, OutputText: geminiOutputText, FetchedDocument: geminiFetchesInput,
		Estimate: func(v oif.View) int64 { return estimate.HeuristicBytesTokens(v.(Request).source.Document().Len()) },
		Usage:    func(v oif.View) *operations.Usage { return v.(Result).Usage },
		RequestSchema: operations.ObjectSchema(map[string]any{
			"model":     map[string]any{"type": "string"},
			"input":     map[string]any{"type": []string{"string", "object", "array"}},
			"questions": map[string]any{"type": "object"},
		}, "model", "input", "questions"),
		ResultSchema: operations.ObjectSchema(map[string]any{
			"answers":         map[string]any{"type": "object"},
			"usage":           map[string]any{"type": "object"},
			"prompt_feedback": map[string]any{"type": "object"},
		}),
		Probe: func(model string) []byte {
			return operations.Raw(map[string]any{
				"model": model, "input": "A neutral test.",
				"questions": map[string]map[string]string{"check": {"type": "noul", "instructions": "Does this contain text?"}},
			})
		},
	}
}

func parseGeminiRequest(source oif.Request) (oif.View, error) {
	root := source.Document().Root()
	if err := known(root, "model input questions"); err != nil {
		return nil, operations.Invalid("/request", "Use a Decisions object with model, input, and questions.")
	}
	if text(member(root, "model")) == "" {
		return nil, operations.Invalid("/model", "Provide a model.")
	}
	if err := validateGeminiContent(member(root, "input"), "/input", true); err != nil {
		return nil, err
	}
	questions := member(root, "questions").Members()
	if len(questions) < 1 || len(questions) > 1000 {
		return nil, operations.Invalid("/questions", "Provide 1 to 1000 keyed questions.")
	}
	for _, q := range questions {
		if !geminiQuestionKey.MatchString(q.Name) {
			return nil, operations.Invalid("/questions", "Use question IDs of up to 64 ASCII letters, digits, underscores, or hyphens.")
		}
		at := operations.Pointer("/questions", q.Name)
		if known(q.Value, "type instructions criteria") != nil {
			return nil, operations.Invalid(at, "Use type, instructions, and optional criteria for each question.")
		}
		if err := validateGeminiContent(member(q.Value, "instructions"), at+"/instructions", false); err != nil {
			return nil, err
		}
		if !validGeminiCriteria(text(member(q.Value, "type")), member(q.Value, "criteria")) {
			return nil, operations.Invalid(at+"/criteria", "Use optional yes/no descriptions for noul, 2 to 255 keyed option descriptions for choice, or 2 to 255 ordered level descriptions for score.")
		}
	}
	return Request{source: source}, nil
}

func validGeminiCriteria(kind string, criteria oif.Value) bool {
	switch kind {
	case "noul":
		return operations.Optional(criteria) || (known(criteria, "true_description false_description") == nil &&
			optionalText(member(criteria, "true_description")) && optionalText(member(criteria, "false_description")))
	case "choice":
		options := criteria.Members()
		if len(options) < 2 || len(options) > 255 {
			return false
		}
		for _, option := range options {
			if option.Name == "" || strings.TrimSpace(option.Name) != option.Name || utf8.RuneCountInString(option.Name) > 128 ||
				strings.ContainsFunc(option.Name, func(r rune) bool { return unicode.IsControl(r) || (unicode.IsSpace(r) && r != ' ') }) ||
				option.Value.Kind() != oif.String {
				return false
			}
		}
		return true
	case "score":
		levels := criteria.Elements()
		if len(levels) < 2 || len(levels) > 255 {
			return false
		}
		for _, level := range levels {
			if text(level) == "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func parseGeminiResult(request oif.Request, source oif.Result) (oif.View, error) {
	root := source.Source().Root()
	questions := member(request.Document().Root(), "questions").Members()
	answers := member(root, "answers")
	feedback := member(root, "prompt_feedback")
	fail := operations.Violation("/answers", "decision_identity_and_probability_contract")
	if root.Kind() != oif.Object || (!operations.Optional(feedback) && feedback.Kind() != oif.Object) {
		return nil, fail
	}
	if reason := member(feedback, "block_reason"); !operations.Optional(reason) {
		if text(reason) == "" || len(answers.Members()) != 0 || (answers.Kind() != oif.Object && answers.Kind() != oif.Absent) {
			return nil, fail
		}
	} else {
		if answers.Kind() != oif.Object || len(answers.Members()) != len(questions) {
			return nil, fail
		}
		for _, q := range questions {
			if !validGeminiAnswer(q.Value, member(answers, q.Name)) {
				return nil, fail
			}
		}
	}
	usage, err := geminiUsage(member(root, "usage"))
	if err != nil {
		return nil, err
	}
	return Result{Source: source, Usage: usage}, nil
}

func validGeminiAnswer(question, answer oif.Value) bool {
	kind := text(member(question, "type"))
	if text(member(answer, "type")) != kind || !probability(member(answer, "confidence")) {
		return false
	}
	criteria := member(question, "criteria")
	probs := member(answer, "probabilities")
	switch kind {
	case "noul":
		return probability(member(answer, "noul"))
	case "choice":
		options := criteria.Members()
		if len(probs.Members()) != len(options) || member(criteria, text(member(answer, "choice"))).Kind() != oif.String {
			return false
		}
		for _, option := range options {
			if !probability(member(probs, option.Name)) {
				return false
			}
		}
		return true
	case "score":
		levels := criteria.Elements()
		legend := member(answer, "legend")
		if !bounded(member(answer, "score"), 0, int64(len(levels)-1)) || len(legend.Members()) != len(levels) ||
			len(probs.Members()) != len(levels) {
			return false
		}
		for i, level := range levels {
			key := strconv.Itoa(i)
			if !operations.SameValue(member(legend, key), level) || !probability(member(probs, key)) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func geminiUsage(usage oif.Value) (*operations.Usage, error) {
	if operations.Optional(usage) {
		return nil, nil
	}
	input, inputOK := operations.Int(member(usage, "total_input_tokens"))
	output, outputOK := operations.Int(member(usage, "total_output_tokens"))
	total, totalOK := operations.Int(member(usage, "total_tokens"))
	if !inputOK || input < 0 || !outputOK || output != 0 || !totalOK || total < input {
		return nil, operations.Violation("/usage", "gemini_decision_usage")
	}
	return &operations.Usage{InputTokens: &input, OutputTokens: &output, TotalTokens: &total}, nil
}

func geminiOutputText(source oif.Result) ([]operations.Text, error) {
	root := source.Source().Root()
	return append(collectText(member(root, "answers"), "/answers", true),
		collectText(member(root, "prompt_feedback"), "/prompt_feedback", false)...), nil
}
