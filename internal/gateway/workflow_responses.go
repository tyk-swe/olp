package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type responseMCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type responseMCPItem struct {
	Type        string             `json:"type"`
	ID          string             `json:"id"`
	ServerLabel string             `json:"server_label"`
	Tools       *[]responseMCPTool `json:"tools,omitempty"`
	Name        string             `json:"name,omitempty"`
	Arguments   string             `json:"arguments,omitempty"`
	Output      string             `json:"output,omitempty"`
	Status      string             `json:"status,omitempty"`
}

func responseMCPListings(toolsets []string, tools []WorkflowTool) []responseMCPItem {
	return slices.Collect(func(yield func(responseMCPItem) bool) {
		for _, id := range toolsets {
			listed := append([]responseMCPTool{}, slices.Collect(func(yieldTool func(responseMCPTool) bool) {
				for _, tool := range tools {
					if tool.ToolsetID == id && !yieldTool(responseMCPTool{Name: tool.Definition.Alias, Description: tool.Definition.Description, InputSchema: tool.Definition.InputSchema}) {
						return
					}
				}
			})...)
			if !yield(responseMCPItem{Type: "mcp_list_tools", ID: "mcpl_" + uuid.NewString(), ServerLabel: id, Tools: &listed}) {
				return
			}
		}
	})
}

func projectResponseMCPItems(body []byte, items []responseMCPItem, maxBytes int64) ([]byte, error) {
	source, err := oif.ParseJSON(body, oif.Limits{MaxBytes: int(maxBytes)})
	if err != nil {
		return nil, err
	}
	field, ok := source.Root().Lookup("output")
	if !ok {
		return nil, fmt.Errorf("response output is missing")
	}
	var output []json.RawMessage
	if err := json.Unmarshal([]byte(field.Raw()), &output); err != nil {
		return nil, err
	}
	projected, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	var prefix []json.RawMessage
	if err := json.Unmarshal(projected, &prefix); err != nil {
		return nil, err
	}
	combined, err := json.Marshal(slices.Concat(prefix, output))
	if err != nil {
		return nil, err
	}
	result, err := oif.Apply(source, []oif.Change{{Pointer: "/output", Value: string(combined), Origin: oif.ExplicitTransform, Reason: "gateway MCP output"}})
	if err != nil {
		return nil, err
	}
	if int64(len(result.Bytes())) > maxBytes {
		return nil, errResponseTooLarge
	}
	return result.Bytes(), nil
}

type responseMCPStreamEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int             `json:"sequence_number"`
	OutputIndex    int             `json:"output_index"`
	Item           responseMCPItem `json:"item"`
}

func projectResponseMCPFrames(raw []byte, items []responseMCPItem, maxEvent int, maxBytes int64) ([]byte, error) {
	final, err := openai.Aggregate(wire.Responses, bytes.NewReader(raw), maxEvent, int(maxBytes))
	if err != nil {
		return nil, err
	}
	terminal, err := projectResponseMCPItems(final.Body, items, maxBytes)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	sequence, injected := 0, false
	write := func(frame sse.Frame) error {
		encoded := frame.Encode()
		if len(frame.Data) > maxEvent || int64(out.Len()+len(encoded)) > maxBytes {
			return errResponseTooLarge
		}
		_, err := out.Write(encoded)
		return err
	}
	inject := func() error {
		for index, item := range items {
			for _, kind := range []string{"response.output_item.added", "response.output_item.done"} {
				data, err := json.Marshal(responseMCPStreamEvent{Type: kind, SequenceNumber: sequence, OutputIndex: index, Item: item})
				if err != nil {
					return err
				}
				if err := write(sse.Frame{Event: &kind, Data: string(data)}); err != nil {
					return err
				}
				sequence++
			}
		}
		injected = true
		return nil
	}
	err = sse.Decode(bytes.NewReader(raw), maxEvent, func(frame sse.Frame) error {
		if frame.Control {
			return write(frame)
		}
		source, err := oif.ParseJSON([]byte(frame.Data), oif.Limits{MaxBytes: maxEvent})
		if err != nil {
			return err
		}
		kindValue, ok := source.Root().Lookup("type")
		if !ok {
			return fmt.Errorf("response stream event type is missing")
		}
		kind, _ := kindValue.Text()
		if kind != "response.created" && !injected {
			if err := inject(); err != nil {
				return err
			}
		}
		changes := []oif.Change{{Pointer: "/sequence_number", Value: strconv.Itoa(sequence), Origin: oif.ExplicitTransform, Reason: "gateway MCP output"}}
		if index, ok := source.Root().Lookup("output_index"); ok {
			value, err := strconv.Atoi(index.Raw())
			if err != nil {
				return err
			}
			changes = append(changes, oif.Change{Pointer: "/output_index", Value: strconv.Itoa(value + len(items)), Origin: oif.ExplicitTransform, Reason: "gateway MCP output"})
		}
		if kind == "response.completed" || kind == "response.incomplete" {
			changes = append(changes, oif.Change{Pointer: "/response", Value: string(terminal), Origin: oif.ExplicitTransform, Reason: "gateway MCP output"})
		}
		updated, err := oif.Apply(source, changes)
		if err != nil {
			return err
		}
		if err := write(sse.Frame{Event: frame.Event, ID: frame.ID, RetryMS: frame.RetryMS, Data: updated.Raw()}); err != nil {
			return err
		}
		sequence++
		if kind == "response.created" && !injected {
			return inject()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

type advertisedTool struct {
	Type     string             `json:"type"`
	Function advertisedFunction `json:"function"`
}

type advertisedFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type responseAdvertisedTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}
