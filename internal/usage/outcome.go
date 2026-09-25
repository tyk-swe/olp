package usage

import (
	"fmt"
	"slices"
)

// OutcomeEvidence carries the outcome facts one attempt established,
// independently of transport, content or billing facts: the provider-declared
// native terminal status, the attributed fault taxonomy, and the bounded
// proxy-local limit the attempt exhausted. Every member serializes explicitly
// as null when it was not recorded, so a reader distinguishes a recorded
// absence from a record that predates outcome facts — the whole value is
// omitted only then.
type OutcomeEvidence struct {
	NativeStatus  *string `json:"native_status"`
	FaultOrigin   *string `json:"fault_origin"`
	FaultScope    *string `json:"fault_scope"`
	FaultResource *string `json:"fault_resource"`
	LimitCategory *string `json:"limit_category"`
	Limit         *int64  `json:"limit"`
}

// The provider-declared native terminal statuses the gateway normalizes
// provider results into before recording them. The set is closed on purpose:
// a dialect translates its provider's own vocabulary at admission, so stored
// evidence never carries a wire spelling a reader cannot classify.
var nativeStatuses = []string{"completed", "incomplete", "failed", "cancelled"}

// The fault origins and scopes a stored attempt may attribute. Proxy-local
// origins name this gateway's own limits and persistence, never provider
// health.
var faultOrigins = []string{
	"native_outcome",
	"provider_transport",
	"provider_declared",
	"client_delivery",
	"proxy_capacity",
	"proxy_persistence",
	"proxy_policy",
	"contract",
}
var faultScopes = []string{"endpoint", "credential", "contract", "request"}

// The bounded proxy-local limit categories a stored attempt may name; an
// exhaustion always reports its resource, category and configured limit.
var limitCategories = []string{"bytes", "event_work", "time", "persistence"}

func checkLabel(field string, value *string, allowed []string) error {
	if value != nil && !slices.Contains(allowed, *value) {
		return fmt.Errorf("routing.outcome.%s %q is not a recorded value", field, *value)
	}
	return nil
}

// validate refuses outcome evidence outside the recorded vocabularies, so a
// corrupt or foreign write is dropped at ingest instead of reaching a reader
// as contract-clean JSON.
func (o *OutcomeEvidence) validate() error {
	if o == nil {
		return nil
	}
	checks := []struct {
		field   string
		value   *string
		allowed []string
	}{
		{"native_status", o.NativeStatus, nativeStatuses},
		{"fault_origin", o.FaultOrigin, faultOrigins},
		{"fault_scope", o.FaultScope, faultScopes},
		{"limit_category", o.LimitCategory, limitCategories},
	}
	for _, check := range checks {
		if err := checkLabel(check.field, check.value, check.allowed); err != nil {
			return err
		}
	}
	if o.Limit != nil && *o.Limit < 0 {
		return fmt.Errorf("routing.outcome.limit is negative")
	}
	return nil
}
