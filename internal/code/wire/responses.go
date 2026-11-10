package wire

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/oif"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
)

type Responses struct {
	allowance bool
}

func NewResponses(allowance bool) Responses { return Responses{allowance: allowance} }

func (o Responses) Event(data []byte) code.Observation {
	return o.filter(ObserveResponses(data, false))
}

func (o Responses) Unary(body []byte) code.Observation {
	return o.filter(ObserveResponses(body, true))
}

func (Responses) Finish() (code.Observation, bool) { return code.Observation{}, false }

func (o Responses) filter(observation code.Observation) code.Observation {
	if !o.allowance {
		observation.Allowance = nil
	}
	return observation
}

func ClassifyResponses(body []byte, headers http.Header, path string, policy vendor.IdentityPolicy, websocket bool) (code.Request, error) {
	var request code.Request
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return request, code.Refuse(400, "code_body_invalid")
	}
	root := document.Root()
	conversation := field(root, "conversation")
	if conversation.Kind() != oif.Absent && conversation.Kind() != oif.Null {
		return request, code.Refuse(409, "code_parent_unresolved")
	}
	compact := false
	for _, item := range field(root, "input").Elements() {
		kind, _ := field(item, "type").Text()
		compact = compact || kind == "compaction_trigger"
		_, hasID := item.Lookup("id")
		if protocolwire.IsResponsesItemReference(kind, hasID) {
			return request, code.Refuse(409, "code_parent_unresolved")
		}
	}
	model, ok := field(root, "model").Text()
	if !ok || code.ValidateModels([]string{model}) != nil {
		return request, code.Refuse(400, "code_model_invalid")
	}
	request.Operation.Name = "responses"
	if path == "responses/compact" && !websocket || path == "responses" && compact {
		request.Operation.Name = "compact"
	} else if path != "responses" {
		return request, code.Refuse(400, "code_operation_unsupported")
	}
	if websocket {
		typ, _ := field(root, "type").Text()
		if typ != "response.create" {
			return request, code.Refuse(400, "code_operation_unsupported")
		}
		generate := field(root, "generate")
		if generate.Kind() != oif.Absent && generate.Kind() != oif.Boolean {
			return request, code.Refuse(400, "code_body_invalid")
		}
		if generate.Raw() == "false" {
			request.Operation.Name = "prewarm"
		}
	}
	for _, name := range []string{"background", "store"} {
		value := field(root, name)
		if value.Kind() != oif.Absent && value.Kind() != oif.Boolean {
			return request, code.Refuse(400, "code_body_invalid")
		}
		if name == "background" && value.Raw() == "true" {
			return request, code.Refuse(422, "code_operation_unsupported")
		}
	}
	request.Operation.Model = model
	var identified code.Identity
	if policy == vendor.IdentityCodex {
		identified, err = CodexIdentity(headers, root)
	} else {
		identified, err = ClientIdentity(headers, root)
	}
	if err != nil {
		return request, err
	}
	request.Operation.Identity = identified
	previous := field(root, "previous_response_id")
	if previous.Kind() != oif.Absent && previous.Kind() != oif.Null {
		request.PreviousResponse, ok = previous.Text()
		if !ok || (code.Identity{Conversation: request.PreviousResponse}).Validate() != nil {
			return request, code.Refuse(400, "code_reference_invalid")
		}
	}
	if err := ValidateGenericRequest(root, headers, request); err != nil {
		return request, err
	}
	request.Estimate = estimateTokens(body, root, "max_output_tokens")
	return request, nil
}

func codexWindowNames() [2]string {
	return [2]string{"primary", "secondary"}
}

