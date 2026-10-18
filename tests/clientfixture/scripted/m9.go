package scripted

import (
	"encoding/json"
	"net/http"
)

func (f *Fixture) legacyCompletions(x *request) {
	var req struct {
		Model, Prompt string
		Stream        bool
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if json.Unmarshal(x.body, &req) != nil || !x.checkOpenAIModel(req.Model) {
		return
	}
	x.note("text", req.Stream)
	id := f.nextID("cmpl")
	row := func(text string, finish any) map[string]any {
		return map[string]any{"id": id, "object": "text_completion", "model": req.Model, "created": createdAt, "choices": []any{map[string]any{"index": 0, "text": text, "finish_reason": finish, "logprobs": nil}}}
	}
	usage := map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10}
	if !req.Stream {
		r := row(DefaultReply, "stop")
		r["usage"] = usage
		writeJSON(x.w, http.StatusOK, r)
		return
	}
	s := newSSE(x.w, "\n\n")
	s.event("", row(DefaultReply, nil))
	s.event("", row("", "stop"))
	if req.StreamOptions.IncludeUsage {
		s.event("", map[string]any{"id": id, "object": "text_completion", "model": req.Model, "choices": []any{}, "usage": usage})
	}
	s.event("", "[DONE]")
}
func (f *Fixture) decisions(x *request) {
	var req struct {
		Model     string
		Input     string
		Questions []struct {
			Type    string
			Name    any
			Choices []struct{ Value any }
			Levels  []struct{ Label string }
		}
	}
	if json.Unmarshal(x.body, &req) != nil || !x.checkOpenAIModel(req.Model) {
		return
	}
	x.note("decision", false)
	answers := []any{}
	for _, q := range req.Questions {
		a := map[string]any{"name": q.Name, "type": q.Type}
		if req.Input == "refuse" {
			a["type"] = "refusal"
			answers = append(answers, a)
			continue
		}
		switch q.Type {
		case "predicate":
			a["probability"] = 0.75
		case "choice":
			p := []any{}
			for i, c := range q.Choices {
				prob := 0.
				if i == 0 {
					prob = 1.
				}
				p = append(p, map[string]any{"value": c.Value, "probability": prob})
			}
			a["choice"] = q.Choices[0].Value
			a["confidence"] = 1.
			a["probabilities"] = p
		case "score":
			p := []any{}
			for i, c := range q.Levels {
				prob := 0.
				if i == 0 {
					prob = 1.
				}
				p = append(p, map[string]any{"value": i, "label": c.Label, "probability": prob})
			}
			a["score"] = 0.
			a["confidence"] = 1.
			a["probabilities"] = p
		}
		answers = append(answers, a)
	}
	writeJSON(x.w, http.StatusOK, map[string]any{"model": req.Model, "answers": answers, "usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10}})
}
func (f *Fixture) systemone(x *request) {
	var req struct {
		Model, State string
		Questions    map[string]struct {
			Type     string
			Criteria json.RawMessage
		}
	}
	if json.Unmarshal(x.body, &req) != nil || !x.checkOpenAIModel(req.Model) {
		return
	}
	x.note("decision", false)
	answers := map[string]any{}
	for name, q := range req.Questions {
		a := map[string]any{"type": q.Type}
		switch q.Type {
		case "noul":
			a["noul"] = 0.75
		case "choice":
			var criteria map[string]any
			json.Unmarshal(q.Criteria, &criteria)
			probabilities := map[string]any{}
			choice := ""
			for k := range criteria {
				probabilities[k] = 0.
				if choice == "" || k < choice {
					choice = k
				}
			}
			probabilities[choice] = 1.
			a["choice"] = choice
			a["confidence"] = 1.
			a["probabilities"] = probabilities
		}
		answers[name] = a
	}
	writeJSON(x.w, http.StatusOK, map[string]any{"model": req.Model, "answers": answers, "usage": map[string]any{"input_tokens": 7, "output_tokens": 3}})
}
