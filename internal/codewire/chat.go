package codewire

import (
	"bytes"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

// Chat observes a Chat Completions response. A stream reports usage only in
// a final chunk, when the client asked for it with stream_options; it ends
// with a [DONE] event. Without usage, consumption stays unknown.
type Chat struct {
	usage    codemode.Usage
	finished string
	terminal bool
}

func NewChat() *Chat { return &Chat{} }

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
		return failed(0)
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
		return failed(0)
	}
	if field(root, "choices").Kind() != oif.Array {
		return codemode.Observation{}
	}
	c.read(root)
	return c.finish()
}

// Finish ends a stream whose final chunk arrived without [DONE].
func (c *Chat) Finish() (codemode.Observation, bool) {
	if c.terminal || c.finished == "" {
		return codemode.Observation{}, false
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
		u := codemode.Usage{Total: count(field(usage, "total_tokens")), Input: count(field(usage, "prompt_tokens")), Output: count(field(usage, "completion_tokens")),
			Cached: count(field(field(usage, "prompt_tokens_details"), "cached_tokens")), Reasoning: count(field(field(usage, "completion_tokens_details"), "reasoning_tokens"))}
		if u.Total == nil && u.Input != nil && u.Output != nil {
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
	if c.finished == "length" {
		kind = "incomplete"
	}
	return codemode.Observation{Terminal: true, Successful: kind == "completed", Usage: c.usage, Outcome: &codemode.Outcome{Origin: "upstream", Kind: kind, ObservedAt: time.Now().UTC()}}
}
