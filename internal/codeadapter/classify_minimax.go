package codeadapter

import (
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
)

func classifyMiniMax(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	code, ok := minimaxEnvelope(body)
	if !ok {
		return Classification{}, false
	}
	switch code {
	case "2056", "2067":
		return Classification{Class: CooldownQuota, Scope: ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_" + code}, true
	case "2061":
		return Classification{Class: CooldownEntitlement, Scope: ScopeModel, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_2061"}, true
	case "2062", "1002", "1039":
		return Classification{Class: CooldownRate, Scope: ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_" + code}, true
	}
	return Classification{}, false
}

func minimaxEnvelope(body []byte) (string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", false
	}
	root := document.Root().Members()
	if len(root) != 3 {
		return "", false
	}
	var failure oif.Value
	var haveType, haveError, haveRequest bool
	for _, member := range root {
		switch member.Name {
		case "type":
			text, ok := member.Value.Text()
			if !ok || text != "error" {
				return "", false
			}
			haveType = true
		case "error":
			if member.Value.Kind() != oif.Object {
				return "", false
			}
			failure, haveError = member.Value, true
		case "request_id":
			if _, ok := member.Value.Text(); !ok {
				return "", false
			}
			haveRequest = true
		default:
			return "", false
		}
	}
	if !haveType || !haveError || !haveRequest {
		return "", false
	}
	members := failure.Members()
	if len(members) != 3 {
		return "", false
	}
	var typeName, message, httpCode string
	var haveTypeName, haveMessage, haveHTTPCode bool
	for _, member := range members {
		text, ok := member.Value.Text()
		if !ok {
			return "", false
		}
		switch member.Name {
		case "type":
			typeName, haveTypeName = text, true
		case "message":
			message, haveMessage = text, true
		case "http_code":
			httpCode, haveHTTPCode = text, true
		default:
			return "", false
		}
	}
	if !haveTypeName || !haveMessage || !haveHTTPCode || typeName == "" || httpCode != "429" {
		return "", false
	}
	return minimaxCode(message)
}

func minimaxCode(message string) (string, bool) {
	open := strings.LastIndex(message, " (")
	if open <= 0 || !strings.HasSuffix(message, ")") {
		return "", false
	}
	code := message[open+2 : len(message)-1]
	if code == "" {
		return "", false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return code, true
}
