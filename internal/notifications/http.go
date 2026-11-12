package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

const (
	channelTimeout    = 15 * time.Second
	webhookTimeout    = 5 * time.Second
	channelDrainBytes = 64 << 10
	webhookURLLimit   = 2048
	routingKeyLimit   = 512
)

func parseWebhook(policy *egress.Policy, d destination) (endpoint, error) {
	if err := onlyFields(d.Configuration); err != nil {
		return nil, err
	}
	target, err := policy.ValidateEndpoint(d.URL)
	if err != nil {
		return nil, err
	}
	return webhook{url: target.String()}, nil
}

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

func parsePagerDuty(policy *egress.Policy, d destination) (endpoint, error) {
	if err := onlyFields(d.Configuration); err != nil {
		return nil, err
	}
	if _, err := policy.ValidateEndpoint(d.URL); err != nil {
		return nil, err
	}
	return pagerDuty{url: d.URL}, nil
}

type webhook struct {
	url string
	key []byte
}

func (w webhook) bind(_ *egress.Policy, c credential) (channel, error) {
	key, _ := c.(signingKey)
	return webhook{url: w.url, key: key}, nil
}

func (w webhook) send(ctx context.Context, s *Sender, m message) error {
	header := make(http.Header)
	if len(w.key) != 0 {
		egress.SignBody(header, w.key, m.body)
	}
	status, _, err := post(ctx, s.webhooks, webhookTimeout, w.url, header, m.body)
	if err != nil {
		return err
	}
	return statusError(status)
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

type pagerDuty struct {
	url        string
	routingKey string
}

func (p pagerDuty) bind(_ *egress.Policy, c credential) (channel, error) {
	fields, _ := c.(credentialFields)
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
