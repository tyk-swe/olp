package protocols

import (
	"fmt"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

type generationAdapter struct{ family openai.Family }

func field(v oif.Value, name string) oif.Value  { out, _ := v.Lookup(name); return out }
func textField(v oif.Value, name string) string { s, _ := field(v, name).Text(); return s }

func (a generationAdapter) LiftRequest(r oif.Request) (oif.View, error) {
	root := r.Document().Root()
	view := generation.Request{}
	if root.Kind() != oif.Object {
		return nil, fmt.Errorf("generation source must be an object")
	}
	add := func(v oif.Value, path, role, scope string) {
		if v.Kind() == oif.Absent {
			return
		}
		m := generation.Message{ID: path, Pointer: path, Role: role, Scope: scope, Source: v}
		if v.Kind() == oif.String || v.Kind() == oif.Array {
			m.Nodes = a.nodes(v, path)
		} else {
			if native := textField(v, "role"); native != "" {
				m.Role = native
			}
			contentName := "content"
			if a.family == openai.FamilyGemini {
				contentName = "parts"
			}
			m.Nodes = a.nodes(field(v, contentName), oif.Pointer(path, contentName))
			if calls := field(v, "tool_calls"); calls.Kind() != oif.Absent {
				m.Nodes = append(m.Nodes, a.nodes(calls, oif.Pointer(path, "tool_calls"))...)
			}
			if m.Role == "tool" && textField(v, "tool_call_id") != "" {
				callID := textField(v, "tool_call_id")
				m.Nodes = []generation.Node{{ID: path, Pointer: path, Kind: "tool_result", Source: v, CallID: callID, Dependencies: []generation.Dependency{{Kind: "tool_call", ID: callID}}, Children: m.Nodes}}
			}
			if len(m.Nodes) == 0 {
				m.Nodes = a.nodes(v, path)
			}
		}
		view.Messages = append(view.Messages, m)
	}
	ignored := map[string]bool{"model": true, "stream": true, "stream_options": true, "tools": true}
	switch a.family {
	case openai.FamilyChat:
		ignored["messages"] = true
		for i, v := range field(root, "messages").Elements() {
			add(v, fmt.Sprintf("/messages/%d", i), textField(v, "role"), "message")
		}
	case openai.FamilyResponses:
		ignored["input"] = true
		ignored["instructions"] = true
		add(field(root, "instructions"), "/instructions", "system", "responses.instructions")
		input := field(root, "input")
		if input.Kind() == oif.String {
			add(input, "/input", "user", "message")
		} else {
			for i, v := range input.Elements() {
				role := textField(v, "role")
				switch textField(v, "type") {
				case "function_call":
					role = "assistant"
				case "function_call_output":
					role = "tool"
				}
				add(v, fmt.Sprintf("/input/%d", i), role, "responses.item")
			}
		}
	case openai.FamilyAnthropic, openai.FamilyBedrock:
		ignored["messages"] = true
		ignored["system"] = true
		add(field(root, "system"), "/system", "system", string(a.family)+".system")
		for i, v := range field(root, "messages").Elements() {
			add(v, fmt.Sprintf("/messages/%d", i), textField(v, "role"), "message")
		}
	case openai.FamilyGemini:
		ignored["contents"] = true
		ignored["systemInstruction"] = true
		add(field(root, "systemInstruction"), "/systemInstruction", "system", "gemini.systemInstruction")
		for i, v := range field(root, "contents").Elements() {
			add(v, fmt.Sprintf("/contents/%d", i), textField(v, "role"), "message")
		}
	default:
		return nil, fmt.Errorf("unregistered generation dialect")
	}
	controls := []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "reasoning", "reasoning_effort", "thinking", "response_format", "generationConfig", "output_config", "inferenceConfig", "tool_choice", "parallel_tool_calls", "stop", "stop_sequences", "n"}
	seen := map[string]bool{}
	for _, name := range controls {
		seen[name] = true
		view.Controls = append(view.Controls, a.control(r, name, field(root, name)))
	}
	for _, m := range root.Members() {
		if !ignored[m.Name] && !seen[m.Name] {
			view.Controls = append(view.Controls, a.control(r, m.Name, m.Value))
		}
	}
	addTool := func(t, tool oif.Value, path string) {
		schema := field(tool, "parameters")
		if v := field(tool, "input_schema"); v.Kind() != oif.Absent {
			schema = v
		}
		if v := field(tool, "parametersJsonSchema"); v.Kind() != oif.Absent {
			schema = v
		}
		if v := field(field(tool, "inputSchema"), "json"); v.Kind() != oif.Absent {
			schema = v
		}
		view.Tools = append(view.Tools, generation.Tool{ID: path, Pointer: path, Source: t, Name: textField(tool, "name"), Description: textField(tool, "description"), SchemaValue: schema, Strictness: field(tool, "strict"), SchemaDialect: textField(schema, "$schema")})
	}
	for i, t := range field(root, "tools").Elements() {
		path := fmt.Sprintf("/tools/%d", i)
		if declarations := field(t, "functionDeclarations"); declarations.Kind() == oif.Array {
			for j, tool := range declarations.Elements() {
				addTool(tool, tool, fmt.Sprintf("%s/functionDeclarations/%d", path, j))
			}
			continue
		}
		tool := t
		if f := field(t, "function"); f.Kind() == oif.Object {
			tool = f
			path += "/function"
		}
		addTool(t, tool, path)
	}
	if a.family == openai.FamilyBedrock {
		for i, t := range field(field(root, "toolConfig"), "tools").Elements() {
			if spec := field(t, "toolSpec"); spec.Kind() == oif.Object {
				addTool(t, spec, fmt.Sprintf("/toolConfig/tools/%d/toolSpec", i))
			}
		}
	}
	return view, nil
}
func (a generationAdapter) control(r oif.Request, name string, value oif.Value) generation.Control {
	origin := oif.Caller
	path := oif.Pointer("", name)
	for _, p := range r.Provenance() {
		if p.Pointer == path {
			origin = p.Origin
		}
	}
	c := generation.Control{Name: name, Pointer: path, Dialect: r.Descriptor().Dialect, Value: value, Presence: value.Presence(), Origin: origin}
	switch name {
	case "max_tokens", "max_completion_tokens", "max_output_tokens":
		c.Units = "tokens"
		c.BudgetScope = r.Descriptor().Dialect.ID + "/" + name
	case "thinking", "reasoning", "reasoning_effort":
		c.BudgetScope = r.Descriptor().Dialect.ID + "/" + name
	}
	return c
}
func (a generationAdapter) nodes(v oif.Value, path string) []generation.Node {
	if v.Kind() == oif.Absent || v.Kind() == oif.Null {
		return nil
	}
	if v.Kind() == oif.Array {
		out := []generation.Node{}
		for i, child := range v.Elements() {
			out = append(out, a.nodes(child, fmt.Sprintf("%s/%d", path, i))...)
		}
		return out
	}
	n := generation.Node{ID: path, Pointer: path, Source: v, Kind: "native"}
	if v.Kind() == oif.String {
		n.Kind = "text"
		n.Text, _ = v.Text()
		return []generation.Node{n}
	}
	if v.Kind() != oif.Object {
		return []generation.Node{n}
	}
	kind := textField(v, "type")
	if kind != "" {
		n.Kind = kind
	}
	if id := textField(v, "id"); id != "" {
		n.ID = id
	}
	switch kind {
	case "text", "input_text", "output_text":
		n.Kind = "text"
		n.Text = textField(v, "text")
	case "tool_use":
		n.Kind = "tool_call"
		n.CallID = textField(v, "id")
		n.Name = textField(v, "name")
		n.Arguments = field(v, "input")
	case "function":
		n.Kind = "tool_call"
		n.CallID = textField(v, "id")
		f := field(v, "function")
		n.Name = textField(f, "name")
		n.Arguments = field(f, "arguments")
	case "function_call":
		n.Kind = "tool_call"
		n.CallID = textField(v, "call_id")
		n.Name = textField(v, "name")
		n.Arguments = field(v, "arguments")
	case "tool_result":
		n.Kind = "tool_result"
		n.CallID = textField(v, "tool_use_id")
	case "function_call_output":
		n.Kind = "tool_result"
		n.CallID = textField(v, "call_id")
	case "message":
		n.Children = a.nodes(field(v, "content"), oif.Pointer(path, "content"))
	case "thinking", "redacted_thinking", "reasoning": // Keep the entire native dependency, including opaque fields.
	default:
		if f := field(v, "functionCall"); f.Kind() != oif.Absent {
			n.Kind = "tool_call"
			n.CallID = textField(f, "id")
			n.Name = textField(f, "name")
			n.Arguments = field(f, "args")
		} else if f := field(v, "functionResponse"); f.Kind() != oif.Absent {
			n.Kind = "tool_result"
			n.CallID = textField(f, "id")
			n.Name = textField(f, "name")
		} else if f := field(v, "toolUse"); f.Kind() != oif.Absent {
			n.Kind = "tool_call"
			n.CallID = textField(f, "toolUseId")
			n.Name = textField(f, "name")
			n.Arguments = field(f, "input")
		} else if f := field(v, "toolResult"); f.Kind() != oif.Absent {
			n.Kind = "tool_result"
			n.CallID = textField(f, "toolUseId")
		} else if text := field(v, "text"); text.Kind() == oif.String {
			n.Kind = "text"
			n.Text, _ = text.Text()
		}
	}
	if n.Kind == "tool_result" && n.CallID != "" {
		n.Dependencies = []generation.Dependency{{Kind: "tool_call", ID: n.CallID}}
	}
	return []generation.Node{n}
}
func (a generationAdapter) LiftResult(r oif.Result) (oif.View, error) {
	root := r.Source().Root()
	result := generation.Result{Usage: field(root, "usage")}
	add := func(v oif.Value, path string, index int, content oif.Value, contentPath, finish string) {
		id := textField(v, "id")
		if id == "" {
			id = path
		}
		result.Candidates = append(result.Candidates, generation.Candidate{ID: id, Pointer: path, Index: index, Source: v, Nodes: a.nodes(content, contentPath), FinishReason: finish})
	}
	switch a.family {
	case openai.FamilyChat:
		for i, c := range field(root, "choices").Elements() {
			path := fmt.Sprintf("/choices/%d", i)
			index := i
			if n, err := strconv.Atoi(field(c, "index").Raw()); err == nil {
				index = n
			}
			m := field(c, "message")
			add(c, path, index, field(m, "content"), path+"/message/content", textField(c, "finish_reason"))
			last := &result.Candidates[len(result.Candidates)-1]
			last.Nodes = append(last.Nodes, a.nodes(field(m, "tool_calls"), path+"/message/tool_calls")...)
			if refusal := field(m, "refusal"); refusal.Kind() != oif.Absent && refusal.Kind() != oif.Null {
				last.Nodes = append(last.Nodes, generation.Node{ID: path + "/message/refusal", Pointer: path + "/message/refusal", Kind: "refusal", Source: refusal})
			}
		}
	case openai.FamilyResponses:
		add(root, "", 0, field(root, "output"), "/output", textField(root, "status"))
	case openai.FamilyAnthropic:
		add(root, "", 0, field(root, "content"), "/content", textField(root, "stop_reason"))
	case openai.FamilyGemini:
		result.Usage = field(root, "usageMetadata")
		for i, c := range field(root, "candidates").Elements() {
			path := fmt.Sprintf("/candidates/%d", i)
			index := i
			if n, err := strconv.Atoi(field(c, "index").Raw()); err == nil {
				index = n
			}
			add(c, path, index, field(field(c, "content"), "parts"), path+"/content/parts", textField(c, "finishReason"))
		}
	case openai.FamilyBedrock:
		add(root, "", 0, field(field(field(root, "output"), "message"), "content"), "/output/message/content", textField(root, "stopReason"))
	default:
		return nil, fmt.Errorf("generation result view unavailable")
	}
	return result, nil
}
func (a generationAdapter) LiftEvent(e oif.Event) (oif.View, error) {
	return generation.Event{Name: e.Name(), Sequence: e.Sequence(), Source: e.Source().Root()}, nil
}
