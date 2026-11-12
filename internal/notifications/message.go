package notifications

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type envelope struct {
	Version    int                        `json:"version"`
	DeliveryID string                     `json:"delivery_id"`
	Event      contract.NotificationEvent `json:"event"`
	RuleID     string                     `json:"rule_id"`
	RuleName   string                     `json:"rule_name"`
	DedupKey   string                     `json:"dedup_key"`
	Resolved   bool                       `json:"resolved"`
}

type message struct {
	body     []byte
	fields   map[string]json.RawMessage
	title    string
	dedupKey string
	resolved bool
	date     time.Time
}

func newMessage(body []byte, event contract.NotificationEvent, ruleName, dedupKey string, resolved bool, date time.Time) (message, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return message{}, err
	}
	return message{body: body, fields: fields, title: mentionSafe.Replace(messageTitle(event, ruleName)), dedupKey: dedupKey, resolved: resolved, date: date}, nil
}

func messageTitle(event contract.NotificationEvent, ruleName string) string {
	if ruleName == "" {
		return string(event)
	}
	return string(event) + " — " + ruleName
}

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
