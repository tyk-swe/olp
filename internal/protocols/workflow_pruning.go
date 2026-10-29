package protocols

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type WorkflowExchange struct {
	Calls   []json.RawMessage
	Results []json.RawMessage
	pieces  []workflowPrunePiece
}

type WorkflowPruning struct {
	request   *wire.Request
	field     string
	rows      []Object
	exchanges []WorkflowExchange
}

type workflowPrunePiece struct {
	row   int
	part  int
	field string
	id    string
	kind  string
	raw   json.RawMessage
}

func WorkflowPruningTask(request *wire.Request, limit int) string {
	if request == nil || limit <= 0 {
		return ""
	}
	generation, err := decodeCanonical(request.Family, request.Document())
	if err != nil {
		return ""
	}
	for index := len(generation.Messages) - 1; index >= 0; index-- {
		message := generation.Messages[index]
		if message.Role != "user" {
			continue
		}
		text := strings.Join(slices.Collect(func(yield func(string) bool) {
			for _, part := range message.Parts {
				if part.Text != "" && !yield(part.Text) {
					return
				}
			}
		}), "\n")
		if text == "" {
			continue
		}
		if len(text) > limit {
			return strings.ToValidUTF8(text[:limit], "")
		}
		return text
	}
	return ""
}

func PlanWorkflowPruning(request *wire.Request) (*WorkflowPruning, error) {
	if request == nil {
		return nil, unsupported("workflow transcript")
	}
	field := "messages"
	if request.Family == wire.Responses {
		field = "input"
	} else if request.Family.Surface() == "gemini" {
		field = "contents"
	}
	var rows []Object
	if err := json.Unmarshal(request.Field(field), &rows); err != nil {
		if request.Family == wire.Responses {
			return &WorkflowPruning{request: request, field: field}, nil
		}
		return nil, err
	}
	pieces := slices.Collect(func(yield func(workflowPrunePiece) bool) {
		for row, fields := range rows {
			for _, piece := range workflowPrunePieces(request.Family, row, fields) {
				if !yield(piece) {
					return
				}
			}
		}
	})
	exchanges := slices.Collect(func(yield func(WorkflowExchange) bool) {
		for start := 0; start < len(pieces); {
			if pieces[start].kind != "call" {
				start++
				continue
			}
			callEnd := start
			for callEnd < len(pieces) && pieces[callEnd].kind == "call" {
				callEnd++
			}
			resultEnd := callEnd
			for resultEnd < len(pieces) && pieces[resultEnd].kind == "result" {
				resultEnd++
			}
			calls, results := pieces[start:callEnd], pieces[callEnd:resultEnd]
			if workflowCompleteExchange(calls, results) {
				value := WorkflowExchange{
					Calls: slices.Collect(func(emit func(json.RawMessage) bool) {
						for _, piece := range calls {
							if !emit(piece.raw) {
								return
							}
						}
					}),
					Results: slices.Collect(func(emit func(json.RawMessage) bool) {
						for _, piece := range results {
							if !emit(piece.raw) {
								return
							}
						}
					}),
					pieces: slices.Concat(calls, results),
				}
				if !yield(value) {
					return
				}
			}
			start = resultEnd
		}
	})
	return &WorkflowPruning{request: request, field: field, rows: rows, exchanges: exchanges}, nil
}

func (plan *WorkflowPruning) Candidates() []WorkflowExchange {
	if plan == nil || len(plan.exchanges) <= 1 {
		return nil
	}
	return slices.Clone(plan.exchanges[:len(plan.exchanges)-1])
}

