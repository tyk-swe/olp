package scripts

import "github.com/tyk-swe/olp/internal/limits"

const (
	limitsResponseVersion = 2
	costResponseVersion   = 1
	fixedWindowMS         = 60_000
	dayMS                 = 86_400_000
	maxMonthMS            = 33 * dayMS
	maxWindowID           = limits.MaxCounter / fixedWindowMS
	headFields            = 6
	allowanceFields       = 5
)

func replyInt(value any) (int64, bool) {
	number, ok := value.(int64)
	return number, ok
}

func luaSafe(value int64) bool { return value >= 0 && value <= limits.MaxCounter }

type head struct {
	status int64
	detail string
	retry  int64
	first  int64
	second int64
}

func decodeHead(items []any, wantVersion int64) (head, bool) {
	if len(items) < headFields {
		return head{}, false
	}
	version, okVersion := replyInt(items[0])
	status, okStatus := replyInt(items[1])
	detail, okDetail := items[2].(string)
	retry, okRetry := replyInt(items[3])
	first, okFirst := replyInt(items[4])
	second, okSecond := replyInt(items[5])
	if !okVersion || !okStatus || !okDetail || !okRetry || !okFirst || !okSecond ||
		version != wantVersion || !luaSafe(retry) || !luaSafe(first) || !luaSafe(second) {
		return head{}, false
	}
	return head{status: status, detail: detail, retry: retry, first: first, second: second}, true
}

type flags struct {
	status int64
	detail string
	first  int64
	second int64
}

func decodeFlags(value any) (flags, bool) {
	items, ok := value.([]any)
	if !ok || len(items) != 5 {
		return flags{}, false
	}
	version, okVersion := replyInt(items[0])
	status, okStatus := replyInt(items[1])
	detail, okDetail := items[2].(string)
	first, okFirst := replyInt(items[3])
	second, okSecond := replyInt(items[4])
	if !okVersion || !okStatus || !okDetail || !okFirst || !okSecond || version != costResponseVersion {
		return flags{}, false
	}
	return flags{status: status, detail: detail, first: first, second: second}, true
}

func switchArg(on bool) string {
	if on {
		return "1"
	}
	return "0"
}
