package codeadapter

import (
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

type kimiOutcome struct {
	envelope string
	class    CooldownClass
	scope    CooldownScope
}

var kimiOutcomes = map[string]kimiOutcome{
	"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota": {
		envelope: "kimi_quota_five_hour", class: CooldownQuota, scope: ScopeCredential,
	},
	"You've reached your weekly (7-day) usage limit. Your quota will reset when the current 7-day window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota": {
		envelope: "kimi_quota_weekly", class: CooldownQuota, scope: ScopeCredential,
	},
	"You've reached your monthly usage limit for this billing cycle. Your quota will be refreshed in the next cycle. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota": {
		envelope: "kimi_quota_monthly", class: CooldownQuota, scope: ScopeCredential,
	},
	"Your credit balance is insufficient. To continue, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota": {
		envelope: "kimi_credit_insufficient", class: CooldownQuota, scope: ScopeCredential,
	},
	"Your organization's Extra Usage balance has run out. Please contact your organization admin to add more Extra Usage.": {
		envelope: "kimi_org_extra_usage_exhausted", class: CooldownQuota, scope: ScopeCredential,
	},
	"You've reached the Extra Usage limit set for your account by your organization. Please contact your organization admin to increase your limit.": {
		envelope: "kimi_org_extra_usage_limit", class: CooldownQuota, scope: ScopeCredential,
	},
	"You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again.": {
		envelope: "kimi_concurrent_limit", class: CooldownConcurrency, scope: ScopeCredential,
	},
	"We're receiving too many requests at the moment. Please wait a moment and try again.": {
		envelope: "kimi_too_many_requests", class: CooldownRate, scope: ScopeCredential,
	},
	"The engine is currently overloaded, please try again later": {
		envelope: "kimi_engine_overloaded", class: CooldownCapacity, scope: ScopeModel,
	},
}

var (
	kimiQuotaCodes    = []string{"exceeded_current_quota_error", "insufficient_quota"}
	kimiOverloadCodes = []string{"overloaded_error", "engine_overloaded_error"}
	kimiRateCodes     = []string{"rate_limit_exceeded", "rate_limit_reached_error", "rate_limit_error"}
)

func classifyKimi(endpoint Endpoint, status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	envelope, ok := extractKimiEnvelope(body)
	if !ok {
		return Classification{}, false
	}
	retryAt := retryAtFrom(time.Time{}, false, headers, now)
	fields, structured := envelope.structuredFields()
	if structured {
		if code, ok := kimiFieldCode(fields, kimiQuotaCodes); ok {
			return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: retryAt, Envelope: "kimi_" + code, NotExecuted: envelope.matchesEndpoint(endpoint)}, true
		}
		if code, ok := kimiFieldCode(fields, kimiOverloadCodes); ok {
			return Classification{Class: CooldownCapacity, Scope: ScopeModel, RetryAt: retryAt, Envelope: "kimi_" + code}, true
		}
	}
	if message, ok := envelope.message(); ok {
		if outcome, ok := kimiOutcomes[message]; ok {
			return Classification{Class: outcome.class, Scope: outcome.scope, RetryAt: retryAt, Envelope: outcome.envelope}, true
		}
	}
	if structured {
		if code, ok := kimiFieldCode(fields, kimiRateCodes); ok {
			return Classification{Class: CooldownRate, Scope: ScopeCredential, RetryAt: retryAt, Envelope: "kimi_" + code}, true
		}
	}
	return Classification{}, false
}

func kimiFieldCode(fields map[string]string, codes []string) (string, bool) {
	for _, name := range []string{"code", "type"} {
		if slices.Contains(codes, fields[name]) {
			return fields[name], true
		}
	}
	return "", false
}

type kimiEnvelope struct {
	isMessagesEnvelope bool
	members            []oif.Member
}

func extractKimiEnvelope(body []byte) (kimiEnvelope, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return kimiEnvelope{}, false
	}
	root := document.Root().Members()
	if len(root) != 1 && len(root) != 2 {
		return kimiEnvelope{}, false
	}
	seen := map[string]bool{}
	var failure oif.Value
	var haveFailure bool
	for _, member := range root {
		if seen[member.Name] {
			return kimiEnvelope{}, false
		}
		seen[member.Name] = true
		switch member.Name {
		case "error":
			failure, haveFailure = member.Value, true
		case "type":
			if len(root) != 2 {
				return kimiEnvelope{}, false
			}
			if text, ok := member.Value.Text(); !ok || text != "error" {
				return kimiEnvelope{}, false
			}
		default:
			return kimiEnvelope{}, false
		}
	}
	if !haveFailure || failure.Kind() != oif.Object {
		return kimiEnvelope{}, false
	}
	return kimiEnvelope{isMessagesEnvelope: len(root) == 2, members: failure.Members()}, true
}

func (e kimiEnvelope) matchesEndpoint(endpoint Endpoint) bool {
	if e.isMessagesEnvelope {
		return endpoint.Protocol == codemode.ProtocolMessages
	}
	return endpoint.Protocol == codemode.ProtocolChat
}

func (e kimiEnvelope) structuredFields() (map[string]string, bool) {
	fields := map[string]string{}
	for _, member := range e.members {
		if _, dup := fields[member.Name]; dup {
			return nil, false
		}
		text, ok := member.Value.Text()
		if !ok {
			return nil, false
		}
		fields[member.Name] = text
	}
	if fields["message"] == "" {
		return nil, false
	}
	if e.isMessagesEnvelope {
		if len(fields) != 2 {
			return nil, false
		}
		if _, ok := fields["type"]; !ok {
			return nil, false
		}
		return fields, true
	}
	for name := range fields {
		if name != "code" && name != "type" && name != "message" {
			return nil, false
		}
	}
	return fields, true
}

func (e kimiEnvelope) message() (string, bool) {
	var message string
	var found bool
	for _, member := range e.members {
		if member.Name != "message" {
			continue
		}
		if found {
			return "", false
		}
		text, ok := member.Value.Text()
		if !ok {
			return "", false
		}
		message, found = text, true
	}
	if !found || message == "" {
		return "", false
	}
	return message, true
}
