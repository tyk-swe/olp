package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
)

const (
	webhookSecretMax  = 1024
	channelTimeout    = 15 * time.Second
	webhookTimeout    = 5 * time.Second
	channelDrainBytes = 64 << 10
)

func parseWebhook(policy *egress.Policy, d Destination) (endpoint, error) {
	if err := requireNoConfiguration(d.Configuration); err != nil {
		return nil, err
	}
	target, err := policy.ValidateEndpoint(d.URL)
	if err != nil {
		return nil, err
	}
	return webhook{url: target.String()}, nil
}

func parseChat(policy *egress.Policy, d Destination, render func(map[string]json.RawMessage) any) (endpoint, error) {
	origin, err := channelOrigin(d.URL)
	if err != nil {
		return nil, err
	}
	if err := requireNoConfiguration(d.Configuration); err != nil {
		return nil, err
	}
	if _, err = policy.ValidateEndpoint(origin.String()); err != nil {
		return nil, err
	}
	return chat{origin: origin, render: render}, nil
}

func parsePagerDuty(policy *egress.Policy, d Destination) (endpoint, error) {
	if err := requireNoConfiguration(d.Configuration); err != nil {
		return nil, err
	}
	if _, err := policy.ValidateEndpoint(d.URL); err != nil {
		return nil, err
	}
	return pagerDuty{url: d.URL}, nil
}

func channelOrigin(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("destination origin is not a URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("destination must be an https://host[:port] origin")
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		return nil, errors.New("destination must be an https://host[:port] origin")
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}

func webhookSecretURL(secret []byte, origin *url.URL) (*url.URL, error) {
	fields, err := decodeObject(secret)
	if err != nil || len(fields) != 1 {
		return nil, errors.New("secret must be a webhook_url object")
	}
	raw, err := textField(fields, "webhook_url", 2048)
	if err != nil || raw == "" {
		return nil, errors.New("secret must be a webhook_url object")
	}
	hook, err := url.Parse(raw)
	if err != nil || hook.Host == "" || hook.User != nil ||
		(hook.Scheme != "https" && hook.Scheme != "http") {
		return nil, errors.New("webhook_url is not an https URL")
	}
	if hook.Scheme+"://"+strings.ToLower(hook.Host) != origin.Scheme+"://"+strings.ToLower(origin.Host) {
		return nil, errors.New("webhook_url must share the destination origin")
	}
	return hook, nil
}

type webhook struct {
	url string
	key []byte
}

func (w webhook) bind(_ *egress.Policy, secret []byte) (channel, error) {
	if len(secret) > webhookSecretMax {
		return nil, errors.New("secret too large")
	}
	return webhook{url: w.url, key: append([]byte(nil), secret...)}, nil
}

func (w webhook) send(ctx context.Context, s *Sender, body []byte) error {
	header := make(http.Header)
	if len(w.key) != 0 {
		sum := hmac.New(sha256.New, w.key)
		_, _ = sum.Write(body)
		header.Set("X-OLP-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	status, _, err := post(ctx, s.webhooks, webhookTimeout, w.url, header, body)
	if err != nil {
		return err
	}
	return statusError(status)
}

type chat struct {
	origin *url.URL
	render func(map[string]json.RawMessage) any
	hook   string
}

func (c chat) bind(policy *egress.Policy, secret []byte) (channel, error) {
	hook, err := webhookSecretURL(secret, c.origin)
	if err != nil {
		return nil, err
	}
	queryFree := *hook
	queryFree.RawQuery, queryFree.ForceQuery = "", false
	queryFree.Fragment, queryFree.RawFragment = "", ""
	if _, err = policy.ValidateEndpoint(queryFree.String()); err != nil {
		return nil, err
	}
	c.hook = hook.String()
	return c, nil
}

func (c chat) send(ctx context.Context, s *Sender, body []byte) error {
	payload, err := json.Marshal(c.render(bodyFields(body)))
	if err != nil {
		return channelError(invalidDestinationCode, err)
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

func (p pagerDuty) bind(_ *egress.Policy, secret []byte) (channel, error) {
	fields, err := decodeObject(secret)
	if err != nil || len(fields) != 1 {
		return nil, errors.New("secret must be a routing_key object")
	}
	key, err := textField(fields, "routing_key", 512)
	if err != nil || key == "" {
		return nil, errors.New("secret must be a routing_key object")
	}
	p.routingKey = key
	return p, nil
}

func (p pagerDuty) send(ctx context.Context, s *Sender, body []byte) error {
	payload, err := newPagerDutyEvent(p.routingKey, body)
	if err != nil {
		return channelError(invalidDestinationCode, err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return channelError(invalidDestinationCode, err)
	}
	status, answer, err := post(ctx, s.channels, channelTimeout, p.url, nil, encoded)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		if status >= 200 && status < 300 {
			return channelError(http5xxCode, errors.New("destination acknowledgement invalid"))
		}
		return statusError(status)
	}
	var ack struct {
		Status   string `json:"status"`
		DedupKey string `json:"dedup_key"`
	}
	if err := json.Unmarshal(answer, &ack); err != nil || ack.Status != "success" || ack.DedupKey != payload.DedupKey {
		return channelError(http5xxCode, errors.New("destination acknowledgement invalid"))
	}
	return nil
}

func post(ctx context.Context, client *http.Client, timeout time.Duration, target string, header http.Header, body []byte) (int, []byte, error) {
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, channelError(invalidDestinationCode, err)
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
		return channelError(http4xxCode, errors.New("destination refused the request"))
	}
	return channelError(http5xxCode, errors.New("destination failed"))
}
