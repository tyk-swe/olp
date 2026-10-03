package scripted

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// The script language. A test controls what the upstream answers by writing
// directives into the user prompt of the request it sends through the client.
// Every directive is `[[olp:KEYWORD ARGUMENT]]`, and arguments are JSON so a
// prompt can carry any text:
//
//	[[olp:tool NAME {"json":"arguments"}]]   call tool NAME in the next round
//	[[olp:also NAME {"json":"arguments"}]]   add a parallel call to that round
//	[[olp:reply "text"]]                     the final text instead of the default
//	[[olp:think "text"]]                     include reasoning with this text
//	[[olp:fail 429]]                         answer with this upstream error status
//	                                         (a 429 says to retry after a second)
//
// A conversation is answered from its content alone. The directives come from
// the latest user prompt (the last user message without tool results). Each
// tool directive is one round: the fixture calls the round's tools while the
// results after the prompt do not yet cover that round, and answers with final
// text once they do. A tool that the request did not declare is never called;
// the reply says so, and the test fails on its own assertion. A request
// without directives gets a plain text reply, or a call of the tool a request
// forces, or a JSON instance of the schema a request asks for.

const (
	// DefaultReply answers a request that carries no directive.
	DefaultReply = "Hello from the OLP client qualification fixture."
	// DefaultReasoning is the reasoning of a request that asks for it.
	DefaultReasoning = "Fixture reasoning: the reply is scripted."
	// ToolResultsPrefix starts the final text after a tool loop; the tool
	// results follow, separated by " | ".
	ToolResultsPrefix = "The tools returned: "

	directiveOpen  = "[[olp:"
	directiveClose = "]]"
)

type invocation struct {
	round, index int
	name         string
	args         json.RawMessage
}

// id derives a stable identifier from the position in the script.
func (i invocation) id(prefix string) string {
	return fmt.Sprintf("%s_fx_%d_%d", prefix, i.round+1, i.index+1)
}

type directives struct {
	rounds [][]invocation
	reply  *string
	think  string
	fail   int
}

func parseDirectives(text string) (directives, error) {
	var d directives
	for {
		at := strings.Index(text, directiveOpen)
		if at < 0 {
			return d, nil
		}
		rest, err := d.parse(text[at+len(directiveOpen):])
		if err != nil {
			return d, fmt.Errorf("malformed %s...%s directive: %w", directiveOpen, directiveClose, err)
		}
		text = rest
	}
}

// parse consumes one directive after its opening and returns the remaining text.
func (d *directives) parse(text string) (string, error) {
	keyword, rest := word(text)
	switch keyword {
	case "tool", "also":
		name, rest := word(rest)
		if name == "" {
			return "", errors.New("a tool directive needs a tool name")
		}
		args, rest, err := value(rest, true)
		if err != nil {
			return "", err
		}
		if !bytes.HasPrefix(bytes.TrimSpace(args), []byte("{")) {
			return "", errors.New("tool arguments must be a JSON object")
		}
		if keyword == "tool" {
			d.rounds = append(d.rounds, nil)
		} else if len(d.rounds) == 0 {
			return "", errors.New("also needs a tool directive before it")
		}
		last := len(d.rounds) - 1
		d.rounds[last] = append(d.rounds[last], invocation{round: last, index: len(d.rounds[last]), name: name, args: args})
		return rest, nil
	case "reply", "think":
		raw, rest, err := value(rest, false)
		if err != nil {
			return "", err
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("%s needs a JSON string", keyword)
		}
		if keyword == "reply" {
			d.reply = &s
		} else {
			d.think = s
		}
		return rest, nil
	case "fail":
		raw, rest, err := value(rest, false)
		if err != nil {
			return "", err
		}
		if json.Unmarshal(raw, &d.fail) != nil || d.fail < 400 || d.fail > 599 {
			return "", errors.New("fail needs an HTTP error status")
		}
		return rest, nil
	}
	return "", fmt.Errorf("unknown keyword %q", keyword)
}

// word reads up to the next space or directive end.
func word(text string) (string, string) {
	text = strings.TrimLeft(text, " \t\r\n")
	end := strings.IndexAny(text, " \t\r\n")
	if c := strings.Index(text, directiveClose); c >= 0 && (end < 0 || c < end) {
		end = c
	}
	if end < 0 {
		return text, ""
	}
	return text[:end], text[end:]
}

// value reads one JSON value and the closing of the directive. With optional
// set, a directive that closes immediately has the empty object.
func value(text string, optional bool) (json.RawMessage, string, error) {
	text = strings.TrimLeft(text, " \t\r\n")
	if rest, ok := strings.CutPrefix(text, directiveClose); ok {
		if optional {
			return json.RawMessage("{}"), rest, nil
		}
		return nil, "", errors.New("missing argument")
	}
	dec := json.NewDecoder(strings.NewReader(text))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("argument is not JSON: %w", err)
	}
	rest := strings.TrimLeft(text[dec.InputOffset():], " \t\r\n")
	rest, ok := strings.CutPrefix(rest, directiveClose)
	if !ok {
		return nil, "", errors.New("missing closing ]]")
	}
	return raw, rest, nil
}

