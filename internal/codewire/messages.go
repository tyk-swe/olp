package codewire

import (
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

// Messages observes an Anthropic Messages response. A stream reports input
// usage in message_start and cumulative usage in each message_delta; its
// message_stop, or a message_delta that stops it, ends the generation.
type Messages struct {
	input, cacheRead, cacheWrite, output *int64
	stop                                 string
	terminal                             bool
	finalOutput, invalidUsage            bool
}

func NewMessages() *Messages { return &Messages{} }

func (m *Messages) Event(data []byte) codemode.Observation {
	root, ok := object(data)
	if !ok || m.terminal {
		return codemode.Observation{}
	}
	switch kind, _ := field(root, "type").Text(); kind {
	case "message_start":
		m.record(field(field(root, "message"), "usage"), false)
	case "message_delta":
		m.record(field(root, "usage"), true)
		if stop, ok := field(field(root, "delta"), "stop_reason").Text(); ok && stop != "" {
			m.stop = stop
		}
	case "message_stop":
		return m.finish()
	case "error":
		m.terminal = true
		return failed(anthropicStatus(root))
	}
	return codemode.Observation{}
}

func (m *Messages) Unary(body []byte) codemode.Observation {
	root, ok := object(body)
	if !ok || m.terminal {
		return codemode.Observation{}
	}
	switch kind, _ := field(root, "type").Text(); kind {
	case "message":
		m.record(field(root, "usage"), true)
		m.stop, _ = field(root, "stop_reason").Text()
		return m.finish()
	case "error":
		m.terminal = true
		// The response status already reported the failure.
		return failed(0)
	}
	return codemode.Observation{}
}

// Finish ends a stream that stopped in a message_delta without the
// message_stop that follows it.
func (m *Messages) Finish() (codemode.Observation, bool) {
	if m.terminal || m.stop == "" {
		return codemode.Observation{}, false
	}
	return m.finish(), true
}

func (m *Messages) record(usage oif.Value, final bool) {
	for target, name := range map[**int64]string{&m.input: "input_tokens", &m.cacheRead: "cache_read_input_tokens", &m.cacheWrite: "cache_creation_input_tokens", &m.output: "output_tokens"} {
		value, present := usage.Lookup(name)
		if !present {
			continue
		}
		n := count(value)
		if n == nil {
			m.invalidUsage = true
			continue
		}
		*target = n
		if final && name == "output_tokens" {
			m.finalOutput = true
		}
	}
}

// finish reports usage as the gateway's other dialects do: input includes
// cache reads and writes, and cached input is the cache reads.
func (m *Messages) finish() codemode.Observation {
	m.terminal = true
	kind := "completed"
	if m.stop == "max_tokens" {
		kind = "incomplete"
	}
	var usage codemode.Usage
	if !m.invalidUsage && m.finalOutput && m.input != nil && m.output != nil {
		input := *m.input
		for _, n := range []*int64{m.cacheRead, m.cacheWrite} {
			if n != nil {
				input += *n
			}
		}
		total := input + *m.output
		usage = codemode.Usage{Total: &total, Input: &input, Output: m.output, Cached: m.cacheRead}
		if usage.Validate() != nil {
			usage = codemode.Usage{}
		}
	}
	return codemode.Observation{Terminal: true, Successful: kind == "completed", Usage: usage, Outcome: &codemode.Outcome{Origin: "upstream", Kind: kind, ObservedAt: time.Now().UTC()}}
}

// anthropicStatus is the HTTP status an Anthropic error type stands for, for
// an error the stream reports after its 200 response.
func anthropicStatus(root oif.Value) int {
	kind, _ := field(field(root, "error"), "type").Text()
	return map[string]int{
		"invalid_request_error": 400, "authentication_error": 401, "billing_error": 402, "permission_error": 403, "not_found_error": 404,
		"request_too_large": 413, "rate_limit_error": 429, "api_error": 500, "overloaded_error": 529,
	}[kind]
}

func failed(status int) codemode.Observation {
	outcome := &codemode.Outcome{Origin: "upstream", Kind: "failed", ObservedAt: time.Now().UTC()}
	if status != 0 {
		outcome.UpstreamStatus = &status
	}
	return codemode.Observation{Terminal: true, Status: status, Outcome: outcome}
}

func object(data []byte) (oif.Value, bool) {
	document, err := oif.ParseJSON(data, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return oif.Value{}, false
	}
	return document.Root(), true
}
