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

	callIdentityBinding  = 24 * time.Hour
	retainedResultTTL    = 15 * time.Minute
	maxRetainedResults   = 512
	maxCallIDBytes       = 256
	callerCallIDMetaName = "olp.call_id"
)

type RetainedCall struct {
	AttemptID  string
	DispatchID string
	Status     Status
	IsError    bool
	Conflict   bool
}

type CallRepeatError struct {
	Retained RetainedCall
}

func (e *CallRepeatError) Error() string { return "mcp: caller call id already bound" }

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

func validCallDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for index := 0; index < len(digest); index++ {
		c := digest[index]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (s *Server) callDigest(authority access.Authority, callID string) string {
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
	if len(s.results) >= maxRetainedResults {
		return
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
