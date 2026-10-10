package notifications

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/shopspring/decimal"
)

var Events = []string{
	"budget.threshold", "provider.grant.lapsed", "key.expiring", "budget.exhausted",
	"provider.circuit.open", "provider.circuit.closed", "provider.error_rate", "route.latency",
	"provider.credential.failing", "model.retirement", "runtime.install_failed", "worker.stale", "report.spend",
}

func WorkerStaleAfter(task string) int64 {
	switch task {
	case "request_metadata_consumer", "request_metadata_gateway_epoch_detection", "media_reconciliation", "grant_refresh":
		return 20
	case "maintenance", "cost_reconciliation", "notification_delivery", "health_probes":
		return 180
	case "export_delivery":
		return 30
	}
	return 0
}

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

func ParseRuleConfiguration(event string, raw json.RawMessage) (RuleConfiguration, error) {
	var c RuleConfiguration
	if !slices.Contains(Events, event) {
		return c, errors.New("unsupported notification event")
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return c, errors.New("configuration must be an object")
	}
	allowed := []string{}
	switch event {
	case "provider.error_rate", "route.latency", "provider.credential.failing":
		allowed = []string{"window_seconds", "minimum_samples", "threshold", "recovery_threshold", "cooldown_seconds"}
		if event == "route.latency" {
			allowed = append(allowed, "metric")
		}
	case "key.expiring":
		allowed = []string{"lead_time_seconds"}
	case "model.retirement":
		allowed = []string{"lead_days", "cooldown_seconds"}
	case "report.spend":
		allowed = []string{"period"}
	case "budget.exhausted", "provider.circuit.open", "provider.circuit.closed", "runtime.install_failed", "worker.stale":
		allowed = []string{"cooldown_seconds"}
	}
	for field := range fields {
		if !slices.Contains(allowed, field) {
			return c, fmt.Errorf("configuration.%s is not supported for %s", field, event)
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("configuration fields have invalid types")
	}
	for key, bounds := range map[string][3]int{
		"window_seconds":    {60, 86400, 300},
		"minimum_samples":   {1, 10000, 20},
		"cooldown_seconds":  {60, 86400, 900},
		"lead_time_seconds": {60, 2678400, 86400},
		"lead_days":         {1, 366, 30},
	} {
		if !slices.Contains(allowed, key) {
			continue
		}
		value := 0
		switch key {
		case "window_seconds":
			value = c.WindowSeconds
		case "minimum_samples":
			value = c.MinimumSamples
		case "cooldown_seconds":
			value = c.CooldownSeconds
		case "lead_time_seconds":
			value = c.LeadTimeSeconds
		case "lead_days":
			value = c.LeadDays
		}
		if _, provided := fields[key]; !provided {
			value = bounds[2]
			if event == "provider.credential.failing" && key == "minimum_samples" {
				value = 3
			}
		}
		if value < bounds[0] || value > bounds[1] {
			return c, fmt.Errorf("configuration.%s must be from %d to %d", key, bounds[0], bounds[1])
		}
		switch key {
		case "window_seconds":
			c.WindowSeconds = value
		case "minimum_samples":
			c.MinimumSamples = value
		case "cooldown_seconds":
			c.CooldownSeconds = value
		case "lead_time_seconds":
			c.LeadTimeSeconds = value
		case "lead_days":
			c.LeadDays = value
		}
	}
	if slices.Contains(allowed, "threshold") {
		defaults := map[string][2]string{"provider.error_rate": {"0.1", "0.05"}, "route.latency": {"1000", "800"}, "provider.credential.failing": {"3", "0"}}
		if _, provided := fields["threshold"]; !provided {
			c.Threshold = defaults[event][0]
		}
		if _, provided := fields["recovery_threshold"]; !provided {
			if _, custom := fields["threshold"]; custom {
				threshold, err := boundedThreshold(event, c.Threshold)
				if err != nil {
					return c, err
				}
				c.RecoveryThreshold = threshold.Mul(decimal.RequireFromString("0.8")).String()
			} else {
				c.RecoveryThreshold = defaults[event][1]
			}
		}
		threshold, err := boundedThreshold(event, c.Threshold)
		if err != nil {
			return c, err
		}
		recovery, err := decimal.NewFromString(c.RecoveryThreshold)
		if err != nil || len(c.RecoveryThreshold) > 32 || recovery.Exponent() < -12 || recovery.Exponent() > 12 || recovery.IsNegative() || recovery.Cmp(threshold) >= 0 {
			return c, errors.New("recovery_threshold must be a nonnegative decimal below threshold")
		}
	}
	if event == "route.latency" {
		if _, provided := fields["metric"]; !provided {
			c.Metric = "latency"
		}
		if c.Metric != "latency" && c.Metric != "ttft" {
			return c, errors.New("metric must be latency or ttft")
		}
	}
	if event == "report.spend" {
		if _, provided := fields["period"]; !provided {
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
