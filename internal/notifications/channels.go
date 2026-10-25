package notifications

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/egress"
)

const (
	webhookType   = "webhook"
	slackType     = "slack"
	msteamsType   = "msteams"
	discordType   = "discord"
	pagerDutyType = "pagerduty"
	emailType     = "email"

	invalidDestinationCode = "invalid_destination"
	timeoutCode            = "timeout"
	networkCode            = "network"
	http4xxCode            = "http_4xx"
	http5xxCode            = "http_5xx"
	smtp4xxCode            = "smtp_4xx"
	smtp5xxCode            = "smtp_5xx"
	credentialCode         = "credential"
	redirectCode           = "redirect"

	channelSecretMax = 16384
	channelConfigMax = 4096
)

// Destination identifies a notification channel and its configuration.
type Destination struct {
	Type          string
	URL           string
	Configuration json.RawMessage
}

type endpoint interface {
	bind(policy *egress.Policy, secret []byte) (channel, error)
}

type channel interface {
	send(ctx context.Context, s *Sender, body []byte) error
}

type sendError struct {
	code string
	err  error
}

func (e *sendError) Error() string { return e.code }
func (e *sendError) Unwrap() error { return e.err }

func channelError(code string, err error) error {
	return &sendError{code: code, err: err}
}

func transportError(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return channelError(timeoutCode, err)
	}
	if errors.Is(err, egress.ErrRedirect) {
		return channelError(redirectCode, err)
	}
	return channelError(networkCode, err)
}

// Sender reuses egress-checked HTTP clients for notification delivery.
type Sender struct {
	policy   egress.Policy
	webhooks *http.Client
	channels *http.Client
}

// NewSender creates a sender with bounded webhook and channel clients.
func NewSender(policy egress.Policy) *Sender {
	return &Sender{
		policy:   policy,
		webhooks: policy.Client(webhookTimeout),
		channels: policy.Client(channelTimeout),
	}
}

// Send validates and binds a destination before sending its body.
func (s *Sender) Send(ctx context.Context, d Destination, secret, body []byte) error {
	c, err := open(&s.policy, d, secret)
	if err != nil {
		return channelError(invalidDestinationCode, err)
	}
	return c.send(ctx, s, body)
}

// SendErrorCode returns the safe delivery category for err.
func SendErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var se *sendError
	if errors.As(err, &se) {
		return se.code
	}
	return networkCode
}

// RequiresSecret reports whether a destination type requires a secret.
func RequiresSecret(typ string) bool {
	switch typ {
	case slackType, msteamsType, discordType, pagerDutyType:
		return true
	}
	return false
}

// ValidateDestinationShape checks a destination without binding its secret.
func ValidateDestinationShape(policy *egress.Policy, d Destination) error {
	_, err := parse(policy, d)
	return err
}

// ValidateDestination checks a destination and its secret.
func ValidateDestination(policy *egress.Policy, d Destination, secret []byte) error {
	_, err := open(policy, d, secret)
	return err
}

func parse(policy *egress.Policy, d Destination) (endpoint, error) {
	if policy == nil {
		return nil, errors.New("egress policy unavailable")
	}
	if len(d.Configuration) > channelConfigMax {
		return nil, errors.New("configuration too large")
	}
	switch cmp.Or(d.Type, webhookType) {
	case webhookType:
		return parseWebhook(policy, d)
	case slackType:
		return parseChat(policy, d, slackMessage)
	case msteamsType:
		return parseChat(policy, d, teamsMessage)
	case discordType:
		return parseChat(policy, d, discordMessage)
	case pagerDutyType:
		return parsePagerDuty(policy, d)
	case emailType:
		return parseEmail(policy, d)
	default:
		return nil, errors.New("unsupported destination type")
	}
}

func open(policy *egress.Policy, d Destination, secret []byte) (channel, error) {
	if len(secret) > channelSecretMax {
		return nil, errors.New("secret too large")
	}
	endpoint, err := parse(policy, d)
	if err != nil {
		return nil, err
	}
	return endpoint.bind(policy, secret)
}

func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("not an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var decoded map[string]json.RawMessage
	if err := dec.Decode(&decoded); err != nil || decoded == nil {
		return nil, errors.New("not an object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return decoded, nil
}

func textField(fields map[string]json.RawMessage, name string, max int) (string, error) {
	raw, present := fields[name]
	if !present {
		return "", nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", errors.New("not a string")
	}
	if len(*value) > max || strings.ContainsAny(*value, "\r\n\x00") {
		return "", errors.New("invalid length or control characters")
	}
	return *value, nil
}

func requireNoConfiguration(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	fields, err := decodeObject(raw)
	if err != nil || len(fields) != 0 {
		return errors.New("destination carries no configuration")
	}
	return nil
}
