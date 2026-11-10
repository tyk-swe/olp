package wire

import (
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

type Messages struct {
	input, cacheRead, cacheWrite, output *int64
	charge                               json.RawMessage
	stop                                 string
	terminal                             bool
	finalOutput, invalidUsage            bool
}

func NewMessages() *Messages { return &Messages{} }

func (m *Messages) Event(data []byte) code.Observation {
	root, ok := object(data)
	if !ok || m.terminal {
		return code.Observation{}
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
		return failedObservation(anthropicStatus(root), data)
	}
	return code.Observation{}
}

func (m *Messages) Unary(body []byte) code.Observation {
	root, ok := object(body)
	if !ok || m.terminal {
		return code.Observation{}
	}
	switch kind, _ := field(root, "type").Text(); kind {
	case "message":
		m.record(field(root, "usage"), true)
		m.stop, _ = field(root, "stop_reason").Text()
		return m.finish()
	case "":
		if failure := field(root, "error"); failure.Kind() == oif.Object {
			m.terminal = true
			return failedObservation(0, body)
		}
		return code.Observation{}
	case "error":
		m.terminal = true
		return failedObservation(0, body)
	}
	return code.Observation{}
}

func (m *Messages) Finish() (code.Observation, bool) {
	if m.terminal || m.stop == "" {
		return code.Observation{}, false
	}
	return m.finish(), true
}

func (m *Messages) record(usage oif.Value, final bool) {
	if charge := AllowanceCharge(usage); charge != nil {
		m.charge = charge
	}
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

func (m *Messages) finish() code.Observation {
	m.terminal = true
	kind := code.OutcomeCompleted
	if m.stop == "max_tokens" {
		kind = code.OutcomeIncomplete
	}
	var usage code.Usage
	if !m.invalidUsage && m.finalOutput && m.input != nil && m.output != nil {
		input := *m.input
		for _, n := range []*int64{m.cacheRead, m.cacheWrite} {
			if n != nil {
				input += *n
			}
		}
		total := input + *m.output
		usage = code.Usage{Total: &total, Input: &input, Output: m.output, Cached: m.cacheRead}
		if usage.Validate() != nil {
			usage = code.Usage{}
		}
	}
	outcome := code.UpstreamTerminalOutcome(kind, nil, time.Now().UTC())
	return code.Observation{Terminal: true, Successful: kind == code.OutcomeCompleted, Usage: usage, AllowanceCharge: m.charge, Outcome: &outcome}
}

func anthropicStatus(root oif.Value) int {
	kind, _ := field(field(root, "error"), "type").Text()
	return map[string]int{
		"invalid_request_error": 400, "authentication_error": 401, "billing_error": 402, "permission_error": 403, "not_found_error": 404,
		"request_too_large": 413, "rate_limit_error": 429, "api_error": 500, "overloaded_error": 529,
	}[kind]
}