// turnMessage is one user-side message: its text, or the tool results it
// carries. Assistant messages play no part in a decision.
type turnMessage struct {
	text    string
	results []string
}

// turn is the dialect-neutral content of a request.
type turn struct {
	messages []turnMessage
	// tools maps each declared tool to its input schema; order keeps the
	// declaration order.
	tools map[string]json.RawMessage
	order []string
	// forced is the tool a request requires: a name, or "*" for any tool.
	forced string
	// schema is the JSON schema of a structured output request; jsonMode asks
	// for JSON without a schema.
	schema   json.RawMessage
	jsonMode bool
	// thinking is whether the request enabled reasoning output.
	thinking bool
}

func newTurn() *turn { return &turn{tools: map[string]json.RawMessage{}} }

func (t *turn) declare(name string, schema json.RawMessage) {
	if name == "" {
		return
	}
	if _, ok := t.tools[name]; !ok {
		t.order = append(t.order, name)
	}
	t.tools[name] = schema
}

// hideTools models tool_choice none: the model sees no tool to call.
func (t *turn) hideTools() { t.tools, t.order, t.forced = map[string]json.RawMessage{}, nil, "" }

func (t *turn) user(text string) { t.messages = append(t.messages, turnMessage{text: text}) }

func (t *turn) results(r ...string) { t.messages = append(t.messages, turnMessage{results: r}) }

// reply is the neutral answer a dialect encodes into its own wire format.
type reply struct {
	script    string
	reasoning string
	text      string
	calls     []invocation
	// failStatus, when set, answers with an upstream error instead.
	failStatus  int
	failMessage string
}

// decide answers a turn. It is a pure function of the turn.
func decide(t *turn) reply {
	anchor := -1
	for i := len(t.messages) - 1; i >= 0; i-- {
		if m := t.messages[i]; len(m.results) == 0 && m.text != "" {
			anchor = i
			break
		}
	}
	var prompt string
	var results []string
	for i, m := range t.messages {
		if i > anchor {
			results = append(results, m.results...)
		}
		if i == anchor {
			prompt = m.text
		}
	}
	d, err := parseDirectives(prompt)
	if err != nil {
		return reply{script: "directive_error", failStatus: http.StatusBadRequest, failMessage: err.Error()}
	}
	if d.fail != 0 {
		return reply{script: "fail", failStatus: d.fail, failMessage: fmt.Sprintf("The fixture upstream was scripted to fail with status %d.", d.fail)}
	}
	r := reply{reasoning: d.think}
	if r.reasoning == "" && t.thinking {
		r.reasoning = DefaultReasoning
	}
	// Rounds whose results are all present are done.
	done, answered := 0, len(results)
	for done < len(d.rounds) && answered >= len(d.rounds[done]) {
		answered -= len(d.rounds[done])
		done++
	}
	switch {
	case done < len(d.rounds):
		for _, call := range d.rounds[done] {
			if _, ok := t.tools[call.name]; !ok {
				r.script = "tool_undeclared"
				r.text = fmt.Sprintf("olp fixture: tool %s is not declared in this request", call.name)
				return r
			}
		}
		r.script, r.calls = "tool_call", d.rounds[done]
	case len(d.rounds) == 0 && t.forced != "" && len(results) == 0:
		name := t.forced
		if name == "*" && len(t.order) > 0 {
			name = t.order[0]
		}
		schema, ok := t.tools[name]
		if !ok {
			r.script, r.text = "tool_undeclared", fmt.Sprintf("olp fixture: tool %s is not declared in this request", name)
			return r
		}
		args, _ := json.Marshal(sample(schema))
		r.script, r.calls = "tool_call", []invocation{{name: name, args: args}}
	default:
		r.script, r.text = finalText(t, d, results)
	}
	return r
}

func finalText(t *turn, d directives, results []string) (script, text string) {
	switch {
	case d.reply != nil:
		return "reply", *d.reply
	case t.schema != nil:
		encoded, _ := json.Marshal(sample(t.schema))
		return "structured", string(encoded)
	case t.jsonMode:
		return "structured", `{"result":"fixture"}`
	case len(results) > 0:
		clipped := make([]string, len(results))
		for i, r := range results {
			clipped[i] = clip(r, 400)
		}
		return "tool_result", ToolResultsPrefix + strings.Join(clipped, " | ")
	}
	return "text", DefaultReply
}

// words splits text into stream deltas that concatenate back to the text.
func words(text string) []string {
	if text == "" {
		return nil
	}
	return strings.SplitAfter(text, " ")
}

// fragments splits a JSON string into two stream fragments, so a client has
// to assemble tool arguments from more than one delta. The split keeps every
// character whole.
func fragments(s string) []string {
	cut := len(s) / 2
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		return []string{s}
	}
	return []string{s[:cut], s[cut:]}
}
