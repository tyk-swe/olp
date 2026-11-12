package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

const webhookURLLimit = 2048

func parseChat(policy *egress.Policy, d destination, render func(message) any) (endpoint, error) {
	if err := onlyFields(d.Configuration); err != nil {
		return nil, err
	}
	origin, err := policy.ValidateEndpoint(d.URL)
	if err != nil {
		return nil, err
	}
	if origin.Path != "" {
		return nil, errors.New("destination must be an https://host[:port] origin")
	}
	return chatOrigin{origin: origin, render: render}, nil
}

type chatOrigin struct {
	origin *url.URL
	render func(message) any
}

func (c chatOrigin) bind(policy *egress.Policy, secret credential) (channel, error) {
	fields, _ := secret.(credentialFields)
	if err := onlyFields(fields, "webhook_url"); err != nil {
		return nil, err
	}
	raw, err := requiredTextField(fields, "webhook_url", webhookURLLimit)
	if err != nil {
		return nil, err
	}
	hook, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("webhook_url is not a URL")
	}
	queryFree := url.URL{Scheme: hook.Scheme, User: hook.User, Host: hook.Host, Path: hook.Path, RawPath: hook.RawPath}
	base, err := policy.ValidateEndpoint(queryFree.String())
	if err != nil {
		return nil, err
	}
	if connectors.Origin(base) != connectors.Origin(c.origin) {
		return nil, errors.New("webhook_url must share the destination origin")
	}
	return chat{hook: raw, render: c.render}, nil
}

type chat struct {
	hook   string
	render func(message) any
}

func (c chat) send(ctx context.Context, s *Sender, m message) error {
	payload, err := json.Marshal(c.render(m))
	if err != nil {
		return &sendError{invalidDestinationCode, err}
	}
	status, _, err := post(ctx, s.channels, channelTimeout, c.hook, nil, payload)
	if err != nil {
		return err
	}
	return statusError(status)
}

const (
	discordContentMax = 2000
	discordTitleMax   = 256
	discordDescMax    = 4096
)

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
