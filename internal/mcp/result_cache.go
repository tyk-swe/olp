package mcp

import (
	"bytes"
	"slices"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/mcp/upstream"
)

type resultCache struct {
	mu      sync.Mutex
	entries map[string]retainedResult
}

func cloneToolResult(result upstream.ToolResult) upstream.ToolResult {
	return upstream.ToolResult{Content: slices.Clone(result.Content), StructuredContent: bytes.Clone(result.StructuredContent), IsError: result.IsError}
}

type retainedResult struct {
	result  upstream.ToolResult
	expires time.Time
}

func (s *resultCache) retain(attemptID string, result upstream.ToolResult, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]retainedResult)
	}
	for id, entry := range s.entries {
		if !entry.expires.After(now) {
			delete(s.entries, id)
		}
	}
	if _, kept := s.entries[attemptID]; !kept && len(s.entries) >= maxRetainedResults {
		oldestID, oldestExpiry := "", time.Time{}
		for id, entry := range s.entries {
			if oldestID == "" || entry.expires.Before(oldestExpiry) {
				oldestID, oldestExpiry = id, entry.expires
			}
		}
		delete(s.entries, oldestID)
	}
	s.entries[attemptID] = retainedResult{result: cloneToolResult(result), expires: now.Add(retainedResultTTL)}
}

func (s *resultCache) find(attemptID string, now time.Time) (upstream.ToolResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[attemptID]
	if !ok {
		return upstream.ToolResult{}, false
	}
	if !entry.expires.After(now) {
		delete(s.entries, attemptID)
		return upstream.ToolResult{}, false
	}
	return cloneToolResult(entry.result), true
}
