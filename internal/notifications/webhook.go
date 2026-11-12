package notifications

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/egress"
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
