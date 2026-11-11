package notifications

import (
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/usage/reports"
)

const incidentKeyField = "incident_key"

type incidentRef struct {
	Key     string `json:"incident_key,omitempty"`
	Subject string `json:"subject,omitempty"`
	Number  *int64 `json:"incident,omitempty"`
}

type signalPayload struct {
	Version    int                        `json:"version"`
	DeliveryID string                     `json:"delivery_id"`
	Event      contract.NotificationEvent `json:"event"`
	RuleID     string                     `json:"rule_id"`
	RuleName   string                     `json:"rule_name"`
	incidentRef
	DedupKey   string         `json:"dedup_key"`
	Resolved   bool           `json:"resolved"`
	OccurredAt string         `json:"occurred_at"`
	Evidence   map[string]any `json:"evidence"`
}

type spendReportPayload struct {
	Version    int                          `json:"version"`
	DeliveryID string                       `json:"delivery_id"`
	Event      contract.NotificationEvent   `json:"event"`
	RuleID     string                       `json:"rule_id"`
	RuleName   string                       `json:"rule_name"`
	DedupKey   string                       `json:"dedup_key"`
	Resolved   bool                         `json:"resolved"`
	Period     string                       `json:"period"`
	Start      string                       `json:"start"`
	End        string                       `json:"end"`
	Summary    reports.Summary              `json:"summary"`
	Breakdowns map[string]reports.Breakdown `json:"breakdowns"`
}

type message struct {
	body     []byte
	fields   map[string]json.RawMessage
	title    string
	dedupKey string
	resolved bool
}

func newMessage(body []byte, event contract.NotificationEvent, ruleName, dedupKey string, resolved bool) (message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return message{}, err
	}
	title := string(event)
	if ruleName != "" {
		title += " — " + ruleName
	}
	return message{body: body, fields: fields, title: mentionSafe.Replace(title), dedupKey: dedupKey, resolved: resolved}, nil
}

type GrantLapse struct {
	CredentialID      string      `json:"credential_version_id"`
	CredentialVersion int         `json:"credential_version"`
	ObservedPrincipal *string     `json:"observed_principal"`
	ProviderID        string      `json:"provider_id"`
	ProviderName      string      `json:"provider_name"`
	LapsedAt          time.Time   `json:"lapsed_at"`
	CredentialSlots   []GrantSlot `json:"credential_slots"`
}

type GrantSlot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
