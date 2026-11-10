package vendor

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

func classifyMiniMax(status int, body []byte, headers http.Header, now time.Time) (Classification, bool) {
	if status != 429 {
		return Classification{}, false
	}
	errCode, ok := minimaxBusinessCode(body)
	if !ok {
		return Classification{}, false
	}
	switch errCode {
	case "2056", "2067":
		return Classification{Class: code.CooldownQuota, Scope: code.ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_" + errCode}, true
	case "2061":
		return Classification{Class: code.CooldownEntitlement, Scope: code.ScopeModel, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_2061"}, true
	case "2062", "1002", "1039":
		return Classification{Class: code.CooldownRate, Scope: code.ScopeCredential, RetryAt: retryAtFrom(time.Time{}, false, headers, now), Envelope: "minimax_" + errCode}, true
	}
	return Classification{}, false
}

func minimaxBusinessCode(body []byte) (string, bool) {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: 64 << 10})
	if err != nil || document.Root().Kind() != oif.Object {
		return "", false
	}
	if errCode, ok := minimaxDeclaredCode(document.Root()); ok {
		return errCode, true
	}
	return minimaxEnvelope(document.Root())
}

func minimaxFoldCodes(statusCode int64, haveStatusCode bool, errorCode int64, haveErrorCode bool) (string, bool) {
	if haveStatusCode && statusCode != 0 {
		return strconv.FormatInt(statusCode, 10), true
	}
	if haveErrorCode && errorCode != 0 {
		return strconv.FormatInt(errorCode, 10), true
	}
	return "", false
}

func minimaxDeclaredCode(root oif.Value) (string, bool) {
	var baseResp, failure oif.Value
	var haveBaseResp, haveFailure, basePoisoned, failurePoisoned bool
	for _, member := range root.Members() {
		switch member.Name {
		case "base_resp":
			if haveBaseResp {
				basePoisoned = true
				continue
			}
			baseResp, haveBaseResp = member.Value, true
		case "error":
			if haveFailure {
				failurePoisoned = true
				continue
			}
			failure, haveFailure = member.Value, true
		}
	}
	var statusCode int64
	var haveStatusCode bool
	if haveBaseResp && !basePoisoned {
		statusCode, haveStatusCode = minimaxBaseRespCode(baseResp)
	}
	var errorCode int64
	var haveErrorCode bool
	if haveFailure && !failurePoisoned {
		errorCode, haveErrorCode = minimaxErrorCode(failure)
	}
	return minimaxFoldCodes(statusCode, haveStatusCode, errorCode, haveErrorCode)
}

func minimaxBaseRespCode(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Object {
		return 0, false
	}
	var statusCode int64
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
			statusCode, haveCode = parsed, true
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
	if !haveCode {
		return 0, false
	}
	return statusCode, true
}

func minimaxErrorCode(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Object {
		return 0, false
	}
	var errorCode int64
	var haveCode, haveMessage, haveType, haveHTTPCode bool
	for _, member := range value.Members() {
		switch member.Name {
		case "code":
			if haveCode {
				return 0, false
			}
			parsed, ok := minimaxNumber(member.Value)
			if !ok {
				return 0, false
			}
			errorCode, haveCode = parsed, true
		case "message":
			if haveMessage {
				return 0, false
			}
			if _, ok := member.Value.Text(); !ok {
				return 0, false
			}
			haveMessage = true
		case "type":
			if haveType {
				return 0, false
			}
			if _, ok := member.Value.Text(); !ok {
				return 0, false
			}
			haveType = true
		case "http_code":
			if haveHTTPCode {
				return 0, false
			}
			if _, ok := member.Value.Text(); !ok && member.Value.Kind() != oif.Number {
				return 0, false
			}
			haveHTTPCode = true
		}
	}
	if !haveCode {
		return 0, false
	}
	return errorCode, true
}

func minimaxNumber(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Number {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value.Raw(), 64)
	if err != nil || parsed != math.Trunc(parsed) || parsed < -9223372036854775808 || parsed >= 9223372036854775808 {
		return 0, false
	}
	return int64(parsed), true
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
	errCode := message[open+2 : len(message)-1]
	if errCode == "" {
		return "", false
	}
	for _, r := range errCode {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return errCode, true
}
