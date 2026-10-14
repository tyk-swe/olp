package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
)

// sinkSettings is every field any sink format's settings accept; a format
// rejects every field that is not its own.
type sinkSettings struct {
	CostBasis   *string           `json:"cost_basis"`
	StartDate   *string           `json:"start_date"`
	Source      *string           `json:"source"`
	EventName   *string           `json:"event_name"`
	Value       *string           `json:"value"`
	Customers   map[string]string `json:"customers"`
	CustomerKey *string           `json:"customer_key"`
	ValueKey    *string           `json:"value_key"`
}

var stripeEventName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)
var stripeCustomerID = regexp.MustCompile(`^cus_[A-Za-z0-9]{1,250}$`)
var stripeFieldKey = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

var meteringValueKinds = map[string]bool{"charge": true, "cost": true,
	"input_tokens": true, "output_tokens": true, "total_tokens": true, "requests": true}

// decodeSettings strict-decodes a sink settings document; unknown fields are
// rejected so a misspelled option can never be silently ignored.
func decodeSettings(raw json.RawMessage) (sinkSettings, error) {
	var settings sinkSettings
	if len(raw) == 0 || string(raw) == "null" {
		return settings, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		return settings, access.Invalid("settings", "settings must be a JSON object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return settings, access.Invalid("settings", "settings must be a single JSON object")
	}
	return settings, nil
}

// presentSettings reports which settings fields the document carries.
func presentSettings(settings sinkSettings) map[string]bool {
	return map[string]bool{
		"cost_basis":   settings.CostBasis != nil,
		"start_date":   settings.StartDate != nil,
		"source":       settings.Source != nil,
		"event_name":   settings.EventName != nil,
		"value":        settings.Value != nil,
		"customers":    settings.Customers != nil,
		"customer_key": settings.CustomerKey != nil,
		"value_key":    settings.ValueKey != nil,
	}
}

// resolveSettings validates a sink's settings against its format and returns
// the canonical document stored, with every default resolved, so readers never
// guess what an omitted field meant. previous is the sink's stored settings on
// an update — nil on create — so an unchanged start_date keeps its acceptance
// window anchored to when it was chosen.
func (m *Management) resolveSettings(ctx context.Context, q access.Queryer, sink *Sink, previous json.RawMessage) (json.RawMessage, error) {
	settings, err := decodeSettings(sink.Settings)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	switch sink.Format {
	case "csv_gzip":
		allowed = map[string]bool{"cost_basis": true, "start_date": true}
	case "cloudevents":
		allowed = map[string]bool{"source": true}
	case "stripe":
		allowed = map[string]bool{"event_name": true, "value": true, "customers": true,
			"customer_key": true, "value_key": true}
	}
	present := presentSettings(settings)
	for _, field := range slices.Sorted(maps.Keys(present)) {
		if present[field] && !allowed[field] {
			return nil, access.Invalid("settings", fmt.Sprintf("%s sinks do not accept settings.%s", sink.Format, field))
		}
	}
	canonical := map[string]any{}
	switch sink.Format {
	case "csv_gzip":
		basis := "cost"
		if settings.CostBasis != nil {
			if *settings.CostBasis != "cost" && *settings.CostBasis != "charge" {
				return nil, access.Invalid("settings.cost_basis", "cost_basis must be cost or charge")
			}
			basis = *settings.CostBasis
		}
		canonical["cost_basis"] = basis
		if settings.StartDate != nil {
			start, err := time.ParseInLocation("2006-01-02", *settings.StartDate, time.UTC)
			if err != nil {
				return nil, access.Invalid("settings.start_date", "start_date must be a YYYY-MM-DD date")
			}
			unchanged := false
			if len(previous) != 0 {
				if stored, perr := decodeSettings(previous); perr == nil &&
					stored.StartDate != nil && *stored.StartDate == *settings.StartDate {
					unchanged = true
				}
			}
			if !unchanged {
				// A new or moved start date must stay inside both the 90-day
				// FOCUS window and the usage-facts retention window.
				usageDays, err := sinkUsageDays(ctx, q)
				if err != nil {
					return nil, err
				}
				today := time.Now().UTC().Truncate(24 * time.Hour)
				earliest := today.AddDate(0, 0, -min(90, usageDays-1))
				if start.Before(earliest) || start.After(today) {
					return nil, access.Invalid("settings.start_date",
						"start_date must be a YYYY-MM-DD date within the retained window")
				}
			}
			canonical["start_date"] = *settings.StartDate
		}
	case "cloudevents":
		if settings.Source != nil {
			source := *settings.Source
			_, parseErr := url.Parse(source)
			bad := false
			for _, c := range source {
				bad = bad || unicode.IsSpace(c) || unicode.IsControl(c)
			}
			if len(source) < 1 || len(source) > 256 || parseErr != nil || bad {
				return nil, access.Invalid("settings.source",
					"source must be a URI reference of 1 to 256 characters without whitespace")
			}
			canonical["source"] = source
		}
	case "stripe":
		if settings.EventName == nil || !stripeEventName.MatchString(*settings.EventName) {
			return nil, access.Invalid("settings.event_name",
				"event_name is required: 1 to 100 of letters, digits, underscore, dot, colon or dash")
		}
		canonical["event_name"] = *settings.EventName
		value := "charge"
		if settings.Value != nil {
			if !meteringValueKinds[*settings.Value] {
				return nil, access.Invalid("settings.value",
					"value must be charge, cost, input_tokens, output_tokens, total_tokens or requests")
			}
			value = *settings.Value
		}
		canonical["value"] = value
		if len(settings.Customers) < 1 || len(settings.Customers) > 1000 {
			return nil, access.Invalid("settings.customers",
				"customers must map from 1 to 1000 projects")
		}
		for project, customer := range settings.Customers {
			parsed, err := uuid.Parse(project)
			if err != nil || parsed.String() != project {
				return nil, access.Invalid("settings.customers",
					"customers keys must be canonical project UUIDs")
			}
			var exists bool
			if err := q.QueryRow(ctx, filterProjectScopeSQL, project).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				return nil, access.Invalid("settings.customers",
					fmt.Sprintf("customers names project %s which does not exist", project))
			}
			if sink.ProjectID != nil && project != *sink.ProjectID {
				return nil, access.Invalid("settings.customers",
					"a project sink maps only its own project")
			}
			if !stripeCustomerID.MatchString(customer) {
				return nil, access.Invalid("settings.customers",
					"customers values must be cus_ identifiers")
			}
		}
		canonical["customers"] = settings.Customers
		customerKey := "stripe_customer_id"
		if settings.CustomerKey != nil {
			if !stripeFieldKey.MatchString(*settings.CustomerKey) {
				return nil, access.Invalid("settings.customer_key",
					"customer_key must be 1 to 64 of letters, digits or underscore")
			}
			customerKey = *settings.CustomerKey
		}
		valueKey := "value"
		if settings.ValueKey != nil {
			if !stripeFieldKey.MatchString(*settings.ValueKey) {
				return nil, access.Invalid("settings.value_key",
					"value_key must be 1 to 64 of letters, digits or underscore")
			}
			valueKey = *settings.ValueKey
		}
		if customerKey == valueKey {
			return nil, access.Invalid("settings.value_key", "customer_key and value_key must differ")
		}
		canonical["customer_key"] = customerKey
		canonical["value_key"] = valueKey
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// sinkUsageDays reads the usage-facts retention window a focus export's start
// date is bounded by; like the retention worker it treats an absent setting as
// 90 days and an unreadable one as an error.
func sinkUsageDays(ctx context.Context, q access.Queryer) (int, error) {
	var value string
	if err := q.QueryRow(ctx, `SELECT COALESCE((SELECT value FROM olp.settings WHERE key='retention.usage_days'),'90')`).Scan(&value); err != nil {
		return 0, err
	}
	days, err := strconv.Atoi(value)
	if err != nil || days < 1 || days > 3650 {
		return 0, fmt.Errorf("retention.usage_days is invalid: %q", value)
	}
	return days, nil
}

// settingsCostBasis reports the stored document's cost basis; an absent field
// is the default.
func settingsCostBasis(raw json.RawMessage) string {
	settings, err := decodeSettings(raw)
	if err != nil || settings.CostBasis == nil {
		return "cost"
	}
	return *settings.CostBasis
}

// validateSinkCredential enforces the credential shape a sink type carries.
// A stripe_meter credential is only its api_key; every other type forbids one.
func validateSinkCredential(sinkType string, credential json.RawMessage) error {
	if len(credential) == 0 || string(credential) == "null" {
		return nil
	}
	doc, err := parseCredential(credential)
	if err != nil {
		return access.Invalid("credential", "credential must be an object with the sink's credential fields")
	}
	if sinkType == "stripe_meter" {
		if doc.SigningSecret != "" || doc.Region != "" || doc.AuthMode != "" || len(doc.Secret) != 0 {
			return access.Invalid("credential", "a stripe_meter credential carries only api_key")
		}
		if err := validStripeKey(doc.APIKey); err != nil {
			return access.Invalid("credential", err.Error())
		}
		return nil
	}
	if doc.APIKey != "" {
		return access.Invalid("credential", "api_key is only valid on stripe_meter sinks")
	}
	return nil
}

// credentialPresent reports whether the request carried a credential document.
func credentialPresent(raw json.RawMessage) bool {
	return len(raw) != 0 && string(raw) != "null"
}
