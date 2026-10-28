package mcp

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

//go:embed scripts/session_create.lua
var sessionCreateSource string

//go:embed scripts/session_touch.lua
var sessionTouchSource string

//go:embed scripts/session_initialize.lua
var sessionInitializeSource string

//go:embed scripts/session_delete.lua
var sessionDeleteSource string

//go:embed scripts/session_certify.lua
var sessionCertifySource string

type sessionScript struct{ source, digest string }

func newSessionScript(source string) sessionScript {
	sum := sha1.Sum([]byte(source))
	return sessionScript{source: source, digest: hex.EncodeToString(sum[:])}
}

var (
	sessionCreateScript     = newSessionScript(sessionCreateSource)
	sessionTouchScript      = newSessionScript(sessionTouchSource)
	sessionInitializeScript = newSessionScript(sessionInitializeSource)
	sessionDeleteScript     = newSessionScript(sessionDeleteSource)
	sessionCertifyScript    = newSessionScript(sessionCertifySource)
)

var errMalformedSessionState = errors.New("mcp: malformed session state in the shared store")

type Commander interface {
	Do(ctx context.Context, args ...string) (any, error)
}

type SessionStore struct {
	client    Commander
	namespace string
}

func NewSessionStore(client Commander, namespace string) (*SessionStore, error) {
	if concrete, ok := client.(*coordination.Client); client == nil || (ok && concrete == nil) {
		return nil, errors.New("mcp: a Valkey client is required")
	}
	if len(namespace) < 1 || len(namespace) > 128 {
		return nil, errors.New("mcp: session namespace must be 1-128 characters")
	}
	for index := 0; index < len(namespace); index++ {
		c := namespace[index]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == ':', c == '_', c == '-':
		default:
			return nil, errors.New("mcp: session namespace may only contain ASCII letters, digits, ':', '_' and '-'")
		}
	}
	return &SessionStore{client: client, namespace: namespace}, nil
}

func (s *SessionStore) sessionKey(binding sessionBinding, id string) string {
	return s.namespace + ":{" + binding.scope() + "}:session:" + id
}

func (s *SessionStore) indexKey(binding sessionBinding) string {
	return s.namespace + ":{" + binding.scope() + "}:sessions"
}

func (s *SessionStore) eval(ctx context.Context, script sessionScript, keys []string, args ...string) (any, error) {
	command := make([]string, 0, 3+len(keys)+len(args))
	command = append(command, "EVALSHA", script.digest, strconv.Itoa(len(keys)))
	command = append(command, keys...)
	command = append(command, args...)
	value, err := s.client.Do(ctx, command...)
	if noSessionScript(err) {
		command[0], command[1] = "EVAL", script.source
		value, err = s.client.Do(ctx, command...)
	}
	return value, err
}

func noSessionScript(err error) bool {
	command, ok := errors.AsType[*coordination.CommandError](err)
	if !ok || command.Ambiguous || command.Cause == nil {
		return false
	}
	text := command.Cause.Error()
	return strings.Contains(text, "NOSCRIPT") || strings.Contains(text, "NoScriptError")
}

func (s *SessionStore) createSession(ctx context.Context, record sessionRecord) (bool, error) {
	args := []string{
		record.id,
		strconv.FormatInt(sessionIdleTimeout.Milliseconds(), 10),
		strconv.Itoa(maxSessionsPerKey),
		record.binding.installationID, record.binding.projectID, record.binding.scope(),
		record.protocol, record.toolsetID, record.toolsetRevisionID, record.releaseID,
	}
	for _, serverID := range slices.Sorted(maps.Keys(record.certifications)) {
		args = append(args, serverID, string(record.certifications[serverID]))
	}
	value, err := s.eval(ctx, sessionCreateScript,
		[]string{s.sessionKey(record.binding, record.id), s.indexKey(record.binding)}, args...)
	if err != nil {
		return false, err
	}
	created, ok := sessionReplyInt(value)
	if !ok || created < 0 || created > 1 {
		return false, errMalformedSessionState
	}
	return created == 1, nil
}

func (s *SessionStore) accessSession(ctx context.Context, id string, binding sessionBinding) (sessionRecord, bool, error) {
	value, err := s.eval(ctx, sessionTouchScript,
		[]string{s.sessionKey(binding, id), s.indexKey(binding)},
		id, strconv.FormatInt(sessionIdleTimeout.Milliseconds(), 10))
	if err != nil {
		return sessionRecord{}, false, err
	}
	fields, found, err := sessionReplyFields(value)
	if err != nil || !found {
		return sessionRecord{}, false, err
	}
	record, err := sessionRecordFromFields(id, binding, fields)
	if err != nil {
		return sessionRecord{}, false, err
	}
	if record.binding != binding {
		return sessionRecord{}, false, nil
	}
	return record, true, nil
}

