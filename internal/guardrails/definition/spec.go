package definition

import (
	"cmp"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
)

type Draft struct {
	project       string
	name          string
	kind          guardrails.Kind
	configuration json.RawMessage
	timeout       int64
	onError       guardrailpolicy.OnError
}

func NewDraft(project, name string, kind guardrails.Kind, configuration json.RawMessage, timeout int64, onError guardrailpolicy.OnError) (Draft, error) {
	d := Draft{project: project, name: name, kind: kind, configuration: configuration, timeout: cmp.Or(timeout, 1000), onError: cmp.Or(onError, guardrailpolicy.FailClosed)}
	if _, err := guardrails.ParseRevision(guardrails.Revision{Type: d.kind, Configuration: d.configuration, TimeoutMS: d.timeout, OnError: d.onError}); err != nil {
		return Draft{}, httpkit.Invalid("configuration", "Supply valid typed guardrail configuration, timeout and failure policy.")
	}
	return d, nil
}

type input struct {
	Configuration json.RawMessage         `json:"configuration,omitempty"`
	TimeoutMS     int64                   `json:"timeout_ms,omitempty"`
	OnError       guardrailpolicy.OnError `json:"on_error,omitempty"`
	Name          string                  `json:"name"`
	Type          guardrails.Kind         `json:"type,omitempty"`
	ProjectID     *string                 `json:"project_id,omitempty"`
}

type edit struct {
	name          string
	configuration json.RawMessage
	timeout       int64
	onError       guardrailpolicy.OnError
}

func parseEdit(in input) (edit, error) {
	name := strings.TrimSpace(in.Name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 100 || strings.ContainsFunc(name, unicode.IsControl) {
		return edit{}, httpkit.Invalid("name", "Choose a name of 1–100 characters without control characters.")
	}
	return edit{name: name, configuration: in.Configuration, timeout: in.TimeoutMS, onError: in.OnError}, nil
}

func (e edit) draft(project string, kind guardrails.Kind) (Draft, error) {
	return NewDraft(project, e.name, kind, e.configuration, e.timeout, e.onError)
}

func acknowledgesEgress(configuration json.RawMessage) bool {
	var acknowledged struct {
		ContentEgress bool `json:"content_egress"`
	}
	return len(configuration) > 0 && json.Unmarshal(configuration, &acknowledged) == nil && acknowledged.ContentEgress
}
