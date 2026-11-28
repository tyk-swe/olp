package sinks

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/domain/textfield"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

func NormalizeSpec(spec Spec, policy *egress.Policy) (Spec, error) {
	if spec.Type != "https" {
		return Spec{}, problem.Invalid("type", "Use the HTTPS JSON sink type.")
	}
	in, err := normalize(input{Name: spec.Name, Destination: spec.Destination, Streams: spec.Streams, Enabled: &spec.Enabled}, policy)
	if err != nil {
		return Spec{}, err
	}
	if err := checkScope(in.Streams, spec.ProjectID); err != nil {
		return Spec{}, err
	}
	return Spec{ProjectID: spec.ProjectID, Name: in.Name, Type: spec.Type, Destination: in.Destination, Streams: in.Streams, Enabled: spec.Enabled}, nil
}

func normalize(in input, policy *egress.Policy) (input, error) {
	name := strings.TrimSpace(in.Name)
	if err := textfield.Validate("name", name, 100); err != nil {
		return input{}, err
	}
	if in.Enabled == nil {
		return input{}, problem.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return input{}, problem.Fail(503, "egress_policy_unavailable", "Export delivery requires provider egress policy.")
	}
	target, err := policy.ValidateEndpoint(in.Destination)
	if err != nil {
		return input{}, problem.Invalid("destination", "The endpoint is not permitted by egress policy.")
	}
	if len(in.Streams) < 1 || len(in.Streams) > 4 {
		return input{}, problem.Invalid("streams", "Choose supported content-free fact streams.")
	}
	for index, stream := range in.Streams {
		if !slices.Contains([]string{"requests", "attempts", "guardrail_decisions", "audit", "runs", "trace_spans"}, stream) || slices.Contains(in.Streams[:index], stream) {
			return input{}, unusableStreams()
		}
	}
	return input{ProjectID: in.ProjectID, Name: name, Type: in.Type, Destination: target.String(), Streams: in.Streams, Enabled: in.Enabled, Credential: in.Credential}, nil
}

func checkScope(streams []string, project *string) error {
	if project != nil && slices.Contains(streams, "audit") {
		return unusableStreams()
	}
	return nil
}

func unusableStreams() error {
	return problem.Invalid("streams", "Use unique streams; audit export requires an installation sink.")
}

func signingCredential(raw json.RawMessage) (*string, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) < 1 || len(value) > 4096 {
		return nil, problem.Invalid("credential", "Use a signing credential of 1–4096 bytes or null.")
	}
	return &value, nil
}
