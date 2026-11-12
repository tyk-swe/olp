package notifications

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/shopspring/decimal"
)

type noConfig struct{}

type cooldownConfig struct {
	CooldownSeconds int `json:"cooldown_seconds"`
}

type leadTimeConfig struct {
	LeadTimeSeconds int `json:"lead_time_seconds"`
}

type retirementConfig struct {
	LeadDays        int `json:"lead_days"`
	CooldownSeconds int `json:"cooldown_seconds"`
}

type reportPeriod string

const (
	dailyReport   reportPeriod = "daily"
	weeklyReport  reportPeriod = "weekly"
	monthlyReport reportPeriod = "monthly"
)

func (p reportPeriod) unit() string {
	switch p {
	case weeklyReport:
		return "week"
	case monthlyReport:
		return "month"
	default:
		return "day"
	}
}

type spendReportConfig struct {
	Period reportPeriod `json:"period"`
}

type band struct {
	Trigger  decimal.Decimal `json:"threshold"`
	Recovery decimal.Decimal `json:"recovery_threshold"`
}

func (b band) breached(value decimal.Decimal, active bool) bool {
	if active {
		return value.GreaterThan(b.Recovery)
	}
	return !value.LessThan(b.Trigger)
}

type measuredConfig struct {
	WindowSeconds  int `json:"window_seconds"`
	MinimumSamples int `json:"minimum_samples"`
	band
	CooldownSeconds int `json:"cooldown_seconds"`
}

type latencyMetric string

const (
	totalLatency      latencyMetric = "latency"
	firstTokenLatency latencyMetric = "ttft"
)

type latencyConfig struct {
	measuredConfig
	Metric latencyMetric `json:"metric"`
}

type intField struct {
	key                 string
	fallback, low, high int
}

var (
	windowField   = intField{"window_seconds", 300, 60, 86400}
	cooldownField = intField{"cooldown_seconds", 900, 60, 86400}
	leadTimeField = intField{"lead_time_seconds", 86400, 60, 2678400}
	leadDaysField = intField{"lead_days", 30, 1, 366}
	measuredKeys  = []string{windowField.key, "minimum_samples", "threshold", "recovery_threshold", cooldownField.key}
)

