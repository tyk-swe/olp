package wire

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

type Chat struct {
	usage    code.Usage
	charge   json.RawMessage
	finished string
	terminal bool
}

func NewChat() *Chat { return &Chat{} }

func (c *Chat) Event(data []byte) code.Observation {
	if c.terminal {
		return code.Observation{}
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return c.finish()
	}
	root, ok := object(data)
	if !ok {
		return code.Observation{}
	}
	if failure := field(root, "error"); failure.Kind() == oif.Object {
		c.terminal = true
		return failedObservation(0, data)
	}
	c.read(root)
	return code.Observation{}
}

func (c *Chat) Unary(body []byte) code.Observation {
	root, ok := object(body)
	if !ok || c.terminal {
		return code.Observation{}
	}
	if failure := field(root, "error"); failure.Kind() == oif.Object {
		c.terminal = true
		return failedObservation(0, body)
	}
	if field(root, "choices").Kind() != oif.Array {
		return code.Observation{}
	}
	c.read(root)
	return c.finish()
}

func (c *Chat) Finish() (code.Observation, bool) {
	if c.terminal || c.finished == "" {
		return code.Observation{}, false
	}
	return c.finish(), true
}

func (c *Chat) read(root oif.Value) {
	for _, choice := range field(root, "choices").Elements() {
		if reason, ok := field(choice, "finish_reason").Text(); ok && reason != "" {
			c.finished = reason
		}
	}
	if usage := field(root, "usage"); usage.Kind() == oif.Object {
		if charge := AllowanceCharge(usage); charge != nil {
			c.charge = charge
		}
		total, present := usage.Lookup("total_tokens")
		u := code.Usage{
			Total:     count(total),
			Input:     count(field(usage, "prompt_tokens")),
			Output:    count(field(usage, "completion_tokens")),
			Cached:    count(field(field(usage, "prompt_tokens_details"), "cached_tokens")),
			Reasoning: count(field(field(usage, "completion_tokens_details"), "reasoning_tokens")),
		}
		if !present && u.Input != nil && u.Output != nil {
			u.Total = new(*u.Input + *u.Output)
		}
		if u.Validate() != nil {
			u = code.Usage{}
		}
		c.usage = u
	}
}

func (c *Chat) finish() code.Observation {
	c.terminal = true
	kind := code.OutcomeCompleted
	if c.finished == "length" {
		kind = code.OutcomeIncomplete
	}
	outcome := code.UpstreamTerminalOutcome(kind, nil, time.Now().UTC())
	return code.Observation{Terminal: true, Successful: kind == code.OutcomeCompleted, Usage: c.usage, AllowanceCharge: c.charge, Outcome: &outcome}
}
