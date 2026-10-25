package notifications

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	discordContentMax = 2000
	discordTitleMax   = 256
	discordDescMax    = 4096
	summaryMax        = 1024
	pagerDutyDedupMax = 255
)

var mentionSafe = strings.NewReplacer("@", "\uFF20", "<", "\u2039", ">", "\u203A")

func truncate(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max-1]) + "…"
}

func bodyFields(body []byte) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(body, &fields)
	return fields
}

func bodyString(fields map[string]json.RawMessage, key string) string {
	raw, present := fields[key]
	if !present {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func eventTitle(fields map[string]json.RawMessage) string {
	event, rule := bodyString(fields, "event"), bodyString(fields, "rule_name")
	if rule == "" {
		return mentionSafe.Replace(event)
	}
	return mentionSafe.Replace(event + " — " + rule)
}

func evidence(fields map[string]json.RawMessage) string {
	raw, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return "(evidence unavailable)"
	}
	return string(raw)
}

func chatEvidence(fields map[string]json.RawMessage, max int) string {
	return mentionSafe.Replace(truncate(evidence(fields), max))
}

func slackMessage(fields map[string]json.RawMessage) any {
	title := eventTitle(fields)
	return map[string]any{
		"text": truncate(title, 150),
		"blocks": []map[string]any{
			{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(title, 150)}},
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + chatEvidence(fields, 2800) + "```"}},
		},
	}
}

func teamsMessage(fields map[string]json.RawMessage) any {
	title := eventTitle(fields)
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{
			{"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil,
				"content": map[string]any{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard", "version": "1.4",
					"body": []map[string]any{
						{"type": "TextBlock", "text": truncate(title, 256), "weight": "Bolder"},
						{"type": "TextBlock", "text": chatEvidence(fields, 3600), "wrap": true},
					},
				}},
		},
	}
}

func discordMessage(fields map[string]json.RawMessage) any {
	title := eventTitle(fields)
	return map[string]any{
		"content":          truncate(title, discordContentMax),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"embeds": []map[string]any{
			{"title": truncate(title, discordTitleMax), "description": chatEvidence(fields, discordDescMax)},
		},
	}
}

type pagerDutyEvent struct {
	RoutingKey  string `json:"routing_key"`
	EventAction string `json:"event_action"`
	DedupKey    string `json:"dedup_key"`
	Payload     struct {
		Summary       string                     `json:"summary"`
		Source        string                     `json:"source"`
		Severity      string                     `json:"severity"`
		CustomDetails map[string]json.RawMessage `json:"custom_details"`
	} `json:"payload"`
}

func newPagerDutyEvent(routingKey string, body []byte) (pagerDutyEvent, error) {
	fields := bodyFields(body)
	var resolved bool
	_ = json.Unmarshal(fields["resolved"], &resolved)
	dedup := bodyString(fields, "incident_key")
	if dedup == "" {
		dedup = bodyString(fields, "dedup_key")
	}
	if dedup == "" {
		if ruleID, event, window := bodyString(fields, "rule_id"), bodyString(fields, "event"), bodyString(fields, "window_id"); ruleID != "" && event != "" && window != "" {
			dedup = ruleID + ":" + event + ":" + window
		}
	}
	if dedup == "" {
		dedup = bodyString(fields, "delivery_id")
	}
	if len(dedup) > pagerDutyDedupMax {
		return pagerDutyEvent{}, errors.New("deduplication key too long")
	}
	if dedup == "" {
		return pagerDutyEvent{}, errors.New("no deduplication key")
	}
	action := "trigger"
	if resolved {
		action = "resolve"
	}
	payload := pagerDutyEvent{
		RoutingKey:  routingKey,
		EventAction: action,
		DedupKey:    dedup,
	}
	payload.Payload.Summary = truncate(eventTitle(fields), summaryMax)
	payload.Payload.Source = "OpenLLMProxy"
	payload.Payload.Severity = "warning"
	payload.Payload.CustomDetails = fields
	return payload, nil
}
