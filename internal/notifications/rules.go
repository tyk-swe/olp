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

type RuleConfiguration struct {
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

var signalFields = []string{"window_seconds", "minimum_samples", "threshold", "recovery_threshold", "cooldown_seconds"}

type rulePolicy struct {
	fields              []string
	threshold, recovery string
}

var rulePolicies = map[string]rulePolicy{
	"budget.threshold":            {},
	"provider.grant.lapsed":       {},
	"provider.error_rate":         {fields: signalFields, threshold: "0.1", recovery: "0.05"},
	"provider.credential.failing": {fields: signalFields, threshold: "3", recovery: "0"},
	"route.latency":               {fields: []string{"window_seconds", "minimum_samples", "threshold", "recovery_threshold", "cooldown_seconds", "metric"}, threshold: "1000", recovery: "800"},
	"key.expiring":                {fields: []string{"lead_time_seconds"}},
	"model.retirement":            {fields: []string{"lead_days", "cooldown_seconds"}},
	"report.spend":                {fields: []string{"period"}},
	"budget.exhausted":            {fields: []string{"cooldown_seconds"}},
	"provider.circuit.open":       {fields: []string{"cooldown_seconds"}},
	"provider.circuit.closed":     {fields: []string{"cooldown_seconds"}},
	"runtime.install_failed":      {fields: []string{"cooldown_seconds"}},
	"worker.stale":                {fields: []string{"cooldown_seconds"}},
}

var intFields = []struct {
	name               string
	min, max, fallback int
	field              func(*RuleConfiguration) *int
}{
	{"window_seconds", 60, 86400, 300, func(c *RuleConfiguration) *int { return &c.WindowSeconds }},
	{"minimum_samples", 1, 10000, 20, func(c *RuleConfiguration) *int { return &c.MinimumSamples }},
	{"cooldown_seconds", 60, 86400, 900, func(c *RuleConfiguration) *int { return &c.CooldownSeconds }},
	{"lead_time_seconds", 60, 2678400, 86400, func(c *RuleConfiguration) *int { return &c.LeadTimeSeconds }},
	{"lead_days", 1, 366, 30, func(c *RuleConfiguration) *int { return &c.LeadDays }},
}

func SupportsEvent(event string) bool {
	_, ok := rulePolicies[event]
	return ok
}

func ParseRuleConfiguration(event string, raw json.RawMessage) (RuleConfiguration, error) {
	var c RuleConfiguration
	policy, ok := rulePolicies[event]
	if !ok {
		return c, errors.New("unsupported notification event")
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return c, errors.New("configuration must be an object")
	}
	allowed := policy.fields
	for _, field := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, field) {
			return c, fmt.Errorf("configuration.%s is not supported for %s", field, event)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("configuration fields have invalid types")
	}

	provided := func(name string) bool {
		_, ok := fields[name]
		return ok
	}
	for _, field := range intFields {
		if !slices.Contains(allowed, field.name) {
			continue
		}
		value := field.fallback
		if provided(field.name) {
			value = *field.field(&c)
		} else if event == "provider.credential.failing" && field.name == "minimum_samples" {
			value = 3
		}
		if value < field.min || value > field.max {
			return c, fmt.Errorf("configuration.%s must be from %d to %d", field.name, field.min, field.max)
		}
		*field.field(&c) = value
	}
	if policy.threshold != "" {
		if !provided("threshold") {
			c.Threshold = policy.threshold
		}
		threshold, err := boundedThreshold(event, c.Threshold)
		if err != nil {
			return c, err
		}
		if !provided("recovery_threshold") {
			if provided("threshold") {
				c.RecoveryThreshold = threshold.Mul(decimal.New(8, -1)).String()
			} else {
				c.RecoveryThreshold = policy.recovery
			}
		}
		recovery, err := decimal.NewFromString(c.RecoveryThreshold)
		if err != nil || len(c.RecoveryThreshold) > 32 || recovery.Exponent() < -12 || recovery.Exponent() > 12 || recovery.IsNegative() || recovery.Cmp(threshold) >= 0 {
			return c, errors.New("recovery_threshold must be a nonnegative decimal below threshold")
		}
	}
	if event == "route.latency" {
		if !provided("metric") {
			c.Metric = "latency"
		}
		if c.Metric != "latency" && c.Metric != "ttft" {
			return c, errors.New("metric must be latency or ttft")
		}
	}
	if event == "report.spend" {
		if !provided("period") {
			c.Period = "daily"
		}
		if c.Period != "daily" && c.Period != "weekly" && c.Period != "monthly" {
			return c, errors.New("period must be daily, weekly or monthly")
		}
	}
	return c, nil
}

func boundedThreshold(event, raw string) (decimal.Decimal, error) {
	if len(raw) == 0 || len(raw) > 32 {
		return decimal.Decimal{}, errors.New("threshold must be a positive bounded decimal string")
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || !value.IsPositive() || value.Exponent() < -12 || value.Exponent() > 12 || value.Cmp(decimal.NewFromInt(1000000000)) > 0 {
		return decimal.Decimal{}, errors.New("threshold must be a positive bounded decimal string")
	}
	if event == "provider.error_rate" && value.Cmp(decimal.NewFromInt(1)) > 0 {
		return decimal.Decimal{}, errors.New("error-rate threshold must not exceed 1")
	}
	if event == "provider.credential.failing" {
		integer, err := strconv.Atoi(raw)
		if err != nil || integer < 1 || integer > 10000 {
			return decimal.Decimal{}, errors.New("credential failure threshold must be an integer from 1 to 10000")
		}
	}
	return value, nil
}

func (c RuleConfiguration) ThresholdState(value string, active bool) (bool, error) {
	if len(value) == 0 || len(value) > 64 {
		return false, errors.New("signal measurement is invalid")
	}
	v, err := decimal.NewFromString(value)
	if err != nil || v.Exponent() < -24 || v.Exponent() > 12 || v.IsNegative() {
		return false, errors.New("signal measurement is invalid")
	}
	threshold, err := decimal.NewFromString(c.Threshold)
	if err != nil {
		return false, err
	}
	if !active {
		return v.Cmp(threshold) >= 0, nil
	}
	recovery, err := decimal.NewFromString(c.RecoveryThreshold)
	if err != nil {
		return false, err
	}
	return v.Cmp(recovery) > 0, nil
}
