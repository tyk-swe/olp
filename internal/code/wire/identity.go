package wire

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/oif"
)

func ConnectionIdentity(policy vendor.IdentityPolicy, headers http.Header) (code.Identity, error) {
	var identity code.Identity
	var err error
	if policy == vendor.IdentityCodex {
		identity, err = CodexIdentity(headers, oif.Value{})
	} else {
		identity, err = ClientIdentity(headers, oif.Value{})
	}
	if err != nil {
		return identity, err
	}
	if identity.Generic() && len(headers.Values("X-Olp-Code-Affinity")) == 0 {
		return identity, code.Refuse(400, "code_identity_invalid")
	}
	return identity, nil
}

func genericIdentity(h http.Header, root oif.Value, policy vendor.IdentityPolicy) (code.Identity, bool, error) {
	metadata := field(root, "client_metadata")
	if policy == vendor.IdentityCodex && metadata.Kind() != oif.Absent && metadata.Kind() != oif.Null && metadata.Kind() != oif.Object {
		return code.Identity{}, false, code.Refuse(400, "code_identity_invalid")
	}
	codex := hasHeader(h, "Thread-Id", "Session-Id", "Session_id", "X-Codex-Parent-Thread-Id", "X-Openai-Subagent") || policy == vendor.IdentityCodex && hasField(metadata, "thread_id", "x-codex-parent-thread-id", "x-openai-subagent")
	claude := hasHeader(h, "X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Agent-Id")
	opencode := hasHeader(h, "X-Opencode-Session-Id", "X-Opencode-Parent-Session-Id")
	affinity := h.Values("X-Olp-Code-Affinity")
	if codex && (claude || opencode || policy != vendor.IdentityCodex) || claude && opencode || (claude || opencode) && policy != vendor.IdentityClient || len(affinity) != 0 && (codex || claude || opencode) {
		return code.Identity{}, false, code.Refuse(400, "code_identity_ambiguous")
	}
	if codex || claude || opencode {
		return code.Identity{}, false, nil
	}
	if len(affinity) == 0 {
		return code.Identity{Conversation: "olp:request/" + uuid.NewString()}, true, nil
	}
	if len(affinity) != 1 || affinity[0] == "" || strings.TrimSpace(affinity[0]) != affinity[0] || len(affinity[0]) > 256 {
		return code.Identity{}, false, code.Refuse(400, "code_identity_ambiguous")
	}
	sum := sha256.Sum256([]byte(affinity[0]))
	return code.Identity{Conversation: "olp:affinity/" + hex.EncodeToString(sum[:])}, true, nil
}

func ClientIdentity(h http.Header, root oif.Value) (code.Identity, error) {
	if identity, generic, err := genericIdentity(h, root, vendor.IdentityClient); generic || err != nil {
		return identity, err
	}
	var identity code.Identity
	values := map[string]string{}
	for _, name := range []string{"X-Opencode-Session-Id", "X-Opencode-Parent-Session-Id", "X-Claude-Code-Session-Id", "X-Claude-Code-Agent-Id", "X-Claude-Code-Parent-Agent-Id"} {
		switch v := h.Values(name); {
		case len(v) == 1 && strings.TrimSpace(v[0]) == v[0] && v[0] != "":
			values[name] = v[0]
		case len(v) != 0:
			return identity, code.Refuse(400, "code_identity_ambiguous")
		}
	}
	session, parent := values["X-Opencode-Session-Id"], values["X-Opencode-Parent-Session-Id"]
	claude, agent, parentAgent := values["X-Claude-Code-Session-Id"], values["X-Claude-Code-Agent-Id"], values["X-Claude-Code-Parent-Agent-Id"]
	opencode, claudeCode := session != "" || parent != "", claude != "" || agent != "" || parentAgent != ""
	switch {
	case opencode && claudeCode || parentAgent != "" && agent == "":
		return identity, code.Refuse(400, "code_identity_ambiguous")
	case opencode && session != "":
		identity.Conversation = encodeIdentityComponent(session)
		if parent != "" {
			identity.Parent = encodeIdentityComponent(parent)
		}
	case claude != "":
		identity.Conversation = encodeIdentityComponent(claude)
		if agent != "" {
			identity.Parent = identity.Conversation
			if parentAgent != "" {
				identity.Parent += "/" + encodeIdentityComponent(parentAgent)
			}
			identity.Conversation += "/" + encodeIdentityComponent(agent)
		}
	}
	return identity, identity.Validate()
}

func CodexIdentity(headers http.Header, root oif.Value) (code.Identity, error) {
	if identity, generic, err := genericIdentity(headers, root, vendor.IdentityCodex); generic || err != nil {
		return identity, err
	}
	var result code.Identity
	metadata := field(root, "client_metadata")
	if metadata.Kind() != oif.Absent && metadata.Kind() != oif.Null && metadata.Kind() != oif.Object {
		return result, code.Refuse(400, "code_identity_invalid")
	}
	merge := func(target *string, value string) bool {
		if value == "" {
			return true
		}
		if *target != "" && *target != value {
			return false
		}
		*target = value
		return true
	}
	readHeader := func(name string, target *string) bool {
		values := headers.Values(name)
		return len(values) == 0 || len(values) == 1 && values[0] != "" && merge(target, values[0])
	}
	readField := func(name string, target *string) bool {
		v := field(metadata, name)
		if v.Kind() == oif.Absent {
			return true
		}
		s, ok := v.Text()
		return ok && s != "" && merge(target, s)
	}
	if !readHeader("thread-id", &result.Conversation) || !readField("thread_id", &result.Conversation) || !readHeader("x-codex-parent-thread-id", &result.Parent) || !readField("x-codex-parent-thread-id", &result.Parent) {
		return result, code.Refuse(400, "code_identity_ambiguous")
	}
	if result.Conversation == "" && (!readHeader("session-id", &result.Conversation) || !readHeader("session_id", &result.Conversation)) {
		return result, code.Refuse(400, "code_identity_ambiguous")
	}
	var subagent string
	if !readHeader("x-openai-subagent", &subagent) || !readField("x-openai-subagent", &subagent) {
		return result, code.Refuse(400, "code_identity_ambiguous")
	}
	if subagent != "" && result.Parent == "" {
		return result, code.Refuse(409, "code_parent_unresolved")
	}
	if result.Generic() || (code.Identity{Conversation: result.Parent}).Generic() {
		return result, code.Refuse(400, "code_identity_ambiguous")
	}
	return result, result.Validate()
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

func encodeIdentityComponent(id string) string {
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
