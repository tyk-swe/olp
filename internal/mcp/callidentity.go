package mcp

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	retainedResultTTL  = 15 * time.Minute
	maxRetainedResults = 512
	maxCallIDBytes     = 256
)

type repeatKind int

const (
	repeatConflict repeatKind = iota
	repeatInProgress
	repeatOutcomeUnknown
	repeatRetainedResult
	repeatResultNotRetained
	repeatGuardrailBlocked
)

func repeatOutcome(repeat *accounting.RetainedCall, hasResult bool) repeatKind {
	switch {
	case repeat.Conflict:
		return repeatConflict
	case repeat.Status == accounting.StatusGuardrailBlocked:
		return repeatGuardrailBlocked
	case repeat.Status == accounting.StatusInProgress:
		return repeatInProgress
	case repeat.Status == accounting.StatusOutcomeUnknown:
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

func (s *Executor) callDigest(authority access.Authority, callID string) string {
	if !validCallID(callID) {
		return ""
	}
	return hex.EncodeToString(s.auth.Digest(secrets.ToolCallDigest, retainedPrincipal(authority)+"\x00"+callID))
}

func retainedPrincipal(authority access.Authority) string {
	if authority.DelegationAgentID != "" {
		return authority.ID + "\x00delegation\x00" + authority.DelegationAgentID + "\x00" + authority.DelegationPairDigest
	}
	if individual := sessions.VerifiedEndUser(authority); individual != "" {
		return authority.ID + "\x00" + individual
	}
	return authority.ID
}
