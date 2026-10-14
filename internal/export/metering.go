package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// exportEnvelope is the wrapper every stream record carries; sendCloudEvent and
// sendStripe read the metering payload out of its `data` member.
type exportEnvelope struct {
	SourceID   string          `json:"source_id"`
	ProjectID  *string         `json:"project_id"`
	OccurredAt string          `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

func readEnvelope(record Record) (exportEnvelope, error) {
	var envelope exportEnvelope
	if err := json.Unmarshal(record.Data, &envelope); err != nil {
		return envelope, err
	}
	if envelope.SourceID == "" {
		return envelope, errors.New("record envelope has no source_id")
	}
	return envelope, nil
}

// occurredAt parses the envelope timestamp the way PostgreSQL renders it and
// returns the UTC instant, preserving sub-second precision.
func occurredAt(envelope exportEnvelope) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, envelope.OccurredAt)
	if err != nil {
		return time.Time{}, err
	}
	return at.UTC(), nil
}

// cloudEvent is one structured-mode CloudEvents 1.0 event. Marshaling a struct
// keeps the byte order fixed, so a redelivery sends identical bytes.
type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Subject         string          `json:"subject"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
}

// sendCloudEvent delivers one priced attempt as a structured CloudEvent. The
// event id is the attempt id, so a consumer deduplicates redeliveries.
func (s *Sender) sendCloudEvent(ctx context.Context, endpoint *url.URL, destination Destination, credential []byte, record Record) error {
	credentialDoc, err := parseCredential(credential)
	if err != nil {
		return sendError("credential", err)
	}
	var settings struct {
		Source string `json:"source"`
	}
	if len(destination.Settings) != 0 {
		if err := json.Unmarshal(destination.Settings, &settings); err != nil {
			return sendError("endpoint", err)
		}
	}
	envelope, err := readEnvelope(record)
	if err != nil {
		return sendError("endpoint", err)
	}
	at, err := occurredAt(envelope)
	if err != nil {
		return sendError("endpoint", err)
	}
	source := settings.Source
	if source == "" {
		source = "/olp/installations/" + s.Installation
	}
	subject := "installation"
	if envelope.ProjectID != nil && *envelope.ProjectID != "" {
		subject = *envelope.ProjectID
	}
	body, err := json.Marshal(cloudEvent{
		SpecVersion: "1.0",
		ID:          envelope.SourceID,
		Source:      source,
		Type:        "dev.openllmproxy.attempt.priced",
		Subject:     subject,
		Time:        at.Format(time.RFC3339Nano),
		// DataContentType is the CloudEvents attribute spelling.
		DataContentType: "application/json",
		Data:            envelope.Data,
	})
	if err != nil {
		return sendError("endpoint", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", "application/cloudevents+json")
	request.Header.Set("X-OLP-Event-ID", record.ID)
	if err := applySignature(request.Header, credentialDoc.SigningSecret, body); err != nil {
		return sendError("credential", err)
	}
	if err := applyCredentialHeaders(request.Header, credentialDoc.Headers); err != nil {
		return sendError("credential", err)
	}
	return s.do(request)
}

// meteringValue mirrors olp.metering_value: the amount a priced attempt
// reports under the sink's configured value kind. The second result is false
// when the payload has nothing reportable.
func meteringValue(kind string, data json.RawMessage) (decimal.Decimal, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return decimal.Zero, false
	}
	field := func(name string) (decimal.Decimal, bool) {
		raw, ok := payload[name]
		if !ok || string(raw) == "null" {
			return decimal.Zero, false
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			var number json.Number
			if err := json.Unmarshal(raw, &number); err != nil {
				return decimal.Zero, false
			}
			text = number.String()
		}
		amount, err := decimal.NewFromString(text)
		if err != nil {
			return decimal.Zero, false
		}
		return amount, true
	}
	switch kind {
	case "requests":
		return decimal.NewFromInt(1), true
	case "total_tokens":
		input, inputOK := field("input_tokens")
		output, outputOK := field("output_tokens")
		if !inputOK && !outputOK {
			return decimal.Zero, false
		}
		return input.Add(output), true
	default:
		return field(kind)
	}
}

// plainDecimal renders without an exponent and without trailing zeros.
func plainDecimal(value decimal.Decimal) string {
	text := value.String()
	if strings.Contains(text, ".") {
		text = strings.TrimRight(text, "0")
		text = strings.TrimRight(text, ".")
	}
	return text
}

// stripeSettings is the validated settings a stripe_meter sink carries.
type stripeSettings struct {
	EventName   string            `json:"event_name"`
	Value       string            `json:"value"`
	Customers   map[string]string `json:"customers"`
	CustomerKey string            `json:"customer_key"`
	ValueKey    string            `json:"value_key"`
}

// stripeErrorBody is the documented Stripe error envelope.
type stripeErrorBody struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// sendStripe reports one priced attempt to Stripe's meter events API. The
// identifier derives from the sink and the attempt, so Stripe's own
// idempotency makes a redelivery a no-op.
func (s *Sender) sendStripe(ctx context.Context, endpoint *url.URL, destination Destination, credential []byte, record Record) error {
	credentialDoc, err := parseCredential(credential)
	if err != nil {
		return sendError("credential", err)
	}
	if err := validStripeKey(credentialDoc.APIKey); err != nil {
		return sendError("credential", err)
	}
	var settings stripeSettings
	if err := json.Unmarshal(destination.Settings, &settings); err != nil {
		return sendError("endpoint", err)
	}
	if settings.Value == "" {
		settings.Value = "charge"
	}
	if settings.CustomerKey == "" {
		settings.CustomerKey = "stripe_customer_id"
	}
	if settings.ValueKey == "" {
		settings.ValueKey = "value"
	}
	envelope, err := readEnvelope(record)
	if err != nil {
		return sendError("endpoint", err)
	}
	if envelope.ProjectID == nil {
		return sendError("unmapped", errors.New("the metering record has no consumer project"))
	}
	customer, mapped := settings.Customers[*envelope.ProjectID]
	if !mapped {
		return sendError("unmapped", errors.New("the consumer project is not mapped to a Stripe customer"))
	}
	value, ok := meteringValue(settings.Value, envelope.Data)
	if !ok || !value.IsPositive() {
		return sendError("unmapped", errors.New("the metering record has no reportable value"))
	}
	at, err := occurredAt(envelope)
	if err != nil {
		return sendError("endpoint", err)
	}
	identifier := "olp-" + strings.ReplaceAll(destination.ID, "-", "") +
		"-" + strings.ReplaceAll(envelope.SourceID, "-", "")
	form := url.Values{
		"event_name":                            {settings.EventName},
		"identifier":                            {identifier},
		"timestamp":                             {strconv.FormatInt(at.Unix(), 10)},
		"payload[" + settings.CustomerKey + "]": {customer},
		"payload[" + settings.ValueKey + "]":    {plainDecimal(value)},
	}
	meterURL := *endpoint
	meterURL.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/billing/meter_events"
	meterURL.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, meterURL.String(),
		strings.NewReader(form.Encode()))
	if err != nil {
		return sendError("network", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Authorization", "Bearer "+credentialDoc.APIKey)
	request.Header.Set("Idempotency-Key", identifier)
	response, err := s.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return sendError("timeout", err)
		}
		return sendError("network", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxDeliveryResponseBytes))
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return nil
	}
	if response.StatusCode == http.StatusBadRequest {
		var failure stripeErrorBody
		if err := json.Unmarshal(body, &failure); err == nil && failure.Error.Code == "duplicate_meter_event" {
			return nil
		}
	}
	return sendError("status", fmt.Errorf("stripe meter answered %d", response.StatusCode))
}

// validStripeKey enforces the credential shape a Stripe meter sink needs:
// a restricted or standard secret key, never the account's other secret fields.
func validStripeKey(key string) error {
	if key == "" {
		return errors.New("a stripe_meter credential requires api_key")
	}
	if !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_") {
		return errors.New("api_key must be a Stripe secret or restricted key")
	}
	if len(key) > 512 {
		return errors.New("api_key must be at most 512 bytes")
	}
	if strings.ContainsAny(key, "\r\n") {
		return errors.New("api_key must not contain newlines")
	}
	return nil
}
