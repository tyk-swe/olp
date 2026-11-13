package codewire

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

// Chat observes a Chat Completions response. A stream reports usage only in
// a final chunk, when the client asked for it with stream_options; it ends
// with a [DONE] event. Without usage, consumption stays unknown.
type Chat struct {
	usage    codemode.Usage
	charge   json.RawMessage
	finished map[int64]string
	expected int64
	invalid  bool
	terminal bool
}

func NewChat(choices int64) *Chat {
	return &Chat{finished: make(map[int64]string), expected: max(1, choices)}
}

func (c *Chat) Event(data []byte) codemode.Observation {
	if c.terminal {
		return codemode.Observation{}
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return c.finish()
	}
	root, ok := object(data)
	if !ok {
		return codemode.Observation{}
	}
	if failure := field(root, "error"); failure.Kind() == oif.Object {
		// Vendors put their own business codes in errors; never statuses.
		c.terminal = true
		return failed(0, data)
	}
	c.read(root)
	return codemode.Observation{}
}

func (c *Chat) Unary(body []byte) codemode.Observation {
	root, ok := object(body)
	if !ok || c.terminal {
		return codemode.Observation{}
	}
	if failure := field(root, "error"); failure.Kind() == oif.Object {
		c.terminal = true
		return failed(0, body)
	}
	if field(root, "choices").Kind() != oif.Array {
		return codemode.Observation{}
	}
	c.read(root)
	return c.finish()
}

// Finish reports the completion state at EOF.
func (c *Chat) Finish() (codemode.Observation, bool) {
	if c.terminal {
		return codemode.Observation{}, false
	}
	return c.finish(), true
}

func (c *Chat) read(root oif.Value) {
	choices := field(root, "choices").Elements()
	for _, choice := range choices {
		index, valid := func() (int64, bool) {
			value := field(choice, "index")
			if value.Kind() == oif.Absent && c.expected == 1 && len(choices) == 1 {
				return 0, true
			}
			if reported := count(value); reported != nil && *reported < c.expected {
				return *reported, true
			}
			return 0, false
		}()
		if !valid {
			c.invalid = true
			continue
		}
		reason, _ := field(choice, "finish_reason").Text()
		previous, seen := c.finished[index]
		if previous != "" && reason != "" && previous != reason {
			c.invalid = true
		}
		if !seen || reason != "" {
			c.finished[index] = reason
		}
	}
	if usage := field(root, "usage"); usage.Kind() == oif.Object {
		if charge := AllowanceCharge(usage); charge != nil {
			c.charge = charge
		}
		total, present := usage.Lookup("total_tokens")
		u := codemode.Usage{Total: count(total), Input: count(field(usage, "prompt_tokens")), Output: count(field(usage, "completion_tokens")),
			Cached: count(field(field(usage, "prompt_tokens_details"), "cached_tokens")), Reasoning: count(field(field(usage, "completion_tokens_details"), "reasoning_tokens"))}
		if !present && u.Input != nil && u.Output != nil {
			u.Total = new(*u.Input + *u.Output)
		}
		if u.Validate() != nil {
			u = codemode.Usage{}
		}
		c.usage = u
	}
}

func (c *Chat) finish() codemode.Observation {
	c.terminal = true
	kind := "completed"
	if c.invalid || int64(len(c.finished)) != c.expected {
		kind = "incomplete"
	}
	for _, reason := range c.finished {
		if reason == "" || reason == "length" {
			kind = "incomplete"
			break
		}
	}
	return codemode.Observation{Terminal: true, Successful: kind == "completed", Usage: c.usage, AllowanceCharge: c.charge, Outcome: &codemode.Outcome{Origin: "upstream", Kind: kind, ObservedAt: time.Now().UTC()}}
}
