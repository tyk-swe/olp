package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type Failure string

const (
	invalidDestinationCode Failure = "invalid_destination"
	timeoutCode            Failure = "timeout"
	networkCode            Failure = "network"
	http4xxCode            Failure = "http_4xx"
	http5xxCode            Failure = "http_5xx"
	smtp4xxCode            Failure = "smtp_4xx"
	smtp5xxCode            Failure = "smtp_5xx"
	credentialCode         Failure = "credential"
	redirectCode           Failure = "redirect"
)

type destination struct {
	Type          contract.NotificationDestinationType
	URL           string
	Configuration json.RawMessage
}

type endpoint interface {
	bind(policy *egress.Policy, secret []byte) (channel, error)
}

type channel interface {
	send(ctx context.Context, s *Sender, m message) error
}

type sendError struct {
	code Failure
	err  error
}

func (e *sendError) Error() string { return string(e.code) }
func (e *sendError) Unwrap() error { return e.err }

func transportError(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return &sendError{timeoutCode, err}
	}
	if errors.Is(err, egress.ErrRedirect) {
		return &sendError{redirectCode, err}
	}
	return &sendError{networkCode, err}
}

type Sender struct {
	policy   egress.Policy
	webhooks *http.Client
	channels *http.Client
}

func NewSender(policy egress.Policy) *Sender {
	return &Sender{
		policy:   policy,
		webhooks: policy.Client(webhookTimeout),
		channels: policy.Client(channelTimeout),
	}
}

func (s *Sender) send(ctx context.Context, d destination, secret []byte, m message) error {
	c, err := open(&s.policy, d, secret)
	if err != nil {
		return &sendError{invalidDestinationCode, err}
	}
	return c.send(ctx, s, m)
}

func sendErrorCode(err error) Failure {
	if err == nil {
		return ""
	}
	var se *sendError
	if errors.As(err, &se) {
		return se.code
	}
	return networkCode
}

func requiresSecret(typ contract.NotificationDestinationType) bool {
	switch typ {
	case contract.NotificationDestinationTypeSlack, contract.NotificationDestinationTypeMsteams,
		contract.NotificationDestinationTypeDiscord, contract.NotificationDestinationTypePagerduty:
		return true
	}
	return false
}

func parse(policy *egress.Policy, d destination) (endpoint, error) {
	switch d.Type {
	case contract.NotificationDestinationTypeWebhook:
		return parseWebhook(policy, d)
	case contract.NotificationDestinationTypeSlack:
		return parseChat(policy, d, slackMessage)
	case contract.NotificationDestinationTypeMsteams:
		return parseChat(policy, d, teamsMessage)
	case contract.NotificationDestinationTypeDiscord:
		return parseChat(policy, d, discordMessage)
	case contract.NotificationDestinationTypePagerduty:
		return parsePagerDuty(policy, d)
	case contract.NotificationDestinationTypeEmail:
		return parseEmail(policy, d)
	default:
		return nil, errors.New("unsupported destination type")
	}
}

func open(policy *egress.Policy, d destination, secret []byte) (channel, error) {
	endpoint, err := parse(policy, d)
	if err != nil {
		return nil, err
	}
	return endpoint.bind(policy, secret)
}

func headerSafe(s string, max int) bool {
	return len(s) <= max && !strings.ContainsAny(s, "\r\n\x00")
}

func textField(fields map[string]json.RawMessage, name string, max int) (string, error) {
	raw, present := fields[name]
	if !present {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || !headerSafe(value, max) {
		return "", fmt.Errorf("%s must be a single-line string of at most %d bytes", name, max)
	}
	return value, nil
}

func requiredTextField(fields map[string]json.RawMessage, name string, max int) (string, error) {
	value, err := textField(fields, name, max)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}
