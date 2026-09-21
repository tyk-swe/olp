package protocols

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// SimulationRequest accepts the management API's shared operation document or
// a native request. It uses the serving codecs for semantic feasibility, so a
// dry run cannot approve parameters that execution would refuse.
func SimulationRequest(data []byte, operation, surface, mode, route string) (*openai.Request, error) {
	f, err := object(data)
	if err != nil {
		return nil, err
	}
	canonical := present(f["route"]) || present(f["parameters"])
	family := openai.FamilyChat
	switch operation {
	case "token_count":
		family = openai.FamilyInputTokens
	case "embeddings":
		family = openai.FamilyEmbeddings
	case "moderation":
		family = openai.FamilyModeration
	case "rerank":
		family = openai.FamilyRerank
	}
	if !canonical {
		if surface == "anthropic" {
			family = openai.FamilyAnthropic
			if operation == "token_count" {
				family = openai.FamilyAnthropicCount
			}
		}
		if surface == "gemini" {
			family = openai.FamilyGemini
			if operation == "token_count" {
				family = openai.FamilyGeminiCount
			} else if mode == "streaming" {
				family = openai.FamilyGeminiStream
			}
		}
		return Parse(family, data, route)
	}
	delete(f, "route")
	delete(f, "model")
	if present(f["parameters"]) {
		parameters, err := object(f["parameters"])
		if err != nil {
			return nil, err
		}
		aliases := map[string]string{"max_output_tokens": "max_completion_tokens", "candidate_count": "n", "stop_sequences": "stop"}
		for k, v := range parameters {
			if alias, ok := aliases[k]; ok {
				k = alias
			}
			f[k] = v
		}
	}
	delete(f, "parameters")
	f["model"] = raw(route)
	f["stream"] = raw(mode == "streaming")
	if operation == "generation" || operation == "token_count" {
		if !present(f["messages"]) {
			f["messages"] = raw([]any{map[string]any{"role": "user", "content": "Hello"}})
		}
		if tools := arr(f["tools"]); len(tools) > 0 {
			converted := []Object{}
			for _, v := range tools {
				tool, err := object(v)
				if err != nil {
					return nil, err
				}
				if present(tool["function"]) {
					converted = append(converted, tool)
					continue
				}
				if schema, ok := tool["input_schema"]; ok {
					tool["parameters"] = schema
					delete(tool, "input_schema")
				}
				converted = append(converted, Object{"type": raw("function"), "function": raw(tool)})
			}
			f["tools"] = raw(converted)
		}
		if format, err := optionalObject(f["response_format"]); err != nil {
			return nil, err
		} else if str(format["type"]) == "json_schema" && !present(format["json_schema"]) {
			delete(format, "type")
			f["response_format"] = raw(Object{"type": raw("json_schema"), "json_schema": raw(format)})
		}
		if surface == "anthropic" && operation == "generation" && !present(f["max_completion_tokens"]) && !present(f["max_tokens"]) {
			f["max_completion_tokens"] = raw(4096)
		}
		if operation == "token_count" {
			g, err := decodeCanonical(openai.FamilyChat, f)
			if err != nil {
				return nil, err
			}
			if len(g.Extensions) > 0 {
				return nil, unsupported(g.Extensions[0])
			}
			f, err = encodeCanonical(g, openai.FamilyInputTokens, route, operation)
			if err != nil {
				return nil, err
			}
		}
	}
	if operation == "embeddings" || operation == "moderation" {
		if !present(f["input"]) {
			return nil, requestError("input", "input is required")
		}
	}
	return Parse(family, raw(f), route)
}

// Validate native scalar controls while retaining unmodeled extension fields.
func validateNativeControls(f Object, gemini bool) error {
	prefix := ""
	controls := f
	if gemini {
		var err error
		controls, err = optionalObject(f["generationConfig"])
		if err != nil {
			return requestError("generationConfig", "generationConfig must be an object")
		}
		prefix = "generationConfig."
	}
	integers := map[string]int64{"max_tokens": 4294967295, "top_k": 2147483647}
	ranges := map[string][2]float64{"temperature": {0, 1}, "top_p": {0, 1}}
	if gemini {
		integers = map[string]int64{"maxOutputTokens": 4294967295, "candidateCount": 65535, "topK": 2147483647}
		ranges = map[string][2]float64{"temperature": {0, 2}, "topP": {0, 1}}
	}
	for name, maximum := range integers {
		if present(controls[name]) {
			var n int64
			if json.Unmarshal(controls[name], &n) != nil || n < 1 || n > maximum {
				return requestError(prefix+name, "expected a positive integer within the protocol range")
			}
		}
	}
	for name, bounds := range ranges {
		if present(controls[name]) {
			var n float64
			if json.Unmarshal(controls[name], &n) != nil || n < bounds[0] || n > bounds[1] {
				return requestError(prefix+name, "numeric parameter is outside the protocol range")
			}
		}
	}
	return nil
}
