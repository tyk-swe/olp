package estimate

import "math/bits"

type category uint8

const (
	unclassified category = iota
	instructions
	conversation
	toolSchemas
	toolCallsResults
	media
	categoryCount
)

func itemCategory(item object) category {
	switch role, _ := roleOf(item.get("role")); role {
	case "system", "developer":
		return instructions
	case "tool", "function":
		return toolCallsResults
	}
	if kind, _, _ := item.get("type").text(); kind == "function_call" || kind == "function_call_output" {
		return toolCallsResults
	}
	return conversation
}

func isNativeToolBlock(block object) bool {
	kind := block.get("type").source()
	return kind == `"tool_use"` || kind == `"tool_result"`
}

func dialectCategory(fallback category, block object) category {
	if isNativeToolBlock(block) {
		return toolCallsResults
	}
	switch role, _ := roleOf(block.get("role")); role {
	case "system", "developer":
		return instructions
	case "tool", "function":
		return toolCallsResults
	}
	return fallback
}

type Breakdown struct {
	Instructions     int64 `json:"instructions"`
	Conversation     int64 `json:"conversation"`
	ToolSchemas      int64 `json:"tool_schemas"`
	ToolCallsResults int64 `json:"tool_calls_results"`
	Media            int64 `json:"media"`
	Unclassified     int64 `json:"unclassified"`
}

func (b Breakdown) withRemainder(tokens int64) *Breakdown {
	b.Unclassified = tokens - b.Instructions - b.Conversation - b.ToolSchemas - b.ToolCallsResults - b.Media
	return &b
}

type categoryCounts [categoryCount]int64

type inputBreakdown struct {
	heuristic categoryCounts
	overflow  categoryCounts
	flat      categoryCounts
}

func (counts categoryCounts) breakdown(total int64) *Breakdown {
	var assigned categoryCounts
	remaining := total
	for part := instructions; part < categoryCount; part++ {
		assigned[part] = min(counts[part], remaining)
		remaining -= assigned[part]
	}
	return &Breakdown{
		Instructions: assigned[instructions], Conversation: assigned[conversation],
		ToolSchemas: assigned[toolSchemas], ToolCallsResults: assigned[toolCallsResults],
		Media: assigned[media], Unclassified: remaining,
	}
}

func distribute(total, weight int64, weights categoryCounts) categoryCounts {
	var counts categoryCounts
	if weight == 0 {
		return counts
	}
	var cumulative int64
	var previous uint64
	for part, value := range weights {
		cumulative = min(addBounded(cumulative, value), weight)
		high, low := bits.Mul64(uint64(cumulative), uint64(total))
		tokens, _ := bits.Div64(high, low, uint64(weight))
		counts[part] = int64(tokens - previous)
		previous = tokens
	}
	return counts
}

func (counts categoryCounts) plus(other categoryCounts) categoryCounts {
	var sum categoryCounts
	for part, value := range counts {
		sum[part] = addBounded(value, other[part])
	}
	return sum
}

type meteredBreakdown struct {
	exact categoryCounts
	tail  categoryCounts
}

func (b *meteredBreakdown) add(m *Meter, s segment, part category) {
	beforeTokens, beforeTail := m.tokens, m.tailBytes
	m.add(s.text, s.rest, s.heuristic)
	b.exact[part] += m.tokens - beforeTokens
	if s.text != "" {
		b.tail[part] += m.tailBytes - beforeTail
	}
}

func (b meteredBreakdown) finish(m Meter, in inputBreakdown, heuristic, tokens, total int64) *Breakdown {
	if m.encoding == nil {
		return in.flat.plus(distribute(tokens, heuristic, in.heuristic)).breakdown(total)
	}
	tail := distribute(tokens-m.tokens, m.tailBytes, b.tail.plus(in.overflow))
	return b.exact.plus(tail).plus(in.flat).breakdown(total)
}
