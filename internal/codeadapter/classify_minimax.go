package codeadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	if err != nil {
		var sourceErr *oif.SourceError
		if errors.As(err, &sourceErr) && sourceErr.Code == "duplicate_key" {
			return minimaxDeclaredCodeSplit(body)
		}
		return "", false
	}
	if document.Root().Kind() != oif.Object {
		return "", false
	}
	if code, ok := minimaxDeclaredCode(document.Root()); ok {
		return code, true
	}
	return minimaxEnvelope(document.Root())
}

func minimaxDeclaredCodeSplit(body []byte) (string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return "", false
	}
	var baseRaw, errorRaw json.RawMessage
	var haveBaseResp, haveFailure, basePoisoned, failurePoisoned bool
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", false
		}
		name, ok := key.(string)
		if !ok {
			return "", false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", false
		}
		switch name {
		case "base_resp":
			if haveBaseResp {
				basePoisoned = true
				continue
			}
			baseRaw, haveBaseResp = raw, true
		case "error":
			if haveFailure {
				failurePoisoned = true
				continue
			}
			errorRaw, haveFailure = raw, true
		}
	}
	if _, err := decoder.Token(); err != nil {
		return "", false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", false
	}
	var statusCode int64
	var haveStatusCode bool
	if haveBaseResp && !basePoisoned {
		statusCode, haveStatusCode = minimaxRawCode(baseRaw, minimaxBaseRespCode)
	}
	var errorCode int64
	var haveErrorCode bool
	if haveFailure && !failurePoisoned {
		errorCode, haveErrorCode = minimaxRawCode(errorRaw, minimaxErrorCode)
	}
	return minimaxFoldCodes(statusCode, haveStatusCode, errorCode, haveErrorCode)
}

func minimaxRawCode(raw json.RawMessage, extract func(oif.Value) (int64, bool)) (int64, bool) {
	document, err := oif.ParseJSON(raw, oif.Limits{MaxBytes: 64 << 10})
	if err != nil {
		return 0, false
	}
	return extract(document.Root())
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
	if !haveCode {
		return 0, false
	}
	return code, true
}

func minimaxErrorCode(value oif.Value) (int64, bool) {
	if value.Kind() != oif.Object {
		return 0, false
	}
	var code int64
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
			code, haveCode = parsed, true
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
	return code, true
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
