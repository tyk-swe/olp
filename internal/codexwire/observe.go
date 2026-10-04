package codexwire

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
)

type Observation = codemode.Observation

func Observe(body []byte, unary bool) Observation {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return Observation{}
	}
	root := document.Root()
	now := time.Now().UTC()
	kind, _ := field(root, "type").Text()
	if kind == "codex.rate_limits" {
		return Observation{Allowance: eventAllowance(root, now)}
	}
	if kind == "error" {
		status, _ := strconv.Atoi(field(root, "status").Raw())
		if status == 0 {
			status, _ = strconv.Atoi(field(root, "status_code").Raw())
		}
		if status < 400 || status > 599 {
			status = 0
		}
		headers := http.Header{}
		for _, member := range field(root, "headers").Members() {
			name, value := member.Name, member.Value
			if text, ok := value.Text(); ok {
				headers.Add(name, text)
			} else if value.Kind() == oif.Number {
				headers.Add(name, value.Raw())
			}
		}
		outcome := &codemode.Outcome{Origin: "upstream", Kind: "failed", ObservedAt: now}
		if status != 0 {
			outcome.UpstreamStatus = &status
		}
		return Observation{Terminal: true, Status: status, Outcome: outcome, Allowance: Allowance(headers, now)}
	}
	terminal := kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed"
	successful := kind == "response.completed"
	outcomeKind := strings.TrimPrefix(kind, "response.")
	if terminal {
		root = field(root, "response")
	}
	if unary {
		status, _ := field(root, "status").Text()
		terminal = status == "completed" || status == "incomplete" || status == "failed"
		successful = status == "completed"
		outcomeKind = status
		object, _ := field(root, "object").Text()
		if object == "response.compaction" && (status == "" || status == "completed") {
			terminal = true
			successful = field(root, "error").Kind() == oif.Absent || field(root, "error").Kind() == oif.Null
			outcomeKind = "failed"
			if successful {
				outcomeKind = "completed"
			}
		}
	}
	if !terminal {
		return Observation{}
	}
	id, _ := field(root, "id").Text()
	usage := field(root, "usage")
	read := func(value oif.Value) *int64 {
		if value.Kind() != oif.Number {
			return nil
		}
		n, err := strconv.ParseInt(value.Raw(), 10, 64)
		if err != nil || n < 0 || n > 1<<53-1 {
			return nil
		}
		return &n
	}
	u := codemode.Usage{Total: read(field(usage, "total_tokens")), Input: read(field(usage, "input_tokens")), Output: read(field(usage, "output_tokens")), Cached: read(field(field(usage, "input_tokens_details"), "cached_tokens")), Reasoning: read(field(field(usage, "output_tokens_details"), "reasoning_tokens"))}
	if u.Validate() != nil {
		u = codemode.Usage{}
	}
	return Observation{Terminal: true, Successful: successful, Outcome: &codemode.Outcome{Origin: "upstream", Kind: outcomeKind, ObservedAt: now}, Usage: u, ResponseID: id}
}

// Stream observes bounded SSE events independently of the forwarded byte stream.
type Stream struct{ *codewire.Events }

func NewStream(limit int, emit func(Observation)) *Stream {
	return &Stream{codewire.NewEvents(limit, func(data []byte) { emit(Observe(data, false)) })}
}

func Allowance(headers http.Header, now time.Time) *codemode.Allowance {
	result := &codemode.Allowance{ObservedAt: now}
	prefixes := map[string]bool{"x-codex": true}
	for name := range headers {
		name = strings.ToLower(name)
		for _, window := range []string{"primary", "secondary"} {
			if strings.HasPrefix(name, "x-") && strings.HasSuffix(name, "-"+window+"-used-percent") {
				prefixes[strings.TrimSuffix(name, "-"+window+"-used-percent")] = true
			}
		}
	}
	for prefix := range prefixes {
		id := normalizeLimit(strings.TrimPrefix(prefix, "x-"))
		for _, kind := range []string{"primary", "secondary"} {
			base := prefix + "-" + kind
			if w := allowanceWindow(id, kind, headerValue(headers, base+"-used-percent"), headerValue(headers, base+"-window-minutes"), headerValue(headers, base+"-reset-at"), now); w != nil && (w.UsedPercent != 0 || w.WindowMinutes != nil && *w.WindowMinutes != 0 || w.ResetsAt != nil) {
				result.Windows = append(result.Windows, *w)
			}
		}
	}
	result.Credits = credits(headerValue(headers, "x-codex-credits-has-credits"), headerValue(headers, "x-codex-credits-unlimited"), headerValue(headers, "x-codex-credits-balance"), now)
	return finishAllowance(result)
}