func (s *SessionStore) markSessionInitialized(ctx context.Context, id string, binding sessionBinding) error {
	value, err := s.eval(ctx, sessionInitializeScript, []string{s.sessionKey(binding, id)})
	if err != nil {
		return err
	}
	marked, ok := sessionReplyInt(value)
	if !ok || marked < 0 || marked > 1 {
		return errMalformedSessionState
	}
	return nil
}

func (s *SessionStore) setSessionCertification(ctx context.Context, id string, binding sessionBinding, serverID string, state certificationState) (bool, error) {
	value, err := s.eval(ctx, sessionCertifyScript, []string{s.sessionKey(binding, id)}, serverID, string(state))
	if err != nil {
		return false, err
	}
	updated, ok := sessionReplyInt(value)
	if !ok || updated < 0 || updated > 1 {
		return false, errMalformedSessionState
	}
	return updated == 1, nil
}

func (s *SessionStore) deleteSession(ctx context.Context, id string, binding sessionBinding) (bool, error) {
	value, err := s.eval(ctx, sessionDeleteScript,
		[]string{s.sessionKey(binding, id), s.indexKey(binding)}, id)
	if err != nil {
		return false, err
	}
	removed, ok := sessionReplyInt(value)
	if !ok || removed < 0 || removed > 1 {
		return false, errMalformedSessionState
	}
	return removed == 1, nil
}

func sessionReplyInt(value any) (int64, bool) {
	switch reply := value.(type) {
	case int64:
		return reply, true
	case int:
		return int64(reply), true
	default:
		return 0, false
	}
}

func sessionReplyFields(value any) (map[string]string, bool, error) {
	if value == nil {
		return nil, false, nil
	}
	flat, ok := value.([]any)
	if !ok {
		return nil, false, errMalformedSessionState
	}
	if len(flat) == 0 {
		return nil, false, nil
	}
	if len(flat)%2 != 0 {
		return nil, false, errMalformedSessionState
	}
	fields := make(map[string]string, len(flat)/2)
	for index := 0; index < len(flat); index += 2 {
		name, nameOK := flat[index].(string)
		field, fieldOK := flat[index+1].(string)
		if !nameOK || !fieldOK {
			return nil, false, errMalformedSessionState
		}
		fields[name] = field
	}
	return fields, true, nil
}

func sessionRecordFromFields(id string, binding sessionBinding, fields map[string]string) (sessionRecord, error) {
	createdMS, createdErr := strconv.ParseInt(fields["created_at_ms"], 10, 64)
	activeMS, activeErr := strconv.ParseInt(fields["last_active_at_ms"], 10, 64)
	initialized := fields["initialized"]
	if createdErr != nil || activeErr != nil || createdMS < 0 || activeMS < 0 ||
		(initialized != "0" && initialized != "1") ||
		fields["installation_id"] == "" || fields["project_id"] == "" ||
		fields["principal_digest"] == "" || fields["protocol"] == "" ||
		fields["toolset_id"] == "" || fields["toolset_revision_id"] == "" || fields["release_id"] == "" {
		return sessionRecord{}, errMalformedSessionState
	}
	certifications := map[string]certificationState{}
	for name, value := range fields {
		serverID, found := strings.CutPrefix(name, "cert:")
		if !found {
			continue
		}
		state := certificationState(value)
		if serverID == "" || (state != certificationCertified && state != certificationDrifted && state != certificationInvalid) {
			return sessionRecord{}, errMalformedSessionState
		}
		certifications[serverID] = state
	}
	stored := sessionBinding{
		installationID: fields["installation_id"],
		projectID:      fields["project_id"],
		principal:      binding.principal,
	}
	if fields["principal_digest"] != binding.scope() {
		stored.principal = principalIdentity{}
	}
	return sessionRecord{
		id: id, binding: stored, protocol: fields["protocol"],
		toolsetID: fields["toolset_id"], toolsetRevisionID: fields["toolset_revision_id"],
		releaseID: fields["release_id"], certifications: certifications,
		createdAt: time.UnixMilli(createdMS).UTC(), lastActiveAt: time.UnixMilli(activeMS).UTC(),
		initialized: initialized == "1",
	}, nil
}
