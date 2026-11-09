package destination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
)

type MeteringValue string

const (
	MeterCharge       MeteringValue = "charge"
	MeterCost         MeteringValue = "cost"
	MeterInputTokens  MeteringValue = "input_tokens"
	MeterOutputTokens MeteringValue = "output_tokens"
	MeterTotalTokens  MeteringValue = "total_tokens"
	MeterRequests     MeteringValue = "requests"
)

func (v MeteringValue) valid() bool {
	switch v {
	case MeterCharge, MeterCost, MeterInputTokens, MeterOutputTokens, MeterTotalTokens, MeterRequests:
		return true
	}
	return false
}

type StripeMeter struct {
	EventName   string            `json:"event_name"`
	Value       MeteringValue     `json:"value"`
	Customers   map[string]string `json:"customers"`
	CustomerKey string            `json:"customer_key"`
	ValueKey    string            `json:"value_key"`
}

func (StripeMeter) Type() Type     { return contract.CreateExportSinkRequestTypeStripeMeter }
func (StripeMeter) Format() Format { return contract.CreateExportSinkRequestFormatStripe }

var (
	stripeEventName  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
	stripeCustomerID = regexp.MustCompile(`^cus_[A-Za-z0-9]{1,250}$`)
	stripeFieldKey   = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
)

func parseStripeMeter(_ Type, settings json.RawMessage) (Spec, error) {
	var input struct {
		EventName   *string           `json:"event_name"`
		Value       *MeteringValue    `json:"value"`
		Customers   map[string]string `json:"customers"`
		CustomerKey *string           `json:"customer_key"`
		ValueKey    *string           `json:"value_key"`
	}
	if err := decodeSettings(contract.CreateExportSinkRequestFormatStripe, settings, &input, "event_name", "value", "customers", "customer_key", "value_key"); err != nil {
		return nil, err
	}
	if input.EventName == nil || !stripeEventName.MatchString(*input.EventName) {
		return nil, access.Invalid("settings.event_name", "event_name is required: 1 to 100 of letters, digits, underscore, dot, colon or dash")
	}
	spec := StripeMeter{EventName: *input.EventName, Value: MeterCharge, Customers: input.Customers, CustomerKey: "stripe_customer_id", ValueKey: "value"}
	if input.Value != nil {
		if !input.Value.valid() {
			return nil, access.Invalid("settings.value", "value must be charge, cost, input_tokens, output_tokens, total_tokens or requests")
		}
		spec.Value = *input.Value
	}
	if len(spec.Customers) < 1 || len(spec.Customers) > 1000 {
		return nil, access.Invalid("settings.customers", "customers must map from 1 to 1000 projects")
	}
	for project, customer := range spec.Customers {
		if parsed, err := uuid.Parse(project); err != nil || parsed.String() != project {
			return nil, access.Invalid("settings.customers", "customers keys must be canonical project UUIDs")
		}
		if !stripeCustomerID.MatchString(customer) {
			return nil, access.Invalid("settings.customers", "customers values must be cus_ identifiers")
		}
	}
	if input.CustomerKey != nil {
		if !stripeFieldKey.MatchString(*input.CustomerKey) {
			return nil, access.Invalid("settings.customer_key", "customer_key must be 1 to 64 of letters, digits or underscore")
		}
		spec.CustomerKey = *input.CustomerKey
	}
	if input.ValueKey != nil {
		if !stripeFieldKey.MatchString(*input.ValueKey) {
			return nil, access.Invalid("settings.value_key", "value_key must be 1 to 64 of letters, digits or underscore")
		}
		spec.ValueKey = *input.ValueKey
	}
	if spec.CustomerKey == spec.ValueKey {
		return nil, access.Invalid("settings.value_key", "customer_key and value_key must differ")
	}
	return spec, nil
}

type stripeMeter struct {
	spec   StripeMeter
	apiKey string
}

func (m StripeMeter) transport(raw []byte, strict bool) (transport, error) {
	var credential struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeCredential(raw, strict, &credential, "a stripe_meter credential carries only api_key"); err != nil {
		return nil, err
	}
	return stripeMeter{spec: m, apiKey: credential.APIKey}, checkStripeKey(credential.APIKey)
}

func checkStripeKey(key string) error {
	switch {
	case key == "":
		return errors.New("a stripe_meter credential requires api_key")
	case !strings.HasPrefix(key, "sk_") && !strings.HasPrefix(key, "rk_"):
		return errors.New("api_key must be a Stripe secret or restricted key")
	case len(key) > 512:
		return errors.New("api_key must be at most 512 bytes")
	case strings.ContainsAny(key, "\r\n"):
		return errors.New("api_key must not contain newlines")
	}
	return nil
}

func (m stripeMeter) send(ctx context.Context, s *Sender, target Target, endpoint *url.URL, record Record) error {
	envelope, err := readEnvelope(record)
	if err != nil {
		return sendError("endpoint", err)
	}
	if envelope.ProjectID == nil {
		return sendError("unmapped", errors.New("the metering record has no consumer project"))
	}
	customer, mapped := m.spec.Customers[*envelope.ProjectID]
	if !mapped {
		return sendError("unmapped", errors.New("the consumer project is not mapped to a Stripe customer"))
	}
	value, ok := meteringValue(m.spec.Value, envelope.Data)
	if !ok || !value.IsPositive() {
		return sendError("unmapped", errors.New("the metering record has no reportable value"))
	}
	identifier := "olp-" + strings.ReplaceAll(target.sinkID, "-", "") + "-" + strings.ReplaceAll(envelope.SourceID, "-", "")
	form := url.Values{
		"event_name":                          {m.spec.EventName},
		"identifier":                          {identifier},
		"timestamp":                           {strconv.FormatInt(envelope.OccurredAt.Unix(), 10)},
		"payload[" + m.spec.CustomerKey + "]": {customer},
		"payload[" + m.spec.ValueKey + "]":    {plainDecimal(value)},
	}
	meter := *endpoint
	meter.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/billing/meter_events"
	meter.RawQuery = ""
	request, err := post(ctx, &meter, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+m.apiKey)
	request.Header.Set("Idempotency-Key", identifier)
	response, err := s.roundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxDeliveryResponseBytes))
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if response.StatusCode == http.StatusBadRequest && json.Unmarshal(body, &failure) == nil && failure.Error.Code == "duplicate_meter_event" {
		return nil
	}
	return sendError("status", fmt.Errorf("stripe meter answered %d", response.StatusCode))
}

func meteringValue(kind MeteringValue, data json.RawMessage) (decimal.Decimal, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return decimal.Zero, false
	}
	field := func(name string) (decimal.Decimal, bool) {
		raw, ok := payload[name]
		if !ok || string(raw) == "null" {
			return decimal.Zero, false
		}
		var number json.Number
		var text string
		switch {
		case json.Unmarshal(raw, &text) == nil:
		case json.Unmarshal(raw, &number) == nil:
			text = number.String()
		default:
			return decimal.Zero, false
		}
		amount, err := decimal.NewFromString(text)
		return amount, err == nil
	}
	switch kind {
	case MeterRequests:
		return decimal.NewFromInt(1), true
	case MeterTotalTokens:
		input, inputOK := field("input_tokens")
		output, outputOK := field("output_tokens")
		return input.Add(output), inputOK || outputOK
	}
	return field(string(kind))
}

func plainDecimal(value decimal.Decimal) string {
	text := value.String()
	if !strings.Contains(text, ".") {
		return text
	}
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}