func headerValue(headers http.Header, name string) string {
	values := headers.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func normalizeLimit(id string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(id)), "-", "_")
}

func allowanceWindow(id, kind, usedText, minutesText, resetText string, now time.Time) *codemode.AllowanceWindow {
	used, err := strconv.ParseFloat(usedText, 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 1<<53-1 {
		return nil
	}
	w := &codemode.AllowanceWindow{LimitID: id, Window: kind, UsedPercent: used, RemainingPercent: max(0, 100-used), ObservedAt: now}
	if minutes, err := strconv.ParseInt(minutesText, 10, 64); err == nil && minutes >= 0 && minutes <= 1<<53-1 {
		w.WindowMinutes = &minutes
	}
	if seconds, err := strconv.ParseInt(resetText, 10, 64); err == nil && seconds > 0 && seconds < 253402300800 {
		reset := time.Unix(seconds, 0).UTC()
		w.ResetsAt = &reset
	}
	if w.Validate() != nil {
		return nil
	}
	return w
}

func credits(hasText, unlimitedText, balance string, now time.Time) *codemode.Credits {
	parse := func(text string) (bool, error) {
		switch strings.ToLower(text) {
		case "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		default:
			return false, strconv.ErrSyntax
		}
	}
	has, err := parse(hasText)
	if err != nil {
		return nil
	}
	unlimited, err := parse(unlimitedText)
	if err != nil {
		return nil
	}
	c := &codemode.Credits{HasCredits: has, Unlimited: unlimited, ObservedAt: now}
	if balance = strings.TrimSpace(balance); balance != "" {
		c.Balance = &balance
		if c.Validate() != nil {
			c.Balance = nil
		}
	}
	return c
}

func eventAllowance(root oif.Value, now time.Time) *codemode.Allowance {
	id, _ := field(root, "metered_limit_name").Text()
	if id == "" {
		id, _ = field(root, "limit_name").Text()
	}
	if id == "" {
		id = "codex"
	}
	result := &codemode.Allowance{ObservedAt: now}
	for _, kind := range []string{"primary", "secondary"} {
		value := field(field(root, "rate_limits"), kind)
		if w := allowanceWindow(normalizeLimit(id), kind, field(value, "used_percent").Raw(), field(value, "window_minutes").Raw(), field(value, "reset_at").Raw(), now); w != nil {
			result.Windows = append(result.Windows, *w)
		}
	}
	c := field(root, "credits")
	balance, _ := field(c, "balance").Text()
	result.Credits = credits(field(c, "has_credits").Raw(), field(c, "unlimited").Raw(), balance, now)
	return finishAllowance(result)
}

func finishAllowance(a *codemode.Allowance) *codemode.Allowance {
	a.Normalize()
	if len(a.Windows) == 0 && a.Credits == nil || a.Validate() != nil {
		return nil
	}
	return a
}

// ForwardHeaders excludes authentication, OLP controls and hop-by-hop fields.
func ForwardHeaders(source http.Header, request bool) http.Header {
	return codewire.ForwardHeaders(source, request)
}

// ForwardTrailers also excludes fields nominated by the original message's
// Connection headers, which are absent from its trailer map.
func ForwardTrailers(source, headers http.Header, request bool) http.Header {
	return codewire.ForwardTrailers(source, headers, request)
}