func (plan *WorkflowPruning) Apply(drop []int) (*wire.Request, error) {
	if plan == nil {
		return nil, unsupported("workflow pruning plan")
	}
	if len(drop) == 0 {
		return plan.request, nil
	}
	pieces := slices.Collect(func(yield func(workflowPrunePiece) bool) {
		for _, index := range drop {
			if index < 0 || index >= len(plan.exchanges)-1 {
				continue
			}
			for _, piece := range plan.exchanges[index].pieces {
				if !yield(piece) {
					return
				}
			}
		}
	})
	rows := slices.Collect(func(yield func(Object) bool) {
		for index, row := range plan.rows {
			matches := slices.Collect(func(emit func(workflowPrunePiece) bool) {
				for _, piece := range pieces {
					if piece.row == index && !emit(piece) {
						return
					}
				}
			})
			if len(matches) == 0 {
				if !yield(row) {
					return
				}
				continue
			}
			if matches[0].field == "" {
				continue
			}
			field := matches[0].field
			parts := arr(row[field])
			remaining := slices.Collect(func(emit func(json.RawMessage) bool) {
				for part, value := range parts {
					if slices.ContainsFunc(matches, func(piece workflowPrunePiece) bool { return piece.part == part }) {
						continue
					}
					if !emit(value) {
						return
					}
				}
			})
			if len(remaining) == 0 && (field != "tool_calls" || !workflowHasContent(row)) {
				continue
			}
			next := maps.Collect(func(emit func(string, json.RawMessage) bool) {
				for key, value := range row {
					if key == field {
						if len(remaining) == 0 {
							continue
						}
						value = raw(remaining)
					}
					if !emit(key, value) {
						return
					}
				}
			})
			if !yield(next) {
				return
			}
		}
	})
	return plan.request.WithField(plan.field, raw(rows))
}

func workflowHasContent(row Object) bool {
	content := row["content"]
	return len(content) > 0 && string(content) != "null" && string(content) != `""` && string(content) != "[]"
}

func workflowCompleteExchange(calls, results []workflowPrunePiece) bool {
	if len(calls) == 0 || len(calls) != len(results) {
		return false
	}
	for _, call := range calls {
		if call.id == "" || slices.ContainsFunc(calls, func(other workflowPrunePiece) bool {
			return other.id == call.id && (other.row != call.row || other.part != call.part)
		}) || !slices.ContainsFunc(results, func(result workflowPrunePiece) bool { return result.id == call.id }) {
			return false
		}
	}
	return true
}

func workflowPrunePieces(family wire.Format, row int, fields Object) []workflowPrunePiece {
	if family == wire.Responses {
		switch str(fields["type"]) {
		case "function_call":
			return []workflowPrunePiece{{row: row, id: str(fields["call_id"]), kind: "call", raw: raw(fields)}}
		case "function_call_output":
			return []workflowPrunePiece{{row: row, id: str(fields["call_id"]), kind: "result", raw: raw(fields)}}
		default:
			return []workflowPrunePiece{{row: row, kind: "barrier"}}
		}
	}
	role := str(fields["role"])
	if family.Surface() == "openai" {
		if role == "tool" {
			return []workflowPrunePiece{{row: row, id: str(fields["tool_call_id"]), kind: "result", raw: raw(fields)}}
		}
		if role != "assistant" || len(arr(fields["tool_calls"])) == 0 {
			return []workflowPrunePiece{{row: row, kind: "barrier"}}
		}
		return slices.Collect(func(yield func(workflowPrunePiece) bool) {
			for part, value := range arr(fields["tool_calls"]) {
				call, err := object(value)
				if err != nil {
					return
				}
				if !yield(workflowPrunePiece{row: row, part: part, field: "tool_calls", id: str(call["id"]), kind: "call", raw: value}) {
					return
				}
			}
		})
	}
	field := "content"
	if family.Surface() == "gemini" {
		field = "parts"
	}
	parts := arr(fields[field])
	pieces := slices.Collect(func(yield func(workflowPrunePiece) bool) {
		for part, value := range parts {
			item, err := object(value)
			if err != nil {
				continue
			}
			kind, id := "", ""
			if family.Surface() == "anthropic" {
				if role == "assistant" && str(item["type"]) == "tool_use" {
					kind, id = "call", str(item["id"])
				}
				if role == "user" && str(item["type"]) == "tool_result" {
					kind, id = "result", str(item["tool_use_id"])
				}
			} else {
				if role == "model" && present(item["functionCall"]) {
					kind, id = "call", workflowGeminiID(item["functionCall"])
				}
				if role == "user" && present(item["functionResponse"]) {
					kind, id = "result", workflowGeminiID(item["functionResponse"])
				}
			}
			if kind != "" && !yield(workflowPrunePiece{row: row, part: part, field: field, id: id, kind: kind, raw: value}) {
				return
			}
		}
	})
	if len(pieces) == 0 {
		return []workflowPrunePiece{{row: row, kind: "barrier"}}
	}
	return pieces
}

func workflowGeminiID(rawValue json.RawMessage) string {
	value, err := object(rawValue)
	if err != nil {
		return ""
	}
	if id := str(value["id"]); id != "" {
		return id
	}
	return str(value["name"])
}

func WorkflowExchangeName(index int) string { return "e" + strconv.Itoa(index) }
