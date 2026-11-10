package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const (
	// SourceOpenAI costs come from the organization's admin costs endpoint.
	SourceOpenAI = "openai_costs"
	// SourceAnthropic costs come from the organization's cost report.
	SourceAnthropic           = "anthropic_cost_report"
	SourceAzureCostManagement = "azure_cost_management"
	SourceAWS                 = "aws_cur"
	SourceGoogle              = "google_bigquery"
)

type periodDays struct {
	start    string
	revision *sourceRevision
	days     reportAccumulator
	replaces *dayWindow
}

type dayWindow struct {
	from time.Time
	to   time.Time
}

func (p periodDays) coverage(days []string) (from, through *string) {
	if p.replaces != nil {
		if !p.replaces.from.Before(p.replaces.to) {
			return nil, nil
		}
		first, last := p.replaces.from.Format(dayFormat), p.replaces.to.AddDate(0, 0, -1).Format(dayFormat)
		return &first, &last
	}
	if len(days) == 0 {
		return nil, nil
	}
	return &days[0], &days[len(days)-1]
}

type fetchInput struct {
	importer *Importer
	row      *importRow
	cfg      *providerConfig
	key      []byte
}

type costSource struct {
	name             string
	kind             string
	validateScope    func(json.RawMessage) (json.RawMessage, error)
	encodeCredential func(string) ([]byte, error)
	decodeCredential func([]byte) ([]byte, error)
	credentialInput  func(json.RawMessage) (string, error)
	fetch            func(context.Context, *fetchInput, func(periodDays) error) error
}

func costSources() []costSource {
	return []costSource{openAICostSource(), anthropicCostSource(), azureCostSource(), awsCostSource(), googleCostSource()}
}

func lookupSource(name string) (costSource, bool) {
	for _, source := range costSources() {
		if source.name == name {
			return source, true
		}
	}
	return costSource{}, false
}

// SourceFor names the vendor cost endpoint a provider kind imports, or
// refuses a kind that has none.
func SourceFor(kind string) (string, error) {
	for _, source := range costSources() {
		if source.kind == kind {
			return source.name, nil
		}
	}
	return "", access.Invalid("billing_import",
		fmt.Sprintf("Billing imports are not supported for provider kind %q.", kind))
}

// ValidateScope checks a scope document against its source's vocabulary and
// returns the canonical document stored. Anything the document does not list
// is rejected rather than silently narrowed.
func ValidateScope(source string, raw json.RawMessage) (json.RawMessage, error) {
	adapter, ok := lookupSource(source)
	if !ok {
		return nil, access.Invalid("scope", "unknown billing source")
	}
	return adapter.validateScope(raw)
}

func encodeCredential(source, key string) ([]byte, error) {
	adapter, ok := lookupSource(source)
	if !ok {
		return nil, access.Invalid("credential", "unknown billing source")
	}
	return adapter.encodeCredential(key)
}

func decodeCredential(source string, data []byte) ([]byte, error) {
	adapter, ok := lookupSource(source)
	if !ok {
		return nil, errIncompatibleCredential
	}
	return adapter.decodeCredential(data)
}

func readCredentialInput(source string, raw json.RawMessage) (string, error) {
	adapter, ok := lookupSource(source)
	if !ok {
		return "", access.Invalid("credential", "unknown billing source")
	}
	return adapter.credentialInput(raw)
}
