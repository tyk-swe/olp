package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/egress"
)

const routingKeyLimit = 512

func parsePagerDuty(policy *egress.Policy, d destination) (endpoint[credentialFields], error) {
	if _, err := decodeKnownFields[struct{}](d.Configuration); err != nil {
		return nil, err
	}
	if _, err := policy.ValidateEndpoint(d.URL); err != nil {
		return nil, err
	}
	return pagerDuty{url: d.URL}, nil
}

type pagerDuty struct {
	url        string
	routingKey string
}

func (p pagerDuty) bind(_ *egress.Policy, fields credentialFields) (channel, error) {
	if err := onlyFields(fields, "routing_key"); err != nil {
		return nil, err
	}
	key, err := requiredTextField(fields, "routing_key", routingKeyLimit)
	if err != nil {
		return nil, err
	}
	return pagerDuty{url: p.url, routingKey: key}, nil
}

func (p pagerDuty) send(ctx context.Context, s *Sender, m message) error {
	payload, err := newPagerDutyEvent(p.routingKey, m)
	if err != nil {
		return &sendError{invalidDestinationCode, err}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return &sendError{invalidDestinationCode, err}
	}
	status, answer, err := post(ctx, s.channels, channelTimeout, p.url, nil, encoded)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		if status >= 200 && status < 300 {
			return &sendError{http5xxCode, errors.New("destination acknowledgement invalid")}
		}
		return statusError(status)
	}
	var ack struct {
		Status   string `json:"status"`
		DedupKey string `json:"dedup_key"`
	}
	if err := json.Unmarshal(answer, &ack); err != nil || ack.Status != "success" || ack.DedupKey != payload.DedupKey {
		return &sendError{http5xxCode, errors.New("destination acknowledgement invalid")}
	}
	return nil
}

const (
	summaryMax        = 1024
	pagerDutyDedupMax = 255
)

type pagerDutyEvent struct {
	RoutingKey  string           `json:"routing_key"`
	EventAction string           `json:"event_action"`
	DedupKey    string           `json:"dedup_key"`
	Payload     pagerDutyPayload `json:"payload"`
}

type pagerDutyPayload struct {
	Summary       string                     `json:"summary"`
	Source        string                     `json:"source"`
	Severity      string                     `json:"severity"`
	CustomDetails map[string]json.RawMessage `json:"custom_details"`
}

func newPagerDutyEvent(routingKey string, m message) (pagerDutyEvent, error) {
	if len(m.dedupKey) > pagerDutyDedupMax {
		return pagerDutyEvent{}, errors.New("deduplication key too long")
	}
	if m.dedupKey == "" {
		return pagerDutyEvent{}, errors.New("no deduplication key")
	}
	return pagerDutyEvent{
		RoutingKey: routingKey, EventAction: pagerDutyAction(m.resolved), DedupKey: m.dedupKey,
		Payload: pagerDutyPayload{Summary: truncate(m.title, summaryMax), Source: "OpenLLMProxy", Severity: "warning", CustomDetails: m.fields},
	}, nil
}

func pagerDutyAction(resolved bool) string {
	if resolved {
		return "resolve"
	}
	return "trigger"
}
