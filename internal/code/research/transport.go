package research

import (
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

type streamForwarding struct {
	method, service, resumeKey string
	requestID                  json.RawMessage
	answerPing                 func([]byte) error
}

func (h *Handler) copyResponse(w http.ResponseWriter, r *http.Request, response *http.Response, forwarding streamForwarding) bool {
	refuse := func() { h.Host.WriteError(w, r, code.Refuse(502, "code_research_response_invalid")) }
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		refuse()
		return false
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	for _, name := range []string{"Retry-After", "Mcp-Protocol-Version"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if media != "text/event-stream" || response.StatusCode != http.StatusOK {
		raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponse+1))
		if err != nil || len(raw) > MaxResponse {
			refuse()
			return false
		}
		if response.StatusCode == http.StatusOK && len(raw) != 0 {
			if media != "application/json" {
				refuse()
				return false
			}
			raw, err = Response(raw, forwarding.method, forwarding.service)
			if err != nil {
				refuse()
				return false
			}
		}
		if media != "" {
			w.Header().Set("Content-Type", media)
		}
		w.WriteHeader(response.StatusCode)
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		n, err := w.Write(raw)
		if err != nil || n != len(raw) || response.StatusCode != http.StatusOK || forwarding.method != "initialize" || !Initialized(raw, forwarding.requestID) || r.Context().Err() != nil {
			return false
		}
		return http.NewResponseController(w).Flush() == nil && r.Context().Err() == nil
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	reader := &io.LimitedReader{R: response.Body, N: MaxResponse + 1}
	decoder := sse.NewDecoder(reader, MaxEvent)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if err := controller.Flush(); err != nil {
		return false
	}
	_ = controller.SetWriteDeadline(time.Time{})
	write := func(raw []byte) error {
		_ = controller.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		defer controller.SetWriteDeadline(time.Time{})
		if n, err := w.Write(raw); err != nil {
			return err
		} else if n != len(raw) {
			return io.ErrShortWrite
		}
		return controller.Flush()
	}
	decoder.OnComment = func(line string) error { return write([]byte(line + "\n")) }
	session := r.Header.Get("Mcp-Session-Id")
	if session == "" {
		session = w.Header().Get("Mcp-Session-Id")
	}
	initialized, pings := false, 0
	for {
		frame, err := decoder.NextFrame()
		if err != nil || reader.N <= 0 {
			return initialized
		}
		if !frame.Control && strings.TrimSpace(frame.Data) != "" {
			reply, err := PingResponse([]byte(frame.Data))
			if err != nil {
				return initialized
			}
			if reply != nil {
				pings++
				if pings > 32 || forwarding.answerPing == nil || forwarding.answerPing(reply) != nil {
					return initialized
				}
				continue
			}
			filtered, err := Response([]byte(frame.Data), forwarding.method, forwarding.service)
			if err != nil {
				return initialized
			}
			if filtered == nil {
				continue
			}
			frame.Data = string(filtered)
		}
		if frame.ID != nil && *frame.ID != "" {
			sealed, err := SealEventID(session, *frame.ID, forwarding.resumeKey)
			if err != nil {
				return initialized
			}
			frame.ID = &sealed
		}
		if err = write(frame.Encode()); err != nil {
			return initialized
		}
		if forwarding.method == "initialize" && !frame.Control && Initialized([]byte(frame.Data), forwarding.requestID) {
			initialized = true
		}
	}
}

func WithoutCostBudgets(authority keyauthority.Authority) keyauthority.Authority {
	authority.Policy.WeeklyCostLimit = nil
	authority.Policy.DailyCostLimit = nil
	authority.Policy.MonthlyCostLimit = nil
	authority.BudgetGroupDailyCostLimit = nil
	authority.BudgetGroupMonthlyCostLimit = nil
	authority.BudgetGroupWeeklyCostLimit = nil
	authority.InstallationBudget, authority.ProjectBudget = nil, nil
	authority.ProjectAttributionBudgets = nil
	authority.Policy.RouteLimits = maps.Clone(authority.Policy.RouteLimits)
	for slug, policy := range authority.Policy.RouteLimits {
		policy.DailyCostLimit, policy.MonthlyCostLimit = nil, nil
		policy.WeeklyCostLimit = nil
		authority.Policy.RouteLimits[slug] = policy
	}
	authority.Policy.EndUserPolicy = stripEndUserCostBudgets(authority.Policy.EndUserPolicy)
	authority.ProjectEndUserPolicy = stripEndUserCostBudgets(authority.ProjectEndUserPolicy)
	authority.OrganizationBudget = nil
	return authority
}

func stripEndUserCostBudgets(policy *keyauthority.EndUserPolicy) *keyauthority.EndUserPolicy {
	if policy == nil {
		return nil
	}
	copied := *policy
	copied.Defaults.DailyCostLimit, copied.Defaults.MonthlyCostLimit = nil, nil
	copied.Defaults.WeeklyCostLimit = nil
	copied.Overrides = maps.Clone(policy.Overrides)
	for digest, limits := range copied.Overrides {
		limits.DailyCostLimit, limits.MonthlyCostLimit = nil, nil
		limits.WeeklyCostLimit = nil
		copied.Overrides[digest] = limits
	}
	return &copied
}
