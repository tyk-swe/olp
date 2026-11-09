package definition

import (
	"bytes"
	"cmp"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Draft struct {
	project       string
	name          string
	kind          guardrails.Kind
	configuration json.RawMessage
	timeout       int64
	onError       guardrails.OnError
	legacy        *contentpolicy.Policy
}

func NewDraft(project, name string, kind guardrails.Kind, configuration json.RawMessage, timeout int64, onError guardrails.OnError) (Draft, error) {
	d := Draft{project: project, name: name, kind: kind, configuration: configuration, timeout: cmp.Or(timeout, 1000), onError: cmp.Or(onError, guardrails.FailClosed)}
	if _, err := guardrails.ParseRevision(guardrails.Revision{Type: d.kind, Configuration: d.configuration, TimeoutMS: d.timeout, OnError: d.onError}); err != nil {
		return Draft{}, access.Invalid("configuration", "Supply valid typed guardrail configuration, timeout and failure policy.")
	}
	return d, nil
}

func legacyDraft(project, name string, kind guardrails.Kind, policy *contentpolicy.Policy, timeout int64, onError guardrails.OnError) (Draft, error) {
	if policy == nil || contentpolicy.Validate(policy) != nil {
		return Draft{}, access.Invalid("policy", "Supply a valid bounded content policy.")
	}
	rules := policy.Rules
	if rules == nil {
		rules = []contentpolicy.Rule{}
	}
	patterns := make([]map[string]any, len(rules))
	for i, r := range rules {
		patterns[i] = map[string]any{"id": r.ID, "pattern": r.Pattern, "replacement": r.Replacement}
	}
	configuration, err := json.Marshal(map[string]any{"rules": patterns})
	if err != nil {
		return Draft{}, err
	}
	d, err := NewDraft(project, name, kind, configuration, timeout, onError)
	if err != nil {
		return Draft{}, err
	}
	d.legacy = &contentpolicy.Policy{Rules: rules}
	return d, nil
}

type input struct {
	Configuration json.RawMessage    `json:"configuration,omitempty"`
	TimeoutMS     int64              `json:"timeout_ms,omitempty"`
	OnError       guardrails.OnError `json:"on_error,omitempty"`
	Name          string             `json:"name"`
	Type          guardrails.Kind    `json:"type,omitempty"`
	ProjectID     *string            `json:"project_id,omitempty"`
	Policy        json.RawMessage    `json:"policy"`
}

type edit struct {
	name          string
	configuration json.RawMessage
	legacy        *contentpolicy.Policy
	timeout       int64
	onError       guardrails.OnError
}

func parseEdit(in input) (edit, error) {
	name := strings.TrimSpace(in.Name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return edit{}, access.Invalid("name", "Choose a name of 1–100 characters without control characters.")
	}
	e := edit{name: name, configuration: in.Configuration, timeout: in.TimeoutMS, onError: in.OnError}
	if len(in.Configuration) > 0 {
		if len(in.Policy) > 0 {
			return edit{}, access.Invalid("configuration", "Do not mix legacy policy and typed configuration.")
		}
		return e, nil
	}
	raw := bytes.TrimSpace(in.Policy)
	if len(raw) == 0 || raw[0] != '{' {
		return edit{}, access.Invalid("policy", "Supply a bounded content-policy object.")
	}
	policy, err := contentpolicy.Decode(in.Policy)
	if err != nil {
		return edit{}, access.Invalid("policy", "Supply valid bounded RE2 block or redact rules.")
	}
	e.legacy = policy
	return e, nil
}

func (e edit) draft(project string, kind guardrails.Kind) (Draft, error) {
	if e.legacy != nil {
		return legacyDraft(project, e.name, kind, e.legacy, e.timeout, e.onError)
	}
	return NewDraft(project, e.name, kind, e.configuration, e.timeout, e.onError)
}

func acknowledgesEgress(configuration json.RawMessage) bool {
	var acknowledged struct {
		ContentEgress bool `json:"content_egress"`
	}
	return len(configuration) > 0 && json.Unmarshal(configuration, &acknowledged) == nil && acknowledged.ContentEgress
}
