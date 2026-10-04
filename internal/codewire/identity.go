package codewire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
)

// ClientIdentity reads the conversation identity of a Claude Code or OpenCode
// request from the client's own headers. OpenCode names its session and, for a
// child session, the parent session. Claude Code names its session and, on a
// subagent's requests, the agent and any parent agent; an agent is a child of
// its parent agent, or of the session itself. Agent identifiers are scoped by
// their session, because teammates reuse name-based identifiers.
func ClientIdentity(h http.Header) (codemode.Identity, error) {
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
