package vendor

import (
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

func classifyGLM(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	errCode, message, ok := glmEnvelope(body)
	if !ok {
		return Classification{}, false
	}
	switch errCode {
	case "1308", "1310", "1316", "1317", "1318", "1319", "1320", "1321":
		reset, hasReset := glmReset(errCode, message)
		return Classification{Class: code.CooldownQuota, Scope: code.ScopeCredential, RetryAt: retryAtFrom(reset, hasReset, headers, now), Envelope: "glm_" + errCode}, true
	case "1311":
		return Classification{Class: code.CooldownEntitlement, Scope: code.ScopeModel, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "glm_1311"}, true
	case "1302":
		return Classification{Class: code.CooldownRate, Scope: code.ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "glm_1302"}, true
	case "1305":
		return Classification{Class: code.CooldownCapacity, Scope: code.ScopeModel, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "glm_1305"}, true
	case "1309", "1314":
		return Classification{SuppressFallback: true, Envelope: "glm_" + errCode}, true
	}
	return Classification{}, false
}

func glmEnvelope(body []byte) (string, string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", "", false
	}
	root := document.Root().Members()
	if len(root) != 1 || root[0].Name != "error" || root[0].Value.Kind() != oif.Object {
		return "", "", false
	}
	members := root[0].Value.Members()
	if len(members) != 2 {
		return "", "", false
	}
	var errCode, message string
	var haveCode, haveMessage bool
	for _, member := range members {
		text, ok := member.Value.Text()
		if !ok {
			return "", "", false
		}
		switch member.Name {
		case "code":
			errCode, haveCode = text, true
		case "message":
			message, haveMessage = text, true
		default:
			return "", "", false
		}
	}
	if !haveCode || !haveMessage || errCode == "" || message == "" {
		return "", "", false
	}
	return errCode, message, true
}

func glmReset(errCode, message string) (time.Time, bool) {
	if errCode == "1308" {
		return glmReset1308(message)
	}
	prefix, suffix, ok := glmResetTemplate(errCode)
	if !ok {
		return time.Time{}, false
	}
	token, found := strings.CutPrefix(message, prefix)
	if !found {
		return time.Time{}, false
	}
	token, found = strings.CutSuffix(token, suffix)
	if !found {
		return time.Time{}, false
	}
	return glmTimestamp(token)
}

func glmResetTemplate(errCode string) (string, string, bool) {
	switch errCode {
	case "1310":
		return "Weekly/Monthly Limit Exhausted. Your limit will reset at ", "", true
	case "1316":
		return "Usage limit reached for the past 5 hours. Insufficient balance for extra usage. Resets at ", ".", true
	case "1317":
		return "Usage limit reached for the past 7 days. Insufficient balance for extra usage. Resets at ", ".", true
	case "1318", "1320":
		return "Usage limit reached for the past 5 hours. Extra usage is not available due to monthly spend limit. Resets at ", ".", true
	case "1319", "1321":
		return "Usage limit reached for the past 7 days. Extra usage is not available due to monthly spend limit. Resets at ", ".", true
	}
	return "", "", false
}

func glmReset1308(message string) (time.Time, bool) {
	rest, found := strings.CutPrefix(message, "Usage limit reached for ")
	if !found {
		return time.Time{}, false
	}
	middle, token, found := strings.Cut(rest, ". Your limit will reset at ")
	if !found || !glmNumberUnit(middle) {
		return time.Time{}, false
	}
	return glmTimestamp(token)
}

func glmNumberUnit(middle string) bool {
	parts := strings.Fields(middle)
	if len(parts) != 2 {
		return false
	}
	number := glmUntick(parts[0])
	unit := glmUntick(parts[1])
	if number == "" || unit == "" {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range unit {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func glmTimestamp(token string) (time.Time, bool) {
	unticked := glmUntick(token)
	if unticked == "" || strings.ContainsAny(unticked, " \t") {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, unticked)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

func glmUntick(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`") {
		return value[1 : len(value)-1]
	}
	return value
}
