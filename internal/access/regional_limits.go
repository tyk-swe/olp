package access

import (
	"bytes"
	"encoding/json"
	"errors"
)

// RegionalRateLimits override the key's default rate and concurrency limits.
// Cost accounts stay global; project templates still impose ceilings.
type RegionalRateLimits struct {
	RequestsPerMinute *int64 `json:"requests_per_minute,omitempty"`
	TokensPerMinute   *int64 `json:"tokens_per_minute,omitempty"`
	MaxConcurrency    *int64 `json:"max_concurrency,omitempty"`
}

func (limits *RegionalRateLimits) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("regional limits must be an object")
	}
	type fields RegionalRateLimits
	var value fields
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&value); err != nil {
		return err
	}
	*limits = RegionalRateLimits(value)
	return nil
}

type RegionalLimits map[string]RegionalRateLimits

func (regions RegionalLimits) Validate() error {
	if len(regions) > 64 {
		return Invalid("regional_limits", "Use at most 64 regions.")
	}
	for region, policy := range regions {
		if !RouteSlug.MatchString(region) {
			return Invalid("regional_limits", "Use valid region names.")
		}
		if err := (AdmissionLimits{RequestsPerMinute: policy.RequestsPerMinute, TokensPerMinute: policy.TokensPerMinute, MaxConcurrency: policy.MaxConcurrency}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

// InRegion never changes the original policy or its override map. Missing
// fields inherit the complete default regional limit, without auto-division.
func (policy KeyPolicy) InRegion(region string) KeyPolicy {
	if region == "" {
		return policy
	}
	if limits, ok := policy.RegionalLimits[region]; ok {
		if limits.RequestsPerMinute != nil {
			policy.RequestsPerMinute = limits.RequestsPerMinute
		}
		if limits.TokensPerMinute != nil {
			policy.TokensPerMinute = limits.TokensPerMinute
		}
		if limits.MaxConcurrency != nil {
			policy.MaxConcurrency = limits.MaxConcurrency
		}
	}
	return policy
}
