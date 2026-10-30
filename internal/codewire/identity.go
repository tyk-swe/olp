package codewire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

type IdentityFamily uint8

const (
	SessionIdentity IdentityFamily = iota
	CodexIdentity
)

func GenericIdentity(h http.Header, root oif.Value, family IdentityFamily) (codemode.Identity, bool, error) {
	metadata := field(root, "client_metadata")
	if family == CodexIdentity && metadata.Kind() != oif.Absent && metadata.Kind() != oif.Null && metadata.Kind() != oif.Object {
		return codemode.Identity{}, false, codemode.Refuse(400, "code_identity_invalid")
	}
	codex := hasHeader(h, "Thread-Id", "Session-Id", "Session_id", "X-Codex-Parent-Thread-Id", "X-Openai-Subagent") || family == CodexIdentity && hasField(metadata, "thread_id", "x-codex-parent-thread-id", "x-openai-subagent")
	claude := hasHeader(h, "X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Agent-Id")
	opencode := hasHeader(h, "X-Opencode-Session-Id", "X-Opencode-Parent-Session-Id")
	affinity := h.Values("X-Olp-Code-Affinity")
	if codex && (claude || opencode || family != CodexIdentity) || claude && opencode || (claude || opencode) && family != SessionIdentity || len(affinity) != 0 && (codex || claude || opencode) {
		return codemode.Identity{}, false, codemode.Refuse(400, "code_identity_ambiguous")
	}
	if codex || claude || opencode {
		return codemode.Identity{}, false, nil
	}
	if len(affinity) == 0 {
		return codemode.Identity{Conversation: "olp:request/" + uuid.NewString()}, true, nil
	}
	if len(affinity) != 1 || affinity[0] == "" || strings.TrimSpace(affinity[0]) != affinity[0] || len(affinity[0]) > 256 {
		return codemode.Identity{}, false, codemode.Refuse(400, "code_identity_ambiguous")
	}
	sum := sha256.Sum256([]byte(affinity[0]))
	return codemode.Identity{Conversation: "olp:affinity/" + hex.EncodeToString(sum[:])}, true, nil
}

func hasHeader(h http.Header, names ...string) bool {
	for _, name := range names {
		if len(h.Values(name)) != 0 {
			return true
		}
	}
	return false
}

func hasField(value oif.Value, names ...string) bool {
	for _, name := range names {
		if _, present := value.Lookup(name); present {
			return true
		}
	}
	return false
}

func ClientIdentity(h http.Header) (codemode.Identity, error) {
	return RequestIdentity(h, oif.Value{})
}

func RequestIdentity(h http.Header, root oif.Value) (codemode.Identity, error) {
	if identity, generic, err := GenericIdentity(h, root, SessionIdentity); generic || err != nil {
		return identity, err
	}
	var identity codemode.Identity
	values := map[string]string{}
	for _, name := range []string{"X-Opencode-Session-Id", "X-Opencode-Parent-Session-Id", "X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Agent-Id"} {
		switch v := h.Values(name); {
		case len(v) == 1 && strings.TrimSpace(v[0]) == v[0] && v[0] != "":
			values[name] = v[0]
		case len(v) != 0:
			return identity, codemode.Refuse(400, "code_identity_ambiguous")
		}
	}
	session, parent := values["X-Opencode-Session-Id"], values["X-Opencode-Parent-Session-Id"]
	claude, agent, parentAgent := values["X-Claude-Code-Session-Id"], values["X-Claude-Code-Agent-Id"], values["X-Claude-Code-Parent-Agent-Id"]
	opencode, claudeCode := session != "" || parent != "", claude != "" || agent != "" || parentAgent != ""
	switch {
	case opencode && claudeCode || parentAgent != "" && agent == "":
		return identity, codemode.Refuse(400, "code_identity_ambiguous")
	case opencode && session != "":
		identity.Conversation = component(session)
		if parent != "" {
			identity.Parent = component(parent)
		}
	case claude != "":
		identity.Conversation = component(claude)
		if agent != "" {
			identity.Parent = identity.Conversation
			if parentAgent != "" {
				identity.Parent += "/" + component(parentAgent)
			}
			identity.Conversation += "/" + component(agent)
		}
	}
	return identity, identity.Validate()
}

// component encodes a client identifier injectively within the code-mode
// identifier alphabet: bytes outside letters, digits, '.' and '-' become ":xx".
// An identifier that would not begin with a letter or digit, or exceeds 100
// bytes once encoded, is replaced by a digest.
func component(id string) string {
	var b strings.Builder
	for i := range len(id) {
		c := id[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '.' || c == '-') {
			b.WriteByte(c)
		} else if i == 0 {
			break
		} else {
			fmt.Fprintf(&b, ":%02x", c)
		}
	}
	if b.Len() == 0 || b.Len() > 100 {
		sum := sha256.Sum256([]byte(id))
		return "h-" + hex.EncodeToString(sum[:16])
	}
	return b.String()
}
