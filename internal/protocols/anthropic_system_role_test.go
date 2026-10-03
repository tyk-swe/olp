package protocols

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// Claude Code appends role "system" messages to the conversation under the
// mid-conversation-system beta, so Messages requests may carry them.
const midConversationSystem = `{"model":"route","max_tokens":16,"messages":[` +
	`{"role":"user","content":"first"},` +
	`{"role":"system","content":[{"type":"text","text":"reminder","cache_control":{"type":"ephemeral"}}]},` +
	`{"role":"assistant","content":[{"type":"text","text":"answer"}]},` +
	`{"role":"user","content":"second"}]}`

func TestAnthropicRequestsAdmitMidConversationSystemMessages(t *testing.T) {
	for _, family := range []openai.Family{openai.FamilyAnthropic, openai.FamilyAnthropicCount} {
		r, err := Parse(family, []byte(midConversationSystem), "")
		if err != nil {
			t.Fatalf("%s: %v", family, err)
		}
		// The native source is preserved byte for byte, cache marker included.
		prepared, _, err := EncodeTarget(r, family, "anthropic", "anthropic", "wire-model", nil)
		if err != nil {
			t.Fatalf("%s: %v", family, err)
		}
		var sent struct {
			Messages []struct {
				Role    string `json:"role"`
				Content json.RawMessage
			}
		}
		if err := json.Unmarshal(prepared, &sent); err != nil {
			t.Fatal(err)
		}
		roles := []string{}
		for _, m := range sent.Messages {
			roles = append(roles, m.Role)
		}
		if len(roles) != 4 || roles[1] != "system" || !bytes.Contains(sent.Messages[1].Content, []byte(`"cache_control":{"type":"ephemeral"}`)) {
			t.Fatalf("%s: native request changed: %s", family, prepared)
		}
	}
}

func TestAnthropicRequestsStillRefuseOtherRoles(t *testing.T) {
	for _, role := range []string{"tool", "developer", "function", ""} {
		body := `{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"` + role + `","content":"x"}]}`
		_, err := Parse(openai.FamilyAnthropic, []byte(body), "")
		var refusal *openai.RequestError
		if !errors.As(err, &refusal) || refusal.Param != "messages.role" {
			t.Fatalf("role %q: err = %v", role, err)
		}
	}
}

// A translating route keeps the position of the system message instead of
// hoisting it to the front or dropping it.
func TestMidConversationSystemMessageTranslatesInPlace(t *testing.T) {
	r, err := Parse(openai.FamilyAnthropic, []byte(midConversationSystem), "")
	if err != nil {
		t.Fatal(err)
	}
	// cache_control cannot be preserved on a Chat Completions target, so the
	// translation refuses it instead of dropping the marker.
	if _, _, err := Encode(r, "openai", "openai", "wire-model", nil); err == nil {
		t.Fatal("cache marker silently dropped by translation")
	}
	r, err = Parse(openai.FamilyAnthropic, []byte(`{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"system","content":"reminder"},{"role":"user","content":"second"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Encode(r, "openai", "openai", "wire-model", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct{ Role, Content string }
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"user", "first"}, {"system", "reminder"}, {"user", "second"}}
	if len(sent.Messages) != len(want) {
		t.Fatalf("messages = %+v", sent.Messages)
	}
	for i, m := range sent.Messages {
		if m.Role != want[i][0] || m.Content != want[i][1] {
			t.Fatalf("message %d = %+v, want %v", i, m, want[i])
		}
	}
	if _, _, err := Encode(r, "anthropic", "anthropic", "wire-model", nil); err != nil {
		t.Fatalf("native Anthropic target refused its own dialect: %v", err)
	}
	// A translated Anthropic target cannot place a late system message and
	// refuses it cleanly.
	chat, err := Parse(openai.FamilyChat, []byte(`{"model":"route","messages":[{"role":"user","content":"first"},{"role":"system","content":"reminder"}],"max_tokens":8}`), "")
	if err != nil {
		t.Fatal(err)
	}
	var refusal *openai.RequestError
	if _, _, err := Encode(chat, "anthropic", "anthropic", "wire-model", nil); !errors.As(err, &refusal) {
		t.Fatalf("late system message to a translated Anthropic target: err = %v", err)
	}
}

// Converse has one system field ahead of the conversation, so a system message
// between turns is refused instead of silently moving to the front.
func TestBedrockRefusesSystemMessagesBetweenTurns(t *testing.T) {
	leading := `{"model":"route","max_tokens":16,"system":"be brief","messages":[{"role":"user","content":"first"}]}`
	r, err := Parse(openai.FamilyAnthropic, []byte(leading), "")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := Encode(r, "bedrock", "bedrock", "wire-model", nil)
	if err != nil {
		t.Fatalf("a leading system prompt: %v", err)
	}
	if !bytes.Contains(body, []byte(`"system":[{"text":"be brief"}]`)) {
		t.Fatalf("the leading system prompt did not reach the system field: %s", body)
	}
	between := `{"model":"route","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"system","content":"reminder"},{"role":"user","content":"second"}]}`
	r, err = Parse(openai.FamilyAnthropic, []byte(between), "")
	if err != nil {
		t.Fatal(err)
	}
	var refusal *openai.RequestError
	if _, _, err := Encode(r, "bedrock", "bedrock", "wire-model", nil); !errors.As(err, &refusal) {
		t.Fatalf("a system message between turns: err = %v", err)
	}
}
