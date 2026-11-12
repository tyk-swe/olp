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

type chatRenderer func(message) any

func (render chatRenderer) parse(policy *egress.Policy, d destination) (endpoint[credentialFields], error) {
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
	render chatRenderer
}

func (c chatOrigin) bind(policy *egress.Policy, fields credentialFields) (channel, error) {
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
	render chatRenderer
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

type slackPayload struct {
	Text   string       `json:"text"`
	Blocks []slackBlock `json:"blocks"`
}

type slackBlock struct {
	Type string    `json:"type"`
	Text slackText `json:"text"`
}

type slackText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func slackMessage(m message) any {
	return slackPayload{
		Text: truncate(m.title, 150),
		Blocks: []slackBlock{
			{Type: "header", Text: slackText{Type: "plain_text", Text: truncate(m.title, 150)}},
			{Type: "section", Text: slackText{Type: "mrkdwn", Text: "```" + chatEvidence(m, 2800) + "```"}},
		},
	}
}

type teamsPayload struct {
	Type        string            `json:"type"`
	Attachments []teamsAttachment `json:"attachments"`
}

type teamsAttachment struct {
	ContentType string    `json:"contentType"`
	ContentURL  *string   `json:"contentUrl"`
	Content     teamsCard `json:"content"`
}

type teamsCard struct {
	Schema  string           `json:"$schema"`
	Type    string           `json:"type"`
	Version string           `json:"version"`
	Body    []teamsTextBlock `json:"body"`
}

type teamsTextBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Weight string `json:"weight,omitempty"`
	Wrap   bool   `json:"wrap,omitempty"`
}

func teamsMessage(m message) any {
	return teamsPayload{
		Type: "message",
		Attachments: []teamsAttachment{{
			ContentType: "application/vnd.microsoft.card.adaptive",
			Content: teamsCard{
				Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
				Type:    "AdaptiveCard",
				Version: "1.4",
				Body: []teamsTextBlock{
					{Type: "TextBlock", Text: truncate(m.title, 256), Weight: "Bolder"},
					{Type: "TextBlock", Text: chatEvidence(m, 3600), Wrap: true},
				},
			},
		}},
	}
}

type discordPayload struct {
	Content         string          `json:"content"`
	AllowedMentions discordMentions `json:"allowed_mentions"`
	Embeds          []discordEmbed  `json:"embeds"`
}

type discordMentions struct {
	Parse []string `json:"parse"`
}

type discordEmbed struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

func discordMessage(m message) any {
	return discordPayload{
		Content:         truncate(m.title, discordContentMax),
		AllowedMentions: discordMentions{Parse: []string{}},
		Embeds:          []discordEmbed{{Title: truncate(m.title, discordTitleMax), Description: chatEvidence(m, discordDescMax)}},
	}
}
