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

func (m message) details() string {
	raw, err := json.MarshalIndent(m.fields, "", "  ")
	if err != nil {
		return "(evidence unavailable)"
	}
	return string(raw)
}

func chatEvidence(m message, max int) string {
	return mentionSafe.Replace(truncate(m.details(), max))
}

func slackMessage(m message) any {
	return map[string]any{
		"text": truncate(m.title, 150),
		"blocks": []map[string]any{
			{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(m.title, 150)}},
			{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + chatEvidence(m, 2800) + "```"}},
		},
	}
}

func teamsMessage(m message) any {
	return map[string]any{
		"type": "message",
		"attachments": []map[string]any{
			{"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil,
				"content": map[string]any{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard", "version": "1.4",
					"body": []map[string]any{
						{"type": "TextBlock", "text": truncate(m.title, 256), "weight": "Bolder"},
						{"type": "TextBlock", "text": chatEvidence(m, 3600), "wrap": true},
					},
				}},
		},
	}
}

func discordMessage(m message) any {
	return map[string]any{
		"content":          truncate(m.title, discordContentMax),
		"allowed_mentions": map[string]any{"parse": []string{}},
		"embeds": []map[string]any{
			{"title": truncate(m.title, discordTitleMax), "description": chatEvidence(m, discordDescMax)},
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

func newPagerDutyEvent(routingKey string, m message) (pagerDutyEvent, error) {
	if len(m.dedupKey) > pagerDutyDedupMax {
		return pagerDutyEvent{}, errors.New("deduplication key too long")
	}
	if m.dedupKey == "" {
		return pagerDutyEvent{}, errors.New("no deduplication key")
	}
	action := "trigger"
	if m.resolved {
		action = "resolve"
	}
	payload := pagerDutyEvent{RoutingKey: routingKey, EventAction: action, DedupKey: m.dedupKey}
	payload.Payload.Summary = truncate(m.title, summaryMax)
	payload.Payload.Source = "OpenLLMProxy"
	payload.Payload.Severity = "warning"
	payload.Payload.CustomDetails = m.fields
	return payload, nil
}
