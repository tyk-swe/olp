package mcp

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const (
	sessionIdleTimeout  = 15 * time.Minute
	maxSessionsPerKey   = 64
	sessionIDRandomSize = 32
)

type session struct {
	id             string
	installationID string
	projectID      string
	principal      string
	protocol       string
	toolset        resolvedToolset
	releaseID      string
	createdAt      time.Time
	lastActiveAt   time.Time
	initialized    bool
}

func principalKey(authority access.Authority) string {
	return strings.Join([]string{authority.ID, authority.Issuer, authority.LookupID, authority.WorkloadDigest}, "\x00")
}

func newSessionID() (string, error) {
	raw := make([]byte, sessionIDRandomSize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func sessionExpired(candidate session, now time.Time) bool {
	return !now.Before(candidate.lastActiveAt.Add(sessionIdleTimeout))
}

func sessionOwnedBy(candidate session, authority access.Authority) bool {
	if authority.ProjectID == nil {
		return false
	}
	return candidate.principal == principalKey(authority) &&
		candidate.projectID == *authority.ProjectID &&
		candidate.installationID == authority.InstallationID
}

func (s *Server) createSession(candidate session) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]session{}
	}
	active := 0
	for id, existing := range s.sessions {
		if sessionExpired(existing, candidate.createdAt) {
			delete(s.sessions, id)
			continue
		}
		if existing.principal == candidate.principal {
			active++
		}
	}
	if active >= maxSessionsPerKey {
		return false
	}
	s.sessions[candidate.id] = candidate
	return true
}

func (s *Server) accessSession(id string, authority access.Authority) (session, bool) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	existing, ok := s.sessions[id]
	if !ok || !sessionOwnedBy(existing, authority) {
		return session{}, false
	}
	now := s.now()
	if sessionExpired(existing, now) {
		delete(s.sessions, id)
		return session{}, false
	}
	accessed := existing
	accessed.lastActiveAt = now
	s.sessions[id] = accessed
	return accessed, true
}

func (s *Server) markSessionInitialized(id string, authority access.Authority) {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	existing, ok := s.sessions[id]
	if !ok || !sessionOwnedBy(existing, authority) {
		return
	}
	updated := existing
	updated.initialized = true
	s.sessions[id] = updated
}

func (s *Server) deleteSession(id string, authority access.Authority) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	existing, ok := s.sessions[id]
	if !ok || !sessionOwnedBy(existing, authority) {
		return false
	}
	if sessionExpired(existing, s.now()) {
		delete(s.sessions, id)
		return false
	}
	delete(s.sessions, id)
	return true
}