func ObserveResponses(body []byte, unary bool) code.Observation {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: MaxBody})
	if err != nil {
		return code.Observation{}
	}
	root := document.Root()
	now := time.Now().UTC()
	kind, _ := field(root, "type").Text()
	if kind == "codex.rate_limits" {
		return code.Observation{Allowance: eventAllowance(root, now)}
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
		var upstreamStatus *int
		if status != 0 {
			upstreamStatus = &status
		}
		outcome := code.UpstreamTerminalOutcome(code.OutcomeFailed, upstreamStatus, now)
		return code.Observation{Terminal: true, Status: status, RetryAfter: headers.Values("Retry-After"), Outcome: &outcome, Allowance: CodexAllowance(headers, now), RejectionBody: RejectionBody(body)}
	}
	if unary && len(root.Members()) == 1 && field(root, "error").Kind() == oif.Object {
		return code.Observation{RejectionBody: RejectionBody(body)}
	}
	terminal := protocolwire.IsTerminalResponseEvent(kind)
	successful := kind == "response.completed"
	outcomeKind := code.OutcomeKind(strings.TrimPrefix(kind, "response."))
	if terminal {
		root = field(root, "response")
	}
	if unary {
		status, _ := field(root, "status").Text()
		terminal = protocolwire.IsTerminalResponseEvent("response." + status)
		successful = status == "completed"
		outcomeKind = code.OutcomeKind(status)
		obj, _ := field(root, "object").Text()
		if obj == "response.compaction" && (status == "" || status == "completed") {
			terminal = true
			successful = field(root, "error").Kind() == oif.Absent || field(root, "error").Kind() == oif.Null
			outcomeKind = code.OutcomeFailed
			if successful {
				outcomeKind = code.OutcomeCompleted
			}
		}
	}
	if !terminal {
		return code.Observation{}
	}
	id, _ := field(root, "id").Text()
	usage := field(root, "usage")
	u := code.Usage{
		Total:     count(field(usage, "total_tokens")),
		Input:     count(field(usage, "input_tokens")),
		Output:    count(field(usage, "output_tokens")),
		Cached:    count(field(field(usage, "input_tokens_details"), "cached_tokens")),
		Reasoning: count(field(field(usage, "output_tokens_details"), "reasoning_tokens")),
	}
	if u.Validate() != nil {
		u = code.Usage{}
	}
	outcome := code.UpstreamTerminalOutcome(outcomeKind, nil, now)
	return code.Observation{Terminal: true, Successful: successful, Outcome: &outcome, Usage: u, ResponseID: id, AllowanceCharge: AllowanceCharge(usage)}
}

func CodexAllowance(headers http.Header, now time.Time) *code.Allowance {
	prefixes := map[string]bool{"x-codex": true}
	for name := range headers {
		lower := strings.ToLower(name)
		for _, window := range codexWindowNames() {
			if strings.HasPrefix(lower, "x-") && strings.HasSuffix(lower, "-"+window+"-used-percent") {
				prefixes[strings.TrimSuffix(lower, "-"+window+"-used-percent")] = true
			}
		}
	}
	var windows []code.AllowanceWindow
	for prefix := range prefixes {
		id, _ := code.NormalizeLimitID(strings.TrimPrefix(prefix, "x-"))
		for _, kind := range codexWindowNames() {
			base := prefix + "-" + kind
			if w := parseAllowanceWindow(id, kind, headerValue(headers, base+"-used-percent"), headerValue(headers, base+"-window-minutes"), headerValue(headers, base+"-reset-at"), now); w != nil && (w.UsedPercent != 0 || w.WindowMinutes != nil && *w.WindowMinutes != 0 || w.ResetsAt != nil) {
				windows = append(windows, *w)
			}
		}
	}
	parsedCredits := parseCredits(headerValue(headers, "x-codex-credits-has-credits"), headerValue(headers, "x-codex-credits-unlimited"), headerValue(headers, "x-codex-credits-balance"), now)
	return finishAllowance(now, windows, parsedCredits)
}

func headerValue(headers http.Header, name string) string {
	values := headers.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

func parseAllowanceWindow(id, kind, usedText, minutesText, resetText string, now time.Time) *code.AllowanceWindow {
	used, err := strconv.ParseFloat(usedText, 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 1<<53-1 {
		return nil
	}
	w := &code.AllowanceWindow{LimitID: id, Window: kind, Unit: code.AllowanceUnitPercent, Evidence: code.AllowanceEvidencePassive, UsedPercent: used, RemainingPercent: max(0, 100-used), ObservedAt: now}
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

func parseCredits(hasText, unlimitedText, balance string, now time.Time) *code.Credits {
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
	c := &code.Credits{HasCredits: has, Unlimited: unlimited, ObservedAt: now}
	if balance = strings.TrimSpace(balance); balance != "" {
		c.Balance = &balance
		if c.Validate() != nil {
			c.Balance = nil
		}
	}
	return c
}

func eventAllowance(root oif.Value, now time.Time) *code.Allowance {
	id, _ := field(root, "metered_limit_name").Text()
	if id == "" {
		id, _ = field(root, "limit_name").Text()
	}
	if id == "" {
		id = "codex"
	}
	limit, _ := code.NormalizeLimitID(id)
	var windows []code.AllowanceWindow
	for _, kind := range codexWindowNames() {
		value := field(field(root, "rate_limits"), kind)
		if w := parseAllowanceWindow(limit, kind, field(value, "used_percent").Raw(), field(value, "window_minutes").Raw(), field(value, "reset_at").Raw(), now); w != nil {
			windows = append(windows, *w)
		}
	}
	c := field(root, "credits")
	balance, _ := field(c, "balance").Text()
	return finishAllowance(now, windows, parseCredits(field(c, "has_credits").Raw(), field(c, "unlimited").Raw(), balance, now))
}

func finishAllowance(now time.Time, windows []code.AllowanceWindow, credits *code.Credits) *code.Allowance {
	allowance := code.NewAllowance(now, windows, credits)
	if len(allowance.Windows) == 0 && allowance.Credits == nil || allowance.Validate() != nil {
		return nil
	}
	return &allowance
}
