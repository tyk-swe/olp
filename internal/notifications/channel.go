package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"strings"
	"time"

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

var errMalformedCredential = errors.New("credential must be a JSON object")

type signingKey []byte

type credentialFields map[string]json.RawMessage

func readSigningKey(secret []byte) (signingKey, error) {
	return signingKey(secret), nil
}

func readCredentialFields(secret []byte) (credentialFields, error) {
	fields, err := decodeObject(secret)
	if err != nil {
		return nil, errMalformedCredential
	}
	return fields, nil
}

type endpoint[C any] interface {
	bind(policy *egress.Policy, credential C) (channel, error)
}

type channel interface {
	send(ctx context.Context, s *Sender, m message) error
}

type destinationKind interface {
	check(policy *egress.Policy, d destination) error
	open(policy *egress.Policy, d destination, secret []byte) (channel, error)
}

type kind[C any] struct {
	credential func(secret []byte) (C, error)
	parse      func(policy *egress.Policy, d destination) (endpoint[C], error)
}

func (k kind[C]) check(policy *egress.Policy, d destination) error {
	_, err := k.parse(policy, d)
	return err
}

func (k kind[C]) open(policy *egress.Policy, d destination, secret []byte) (channel, error) {
	credential, err := k.credential(secret)
	if err != nil {
		return nil, err
	}
	endpoint, err := k.parse(policy, d)
	if err != nil {
		return nil, err
	}
	return endpoint.bind(policy, credential)
}

func kindOf(typ contract.NotificationDestinationType) destinationKind {
	switch typ {
	case contract.NotificationDestinationTypeWebhook:
		return kind[signingKey]{readSigningKey, parseWebhook}
	case contract.NotificationDestinationTypeSlack:
		return kind[credentialFields]{readCredentialFields, chatRenderer(slackMessage).parse}
	case contract.NotificationDestinationTypeMsteams:
		return kind[credentialFields]{readCredentialFields, chatRenderer(teamsMessage).parse}
	case contract.NotificationDestinationTypeDiscord:
		return kind[credentialFields]{readCredentialFields, chatRenderer(discordMessage).parse}
	case contract.NotificationDestinationTypePagerduty:
		return kind[credentialFields]{readCredentialFields, parsePagerDuty}
	case contract.NotificationDestinationTypeEmail:
		return kind[credentialFields]{readCredentialFields, parseEmail}
	default:
		return kind[credentialFields]{readCredentialFields, parseUnsupported}
	}
}

func parseUnsupported(*egress.Policy, destination) (endpoint[credentialFields], error) {
	return nil, errors.New("unsupported destination type")
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
	c, err := kindOf(d.Type).open(&s.policy, d, secret)
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

func headerSafe(s string, max int) bool {
	return len(s) <= max && !strings.ContainsAny(s, "\r\n\x00")
}

func requiredTextField(fields map[string]json.RawMessage, name string, max int) (string, error) {
	var value string
	if err := json.Unmarshal(fields[name], &value); err != nil || value == "" || !headerSafe(value, max) {
		return "", fmt.Errorf("%s must be a non-empty single-line string of at most %d bytes", name, max)
	}
	return value, nil
}

const (
	channelTimeout    = 15 * time.Second
	webhookTimeout    = 5 * time.Second
	channelDrainBytes = 64 << 10
)

func post(ctx context.Context, client *http.Client, timeout time.Duration, target string, header http.Header, body []byte) (int, []byte, error) {
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, &sendError{invalidDestinationCode, err}
	}
	req.Header.Set("Content-Type", "application/json")
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, transportError(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, channelDrainBytes))
	return resp.StatusCode, answer, nil
}

func statusError(status int) error {
	if status >= 200 && status < 300 {
		return nil
	}
	if status < 500 {
		return &sendError{http4xxCode, errors.New("destination refused the request")}
	}
	return &sendError{http5xxCode, errors.New("destination failed")}
}
