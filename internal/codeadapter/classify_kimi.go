package codeadapter

import (
	"net/http"
	"time"

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

var kimiQuotaCodes = map[string]string{
	"exceeded_current_quota_error": "kimi_exceeded_current_quota_error",
	"insufficient_quota":           "kimi_insufficient_quota",
}

func classifyKimi(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	if envelope, ok := kimiStructuredQuota(body); ok {
		return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: envelope, NotExecuted: true}, true
	}
	message, ok := kimiEnvelope(body)
	if !ok {
		return Classification{}, false
	}
	outcome, ok := kimiOutcomes[message]
	if !ok {
		return Classification{}, false
	}
	return Classification{Class: outcome.class, Scope: outcome.scope, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: outcome.envelope}, true
}

func kimiStructuredQuota(body []byte) (string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", false
	}
	root := document.Root().Members()
	var failure oif.Value
	messages := false
	switch len(root) {
	case 1:
		if root[0].Name != "error" {
			return "", false
		}
		failure = root[0].Value
	case 2:
		seen := map[string]bool{}
		for _, member := range root {
			if seen[member.Name] {
				return "", false
			}
			seen[member.Name] = true
			switch member.Name {
			case "type":
				if text, ok := member.Value.Text(); !ok || text != "error" {
					return "", false
				}
			case "error":
				failure = member.Value
			default:
				return "", false
			}
		}
		messages = true
	default:
		return "", false
	}
	if failure.Kind() != oif.Object {
		return "", false
	}
	fields := map[string]string{}
	for _, member := range failure.Members() {
		if _, dup := fields[member.Name]; dup {
			return "", false
		}
		text, ok := member.Value.Text()
		if !ok {
			return "", false
		}
		fields[member.Name] = text
	}
	if fields["message"] == "" {
		return "", false
	}
	if messages {
		if len(fields) != 2 {
			return "", false
		}
		envelope, ok := kimiQuotaCodes[fields["type"]]
		return envelope, ok
	}
	for name := range fields {
		if name != "code" && name != "type" && name != "message" {
			return "", false
		}
	}
	if envelope, ok := kimiQuotaCodes[fields["code"]]; ok {
		return envelope, true
	}
	envelope, ok := kimiQuotaCodes[fields["type"]]
	return envelope, ok
}

func kimiEnvelope(body []byte) (string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", false
	}
	root := document.Root().Members()
	var failure oif.Value
	switch len(root) {
	case 1:
		if root[0].Name != "error" {
			return "", false
		}
		failure = root[0].Value
	case 2:
		for _, member := range root {
			switch member.Name {
			case "type":
				if text, ok := member.Value.Text(); !ok || text != "error" {
					return "", false
				}
			case "error":
				failure = member.Value
			default:
				return "", false
			}
		}
	default:
		return "", false
	}
	if failure.Kind() != oif.Object {
		return "", false
	}
	var message string
	var found bool
	for _, member := range failure.Members() {
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
