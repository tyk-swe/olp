package notifications

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"

	"github.com/shopspring/decimal"
)

type ruleConfiguration struct {
	WindowSeconds     int    `json:"window_seconds,omitempty"`
	MinimumSamples    int    `json:"minimum_samples,omitempty"`
	Threshold         string `json:"threshold,omitempty"`
	RecoveryThreshold string `json:"recovery_threshold,omitempty"`
	CooldownSeconds   int    `json:"cooldown_seconds,omitempty"`
	Metric            string `json:"metric,omitempty"`
	Period            string `json:"period,omitempty"`
	LeadTimeSeconds   int    `json:"lead_time_seconds,omitempty"`
	LeadDays          int    `json:"lead_days,omitempty"`
}

type thresholdSpec struct {
	trigger, recovery string
	minimumSamples    int
	check             func(raw string, value decimal.Decimal) error
}

type configurationSpec struct {
	fields    []string
	threshold *thresholdSpec
}

var intFields = []struct {
	name               string
	min, max, fallback int
	field              func(*ruleConfiguration) *int
}{
	{"window_seconds", 60, 86400, 300, func(c *ruleConfiguration) *int { return &c.WindowSeconds }},
	{"minimum_samples", 1, 10000, 20, func(c *ruleConfiguration) *int { return &c.MinimumSamples }},
	{"cooldown_seconds", 60, 86400, 900, func(c *ruleConfiguration) *int { return &c.CooldownSeconds }},
	{"lead_time_seconds", 60, 2678400, 86400, func(c *ruleConfiguration) *int { return &c.LeadTimeSeconds }},
	{"lead_days", 1, 366, 30, func(c *ruleConfiguration) *int { return &c.LeadDays }},
}

var choiceFields = []struct {
	name, fallback string
	choices        []string
	field          func(*ruleConfiguration) *string
}{
	{"metric", "latency", []string{"latency", "ttft"}, func(c *ruleConfiguration) *string { return &c.Metric }},
	{"period", "daily", []string{"daily", "weekly", "monthly"}, func(c *ruleConfiguration) *string { return &c.Period }},
}

func (spec configurationSpec) parse(raw json.RawMessage) (ruleConfiguration, error) {
	document := raw
	if len(document) == 0 || string(document) == "null" {
		document = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil || fields == nil {
		return ruleConfiguration{}, errors.New("configuration must be an object")
	}
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(spec.fields, field) {
			return ruleConfiguration{}, fmt.Errorf("configuration.%s is not supported", field)
		}
	}
	var c ruleConfiguration
	d := json.NewDecoder(bytes.NewReader(document))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF {
		return ruleConfiguration{}, errors.New("configuration fields have invalid types")
	}
	provided := func(name string) bool {
		_, ok := fields[name]
		return ok
	}
	for _, field := range intFields {
		if !slices.Contains(spec.fields, field.name) {
			continue
		}
		value := *field.field(&c)
		if !provided(field.name) {
			value = field.fallback
			if field.name == "minimum_samples" && spec.threshold != nil && spec.threshold.minimumSamples > 0 {
				value = spec.threshold.minimumSamples
			}
		}
		if value < field.min || value > field.max {
			return ruleConfiguration{}, fmt.Errorf("configuration.%s must be from %d to %d", field.name, field.min, field.max)
		}
		*field.field(&c) = value
	}
	for _, field := range choiceFields {
		if !slices.Contains(spec.fields, field.name) {
			continue
		}
		if !provided(field.name) {
			*field.field(&c) = field.fallback
		}
		if !slices.Contains(field.choices, *field.field(&c)) {
			return ruleConfiguration{}, fmt.Errorf("configuration.%s is not a supported choice", field.name)
		}
	}
	if spec.threshold == nil {
		return c, nil
	}
	if !provided("threshold") {
		c.Threshold = spec.threshold.trigger
	}
	threshold, err := boundedDecimal(c.Threshold)
	if err != nil {
		return ruleConfiguration{}, err
	}
	if spec.threshold.check != nil {
		if err := spec.threshold.check(c.Threshold, threshold); err != nil {
			return ruleConfiguration{}, err
		}
	}
	if !provided("recovery_threshold") {
		c.RecoveryThreshold = spec.threshold.recovery
		if provided("threshold") {
			c.RecoveryThreshold = threshold.Mul(decimal.New(8, -1)).String()
		}
	}
	recovery, err := decimal.NewFromString(c.RecoveryThreshold)
	if err != nil || len(c.RecoveryThreshold) > 32 || recovery.Exponent() < -12 || recovery.Exponent() > 12 || recovery.IsNegative() || recovery.Cmp(threshold) >= 0 {
		return ruleConfiguration{}, errors.New("recovery_threshold must be a nonnegative decimal below threshold")
	}
	return c, nil
}

func boundedDecimal(raw string) (decimal.Decimal, error) {
	if len(raw) == 0 || len(raw) > 32 {
		return decimal.Decimal{}, errors.New("threshold must be a positive bounded decimal string")
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || !value.IsPositive() || value.Exponent() < -12 || value.Exponent() > 12 || value.Cmp(decimal.NewFromInt(1000000000)) > 0 {
		return decimal.Decimal{}, errors.New("threshold must be a positive bounded decimal string")
	}
	return value, nil
}

func atMostOne(_ string, value decimal.Decimal) error {
	if value.Cmp(decimal.NewFromInt(1)) > 0 {
		return errors.New("error-rate threshold must not exceed 1")
	}
	return nil
}

func failureCount(raw string, _ decimal.Decimal) error {
	integer, err := strconv.Atoi(raw)
	if err != nil || integer < 1 || integer > 10000 {
		return errors.New("credential failure threshold must be an integer from 1 to 10000")
	}
	return nil
}

func (c ruleConfiguration) breached(value string, active bool) (bool, error) {
	if len(value) == 0 || len(value) > 64 {
		return false, errors.New("signal measurement is invalid")
	}
	v, err := decimal.NewFromString(value)
	if err != nil || v.Exponent() < -24 || v.Exponent() > 12 || v.IsNegative() {
		return false, errors.New("signal measurement is invalid")
	}
	if !active {
		threshold, err := decimal.NewFromString(c.Threshold)
		return err == nil && v.Cmp(threshold) >= 0, err
	}
	recovery, err := decimal.NewFromString(c.RecoveryThreshold)
	return err == nil && v.Cmp(recovery) > 0, err
}