func (f intField) read(fields map[string]json.RawMessage) (int, error) {
	raw, provided := fields[f.key]
	if !provided {
		return f.fallback, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < f.low || value > f.high {
		return 0, fmt.Errorf("configuration.%s must be an integer from %d to %d", f.key, f.low, f.high)
	}
	return value, nil
}

func stringField(fields map[string]json.RawMessage, key, fallback string) (string, error) {
	raw, provided := fields[key]
	if !provided {
		return fallback, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("configuration.%s must be a string", key)
	}
	return value, nil
}

func choice[T ~string](fields map[string]json.RawMessage, key string, fallback T, others ...T) (T, error) {
	value, err := stringField(fields, key, string(fallback))
	if err != nil || T(value) != fallback && !slices.Contains(others, T(value)) {
		return fallback, fmt.Errorf("configuration.%s is not a supported choice", key)
	}
	return T(value), nil
}

func boundedDecimal(raw string, maxLength int, minExponent int32) (decimal.Decimal, error) {
	if len(raw) > maxLength {
		return decimal.Decimal{}, errors.New("decimal string is too long")
	}
	value, err := decimal.NewFromString(raw)
	if err != nil || value.IsNegative() || value.Exponent() < minExponent || value.Exponent() > 12 {
		return decimal.Decimal{}, errors.New("not a nonnegative decimal string within bounds")
	}
	return value, nil
}

func resolveNothing(raw json.RawMessage) (noConfig, error) {
	_, err := objectFields(raw)
	return noConfig{}, err
}

func resolveCooldown(raw json.RawMessage) (cooldownConfig, error) {
	fields, err := objectFields(raw, cooldownField.key)
	if err != nil {
		return cooldownConfig{}, err
	}
	cooldown, err := cooldownField.read(fields)
	return cooldownConfig{CooldownSeconds: cooldown}, err
}

func resolveLeadTime(raw json.RawMessage) (leadTimeConfig, error) {
	fields, err := objectFields(raw, leadTimeField.key)
	if err != nil {
		return leadTimeConfig{}, err
	}
	leadTime, err := leadTimeField.read(fields)
	return leadTimeConfig{LeadTimeSeconds: leadTime}, err
}

func resolveRetirement(raw json.RawMessage) (retirementConfig, error) {
	fields, err := objectFields(raw, leadDaysField.key, cooldownField.key)
	if err != nil {
		return retirementConfig{}, err
	}
	days, daysErr := leadDaysField.read(fields)
	cooldown, cooldownErr := cooldownField.read(fields)
	return retirementConfig{LeadDays: days, CooldownSeconds: cooldown}, errors.Join(daysErr, cooldownErr)
}

func resolveSpendReport(raw json.RawMessage) (spendReportConfig, error) {
	fields, err := objectFields(raw, "period")
	if err != nil {
		return spendReportConfig{}, err
	}
	period, err := choice(fields, "period", dailyReport, weeklyReport, monthlyReport)
	return spendReportConfig{Period: period}, err
}

func resolveLatency(raw json.RawMessage) (latencyConfig, error) {
	fields, err := objectFields(raw, slices.Concat(measuredKeys, []string{"metric"})...)
	if err != nil {
		return latencyConfig{}, err
	}
	measured, measuredErr := latencyDefaults.read(fields)
	metric, metricErr := choice(fields, "metric", totalLatency, firstTokenLatency)
	return latencyConfig{measuredConfig: measured, Metric: metric}, errors.Join(measuredErr, metricErr)
}

type measuredDefaults struct {
	trigger, recovery string
	minimumSamples    int
	ceiling           decimal.Decimal
	integral          bool
}

var (
	errorRateDefaults  = measuredDefaults{trigger: "0.1", recovery: "0.05", minimumSamples: 20, ceiling: decimal.NewFromInt(1)}
	credentialDefaults = measuredDefaults{trigger: "3", recovery: "0", minimumSamples: 3, ceiling: decimal.NewFromInt(10000), integral: true}
	latencyDefaults    = measuredDefaults{trigger: "1000", recovery: "800", minimumSamples: 20, ceiling: decimal.NewFromInt(1000000000)}
)

func (d measuredDefaults) resolve(raw json.RawMessage) (measuredConfig, error) {
	fields, err := objectFields(raw, measuredKeys...)
	if err != nil {
		return measuredConfig{}, err
	}
	return d.read(fields)
}

func (d measuredDefaults) read(fields map[string]json.RawMessage) (measuredConfig, error) {
	window, windowErr := windowField.read(fields)
	samples, samplesErr := intField{"minimum_samples", d.minimumSamples, 1, 10000}.read(fields)
	cooldown, cooldownErr := cooldownField.read(fields)
	thresholds, bandErr := d.band(fields)
	return measuredConfig{WindowSeconds: window, MinimumSamples: samples, band: thresholds, CooldownSeconds: cooldown},
		errors.Join(windowErr, samplesErr, cooldownErr, bandErr)
}

func (d measuredDefaults) band(fields map[string]json.RawMessage) (band, error) {
	triggerText, err := stringField(fields, "threshold", d.trigger)
	if err != nil {
		return band{}, err
	}
	trigger, err := boundedDecimal(triggerText, 32, -12)
	if err != nil || !trigger.IsPositive() || trigger.GreaterThan(d.ceiling) || d.integral && !trigger.IsInteger() {
		return band{}, fmt.Errorf("configuration.threshold must be a positive decimal string up to %s", d.ceiling)
	}
	recoveryText, err := stringField(fields, "recovery_threshold", d.recoveryFallback(fields, trigger))
	if err != nil {
		return band{}, err
	}
	recovery, err := boundedDecimal(recoveryText, 32, -12)
	if err != nil || !recovery.LessThan(trigger) {
		return band{}, errors.New("configuration.recovery_threshold must be a nonnegative decimal string below threshold")
	}
	return band{Trigger: trigger, Recovery: recovery}, nil
}

func (d measuredDefaults) recoveryFallback(fields map[string]json.RawMessage, trigger decimal.Decimal) string {
	if _, custom := fields["threshold"]; custom {
		return trigger.Mul(decimal.New(8, -1)).String()
	}
	return d.recovery
}
