package connectors

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/vendors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// declaredClasses maps the failure classes a plugin profile declares to the
// classes that govern failover and cooldown.
var declaredClasses = map[string]upstream.Class{
	abi.ClassCredential:  upstream.Credential,
	abi.ClassRateLimited: upstream.RateLimit,
	abi.ClassRetryable:   upstream.ServerError,
	abi.ClassTerminal:    upstream.ClientError,
}

// parseClassification reads a hosting adaptation's failure classification.
func parseClassification(declared []abi.FailureRule) ([]upstream.Rule, error) {
	if len(declared) > 32 {
		return nil, &ProfileError{Field: "hosting.classification", Message: "Declare at most 32 rules."}
	}
	var rules []upstream.Rule
	for i, rule := range declared {
		field := fmt.Sprintf("hosting.classification[%d]", i)
		class, known := declaredClasses[rule.Class]
		switch {
		case rule.Status == 0 && rule.Code == "" && rule.Type == "":
			return nil, &ProfileError{Field: field, Message: "Match a status, an error code or an error type."}
		case rule.Status != 0 && (rule.Status < 400 || rule.Status > 599):
			return nil, &ProfileError{Field: field + ".status", Message: "Match an unsuccessful status from 400 to 599."}
		case !errorValue(rule.Code):
			return nil, &ProfileError{Field: field + ".code", Message: "Match an error code of at most 256 characters without control characters."}
		case !errorValue(rule.Type):
			return nil, &ProfileError{Field: field + ".type", Message: "Match an error type of at most 256 characters without control characters."}
		case !known:
			return nil, &ProfileError{Field: field + ".class", Message: "Classify the failures as credential, rate_limited, retryable or terminal."}
		}
		rules = append(rules, upstream.Rule{Status: rule.Status, Code: rule.Code, Type: rule.Type, Class: class})
	}
	return rules, nil
}

func errorValue(text string) bool {
	return len(text) <= 256 && !strings.ContainsFunc(text, unicode.IsControl)
}

// Classification is the failure classification the connector's profile
// declares, which the upstream classifier consults ahead of its built-in
// rules. Only plugin profiles declare one.
func (c Config) Classification() []upstream.Rule {
	var rules []upstream.Rule
	if c.Plugin != nil {
		rules = slices.Clone(c.Plugin.hosting.classification)
	}
	// A reviewed vendor classifies its own misleading statuses.
	for _, rule := range vendors.ErrorClassesFor(c.VendorID) {
		rules = append(rules, upstream.Rule{Status: rule.Status, Type: rule.Type, Class: upstream.Class(rule.Class)})
	}
	return rules
}
