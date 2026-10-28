package mcp

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	StatusInProgress Status = "in_progress"

	OrphanDispatchAge = 2 * time.Minute

	callIdentityBinding = 24 * time.Hour
	retainedResultTTL   = 15 * time.Minute
	maxRetainedResults  = 512
	maxCallIDBytes      = 256
)

type RetainedCall struct {
	AttemptID  string
	DispatchID string
	Status     Status
	Conflict   bool
}

type ReserveResult struct {
	Reservation *Reservation
	Repeat      *RetainedCall
}

type repeatKind int

const (
	repeatConflict repeatKind = iota
	repeatInProgress
	repeatOutcomeUnknown
	repeatRetainedResult
	repeatResultNotRetained
	repeatGuardrailBlocked
)

func repeatOutcome(repeat *RetainedCall, hasResult bool) repeatKind {
	switch {
	case repeat.Conflict:
		return repeatConflict
	case repeat.Status == StatusGuardrailBlocked:
		return repeatGuardrailBlocked
	case repeat.Status == StatusInProgress:
		return repeatInProgress
	case repeat.Status == StatusOutcomeUnknown:
		return repeatOutcomeUnknown
	case hasResult:
		return repeatRetainedResult
	default:
		return repeatResultNotRetained
	}
}

func repeatFailure(kind repeatKind) (code string, message string) {
	switch kind {
	case repeatConflict:
		return "call_id_conflict", "This call identifier already belongs to a different tool call."
	case repeatInProgress:
		return "call_in_progress", "This call identifier belongs to a call that is still in progress."
	case repeatOutcomeUnknown:
		return "outcome_unknown", "The tool call may have run upstream, but its outcome could not be confirmed."
	default:
		return "result_not_retained", "This call already ran, but its result is no longer retained."
	}
}

func validCallID(callID string) bool {
	if len(callID) < 1 || len(callID) > maxCallIDBytes || strings.TrimSpace(callID) == "" {
		return false
	}
	for index := 0; index < len(callID); index++ {
		if callID[index] < 0x20 || callID[index] == 0x7f {
			return false
		}
	}
	return true
}

func parseEvidenceDigest(digest string) ([]byte, bool) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != digest {
		return nil, false
	}
	return decoded, true
}

func (s *Server) callDigest(authority access.Authority, callID string) string {
	if !validCallID(callID) {
		return ""
	}
	return hex.EncodeToString(s.Auth.Digest(secrets.ToolCallDigest, authority.ID+"\x00"+callID))
}

type retainedResult struct {
	result  mcpservers.ToolResult
	expires time.Time
}

func (s *Server) retainResult(attemptID string, result mcpservers.ToolResult) {
	now := s.now()
	s.resultsMu.Lock()
	defer s.resultsMu.Unlock()
	if s.results == nil {
		s.results = make(map[string]retainedResult)
	}
	for id, entry := range s.results {
		if !entry.expires.After(now) {
			delete(s.results, id)
		}
	}
	if _, kept := s.results[attemptID]; !kept && len(s.results) >= maxRetainedResults {
		oldestID, oldestExpiry := "", time.Time{}
		for id, entry := range s.results {
			if oldestID == "" || entry.expires.Before(oldestExpiry) {
				oldestID, oldestExpiry = id, entry.expires
			}
		}
		delete(s.results, oldestID)
	}
	s.results[attemptID] = retainedResult{result: result, expires: now.Add(retainedResultTTL)}
}

func (s *Server) retainedResultFor(attemptID string) (mcpservers.ToolResult, bool) {
	s.resultsMu.Lock()
	defer s.resultsMu.Unlock()
	entry, ok := s.results[attemptID]
	if !ok {
		return mcpservers.ToolResult{}, false
	}
	if !entry.expires.After(s.now()) {
		delete(s.results, attemptID)
		return mcpservers.ToolResult{}, false
	}
	return entry.result, true
}
