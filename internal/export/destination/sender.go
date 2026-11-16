package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

const (
	DeliveryTimeout          = 15 * time.Second
	MaxCredentialBytes       = 65536
	maxDeliveryResponseBytes = 65536
)

type Record struct {
	ID        string
	Stream    string
	At        time.Time
	Data      json.RawMessage
	ObjectKey string
}

type Target struct {
	sinkID    string
	url       string
	transport transport
}

type transport interface {
	send(ctx context.Context, s *Sender, target Target, endpoint *url.URL, record Record) error
}

func Load(sinkID, rawURL string, spec Spec, credential []byte) (Target, error) {
	t, err := spec.transport(credential, false)
	if err != nil {
		return Target{}, sendError("credential", err)
	}
	return Target{sinkID: sinkID, url: rawURL, transport: t}, nil
}

func CheckCredential(spec Spec, credential []byte) error {
	_, err := spec.transport(credential, true)
	return err
}

type Sender struct {
	installation string
	policy       *egress.Policy
	auth         *connectors.Auth
	client       *http.Client
	getenv       func(string) string
}

func NewSender(policy *egress.Policy, getenv func(string) string, installation string) *Sender {
	client := policy.Client(DeliveryTimeout)
	client.Timeout = DeliveryTimeout
	return &Sender{installation: installation, policy: policy, auth: connectors.NewAuth(policy, getenv), client: client, getenv: getenv}
}

func (s *Sender) Send(ctx context.Context, target Target, record Record) error {
	endpoint, err := s.policy.ValidateEndpoint(target.url)
	if err != nil {
		return sendError("endpoint", err)
	}
	return target.transport.send(ctx, s, target, endpoint, record)
}

type SendError struct {
	Rejected bool
	Code     string
	Err      error
}

func (e *SendError) Error() string { return e.Code }
func (e *SendError) Unwrap() error { return e.Err }

func sendError(code string, err error) error {
	return &SendError{Code: code, Err: err}
}

func DeliveryErrorCode(err error) string {
	var send *SendError
	if errors.As(err, &send) {
		return send.Code
	}
	return "network"
}

func DeliveryRejected(err error) bool {
	var send *SendError
	return errors.As(err, &send) && send.Rejected
}

func (s *Sender) roundTrip(req *http.Request) (*http.Response, error) {
	response, err := s.client.Do(req)
	if err == nil {
		return response, nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil, sendError("timeout", err)
	}
	return nil, sendError("network", err)
}

func (s *Sender) response(req *http.Request) (*http.Response, error) {
	response, err := s.roundTrip(req)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		drain(response)
		return nil, sendError("status", fmt.Errorf("destination answered %d", response.StatusCode))
	}
	return response, nil
}

func (s *Sender) do(req *http.Request) error {
	response, err := s.response(req)
	if err != nil {
		return err
	}
	drain(response)
	return nil
}

func drain(response *http.Response) {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxDeliveryResponseBytes))
}

func post(ctx context.Context, endpoint *url.URL, contentType string, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, sendError("network", err)
	}
	request.Header.Set("Content-Type", contentType)
	return request, nil
}

func decodeCredential(raw []byte, strict bool, into any, shape string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if len(raw) > MaxCredentialBytes {
		return fmt.Errorf("credential must be at most %d bytes", MaxCredentialBytes)
	}
	if !strict {
		return json.Unmarshal(raw, into)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return errors.New(shape)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("credential must be one JSON document")
	}
	return nil
}
