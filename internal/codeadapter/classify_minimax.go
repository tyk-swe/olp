package codeadapter

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
)

func classifyMiniMax(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	code, ok := minimaxBusinessCode(body)
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

func minimaxBusinessCode(body []byte) (string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", false
	}
	if code, ok := minimaxDeclaredCode(document.Root()); ok {
		return code, true
	}
	return minimaxEnvelope(document.Root())
}

func minimaxDeclaredCode(root oif.Value) (string, bool) {
	var baseResp, failure oif.Value
	var haveBaseResp, haveFailure bool
	for _, member := range root.Members() {
		switch member.Name {
		case "base_resp":
			if haveBaseResp {
				return "", false
			}
			baseResp, haveBaseResp = member.Value, true
		case "error":
			if haveFailure {
				return "", false
			}
			failure, haveFailure = member.Value, true
		}
	}
	if !haveBaseResp && !haveFailure {
		return "", false
	}
	var statusCode int64
	if haveBaseResp {
		code, ok := minimaxBaseRespCode(baseResp)
		if !ok {
			return "", false
		}
		statusCode = code
	}
	var errorCode int64
	var haveErrorCode bool
	if haveFailure {
		code, present, ok := minimaxErrorCode(failure)
		if !ok {
			return "", false
		}
		errorCode, haveErrorCode = code, present
	}
	if statusCode != 0 {
		return strconv.FormatInt(statusCode, 10), true
	}
	if haveErrorCode && errorCode != 0 {
		return strconv.FormatInt(errorCode, 10), true
	}
	return "", false
}

func minimaxBaseRespCode(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Object {
		return 0, false
	}
	var code int64
	var haveCode, haveMessage bool
	for _, member := range value.Members() {
		switch member.Name {
		case "status_code":
			if haveCode {
				return 0, false
			}
			parsed, ok := minimaxNumber(member.Value)
			if !ok {
				return 0, false
			}
			code, haveCode = parsed, true
		case "status_msg":
			if haveMessage {
				return 0, false
			}
			if _, ok := member.Value.Text(); !ok {
				return 0, false
			}
			haveMessage = true
		}
	}
	if !haveCode || !haveMessage {
		return 0, false
	}
	return code, true
}

func minimaxErrorCode(value oif.Value) (int64, bool, bool) {
	if value.Kind() != oif.Object {
		return 0, false, false
	}
	var code int64
	var haveCode, haveMessage, haveType, haveHTTPCode bool
	for _, member := range value.Members() {
		switch member.Name {
		case "code":
			if haveCode {
				return 0, false, false
			}
			parsed, ok := minimaxNumber(member.Value)
			if !ok {
				return 0, false, false
			}
			code, haveCode = parsed, true
		case "message":
			if haveMessage {
				return 0, false, false
			}
			if _, ok := member.Value.Text(); !ok {
				return 0, false, false
			}
			haveMessage = true
		case "type":
			if haveType {
				return 0, false, false
			}
			if _, ok := member.Value.Text(); !ok {
				return 0, false, false
			}
			haveType = true
		case "http_code":
			if haveHTTPCode {
				return 0, false, false
			}
			if _, ok := member.Value.Text(); !ok && member.Value.Kind() != oif.Number {
				return 0, false, false
			}
			haveHTTPCode = true
		}
	}
	return code, haveCode, true
}

func minimaxNumber(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Number {
		return 0, false
	}
	parsed, err := strconv.ParseInt(value.Raw(), 10, 64)
	return parsed, err == nil
}

func minimaxEnvelope(root oif.Value) (string, bool) {
	members := root.Members()
	if len(members) != 3 {
		return "", false
	}
	var failure oif.Value
	var haveType, haveError, haveRequest bool
	for _, member := range members {
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
	fields := failure.Members()
	if len(fields) != 3 {
		return "", false
	}
	var typeName, message, httpCode string
	var haveTypeName, haveMessage, haveHTTPCode bool
	for _, member := range fields {
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
