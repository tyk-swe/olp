//go:build integration && codecli

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/tests/clientfixture/scripted"
	"github.com/tyk-swe/olp/tests/codecli"
)

// vendorHosts names the adapter whose upstream each of the fixture peer's
// hosts stands in for.
var vendorHosts = map[string]codemode.Adapter{
	"api.z.ai":         codemode.AdapterZAICoding,
	"open.bigmodel.cn": codemode.AdapterZAICoding,
	"opencode.ai":      codemode.AdapterOpenCodeGo,
}

// scriptedVendor answers as each coding plan's upstream with the scripted
// client fixture. It first holds each request to the key of the adapter whose
// host it reached, in the header that adapter places for the request's
// protocol, then gives the fixture its own path, credential and model; the
// answer streams back unchanged.
func scriptedVendor(t *testing.T, f *codingPlanFixture, keys map[codemode.Adapter]string) {
	t.Helper()
	fixture := scripted.New(scripted.Options{Credential: "scripted-upstream"})
	f.peer.mu.Lock()
	defer f.peer.mu.Unlock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		protocol, path, model := codemode.ProtocolChat, scripted.OpenAIPrefix+"/chat/completions", scripted.OpenAIModel
		if strings.HasSuffix(r.URL.Path, "/messages") {
			protocol, path, model = codemode.ProtocolMessages, scripted.AnthropicPrefix+"/messages", scripted.AnthropicModel
		}
		adapter := vendorHosts[r.Host]
		key, known := keys[adapter]
		header, want := codeadapter.CredentialHeader(adapter, protocol), key
		if header == "Authorization" {
			want = "Bearer " + key
		}
		if !known || r.Header.Get(header) != want || len(r.Header.Values("Authorization"))+len(r.Header.Values("X-Api-Key")) != 1 {
			t.Errorf("%s reached the upstream without the account's own credential", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			w.WriteHeader(400)
			return
		}
		body["model"] = model
		unquote(body)
		raw, _ = json.Marshal(quoted(body))
		forwarded := r.Clone(r.Context())
		forwarded.URL.Path, forwarded.Body, forwarded.ContentLength = path, io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
		forwarded.Header.Del("Authorization")
		forwarded.Header.Del("X-Api-Key")
		if protocol == codemode.ProtocolMessages {
			forwarded.Header.Set("X-Api-Key", "scripted-upstream")
		} else {
			forwarded.Header.Set("Authorization", "Bearer scripted-upstream")
		}
		fixture.ServeHTTP(w, forwarded)
	}
}

// unquote decodes user text that is one JSON string literal: OpenCode's run
// command sends its prompt that way, which a model reads through but the
// fixture's directive parser does not.
func unquote(body map[string]any) {
	decode := func(text any) any {
		var decoded string
		if s, ok := text.(string); ok && strings.HasPrefix(s, `"`) && json.Unmarshal([]byte(s), &decoded) == nil {
			return decoded
		}
		return text
	}
	messages, _ := body["messages"].([]any)
	for _, m := range messages {
		message, _ := m.(map[string]any)
		if message == nil || message["role"] != "user" {
			continue
		}
		message["content"] = decode(message["content"])
		blocks, _ := message["content"].([]any)
		for _, b := range blocks {
			if block, _ := b.(map[string]any); block != nil && block["type"] == "text" {
				block["text"] = decode(block["text"])
			}
		}
	}
}

// quoted neutralizes script directives a client only quotes, such as Claude
// Code's auto-mode classifier quoting the prompt as escaped JSON, so the
// fixture answers that side request with plain text instead of refusing it.
func quoted(value any) any {
	switch v := value.(type) {
	case string:
		if strings.Contains(v, `[[olp:`) && strings.Contains(v, `\"`) {
			return strings.ReplaceAll(v, `[[olp:`, `[[quoted-olp:`)
		}
	case map[string]any:
		for name, member := range v {
			v[name] = quoted(member)
		}
	case []any:
		for i, element := range v {
			v[i] = quoted(element)
		}
	}
	return value
}

// clientConfiguration is the configuration management generates for client
// against the fixture's in-process gateway. An empty plan takes the default
// planning model.
func (f *codingPlanFixture) clientConfiguration(t *testing.T, client, model, plan string) string {
	t.Helper()
	query := url.Values{"gateway_url": {f.gateway.URL}, "client": {client}, "model": {model}, "plan_model": {plan}}
	config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route["id"].(string)+"/client-config?"+query.Encode(), nil, nil, 200)
	return config["configuration"].(string)
}

// qualified holds a journey to its evidence: every upstream request carried
// the account's key and never the developer's OLP key or a workstation
// credential, each settled with reported usage, one account served the whole
// journey, and no request left through the proxy trap.
func (f *codingPlanFixture) qualified(t *testing.T, agent *codecli.Agent, roots int) []map[string]any {
	t.Helper()
	sent := f.peer.received()
	if len(sent) == 0 {
		t.Fatal("the journey reached no upstream")
	}
	for _, request := range sent {
		if text := fmtRequest(request); strings.Contains(text, f.key) || strings.Contains(text, codecli.Decoy) {
			t.Fatalf("a client credential reached the upstream: %s %s", request.Host, request.Path)
		}
	}
	attempts := f.settled(t, len(sent))
	if len(attempts) != len(sent) {
		t.Fatalf("%d attempts for %d upstream requests", len(attempts), len(sent))
	}
	for _, attempt := range attempts {
		if attempt["state"] != "settled" || attempt["reported_tokens"] == nil {
			t.Fatalf("attempt without settled usage: %v", attempt)
		}
	}
	bindings := f.h.want(f.owner, "GET", "/api/v1/code/bindings?project_id="+f.project, nil, nil, 200)["items"].([]any)
	accounts, rooted := map[any]bool{}, map[any]bool{}
	for _, item := range bindings {
		binding := item.(map[string]any)
		accounts[binding["account_id"]], rooted[binding["root_id"]] = true, true
	}
	if len(accounts) != 1 || len(rooted) != roots {
		t.Fatalf("bindings: %v", bindings)
	}
	if hosts := agent.Escaped(); len(hosts) != 0 {
		t.Fatalf("the client sent traffic outside OLP: %v", hosts)
	}
	out := []map[string]any{}
	for _, item := range bindings {
		out = append(out, item.(map[string]any))
	}
	return out
}

func adapterOf(profile string) codemode.Adapter {
	vendor, _ := codeadapter.ForConnection("plugin", "grant", profile)
	return vendor.Adapter
}

func fmtRequest(r peerRequest) string {
	var b strings.Builder
	b.WriteString(r.Path + "?" + r.RawQuery)
	for name, values := range r.Header {
		b.WriteString(name + ": " + strings.Join(values, ", ") + "\n")
	}
	b.Write(r.Body)
	return b.String()
}

func TestCodeQualificationClaudeCodeJourneys(t *testing.T) {
	for _, test := range []struct{ plugin, profile, model string }{
		{"zai-coding", codeplans.ZAIProfile, "glm-5.3"},
		{"opencode-go", codeplans.OpenCodeGoProfile, "minimax-m3"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			key := "0123456789abcdef0123456789abcdef.CONTROLLEDclaude"
			f := newCodingPlanFixture(t, test.plugin, test.profile, key)
			scriptedVendor(t, f, map[codemode.Adapter]string{adapterOf(test.profile): key})
			configuration := f.clientConfiguration(t, codeadapter.ClientClaudeCode, test.model, "")
			claude := codecli.NewClaudeCode(t, f.key)
			note := filepath.Join(claude.Work, "note.txt")
			if err := os.WriteFile(note, []byte("CONTROLLED_NOTE_CONTENT"), 0600); err != nil {
				t.Fatal(err)
			}
			read := claude.Run(t, configuration, "-p", `Read the note. [[olp:tool Read {"file_path":"`+note+`"}]]`, "--output-format", "json", "--permission-mode", "default", "--allowedTools", "Read")
			if !bytes.Contains(read, []byte("CONTROLLED_NOTE_CONTENT")) || bytes.Contains(read, []byte(`"is_error":true`)) {
				t.Fatalf("tool loop: %s", read)
			}
			child := claude.Run(t, configuration, "-p", `Delegate. [[olp:tool Agent {"description":"child","prompt":"Report done.","subagent_type":"general-purpose"}]]`, "--output-format", "json", "--permission-mode", "default", "--allowedTools", "Agent,Task")
			if bytes.Contains(child, []byte(`"is_error":true`)) {
				t.Fatalf("subagent: %s", child)
			}
			resumed := claude.Run(t, configuration, "-p", "Continue.", "--continue", "--output-format", "json", "--permission-mode", "default")
			if bytes.Contains(resumed, []byte(`"is_error":true`)) {
				t.Fatalf("resume: %s", resumed)
			}
			bindings := f.qualified(t, claude, 2)
			var subagent bool
			for _, binding := range bindings {
				conversation := binding["conversation"].(string)
				subagent = subagent || strings.Contains(conversation, "/") && binding["parent_id"] != nil
			}
			if !subagent {
				t.Fatalf("the subagent was not bound beneath its session: %v", bindings)
			}
			for _, request := range f.peer.received() {
				if request.Header.Get("X-Claude-Code-Session-Id") == "" || request.RawQuery != "beta=true" || request.Header.Get("Anthropic-Version") == "" {
					t.Fatalf("Claude Code request lost its native headers: %v %s", request.Header, request.RawQuery)
				}
				var body struct{ Model string }
				if json.Unmarshal(request.Body, &body) != nil || body.Model != test.model {
					t.Fatalf("a request left the route's model: %s", body.Model)
				}
			}
		})
	}
}

func TestCodeQualificationOpenCodeJourneys(t *testing.T) {
	for _, test := range []struct{ plugin, profile, model, path string }{
		{"zai-coding", codeplans.ZAIProfile, "glm-5.3", "/api/coding/paas/v4/chat/completions"},
		{"opencode-go", codeplans.OpenCodeGoProfile, "glm-5.3", "/zen/go/v1/chat/completions"},
		{"opencode-go", codeplans.OpenCodeGoProfile, "minimax-m3", "/zen/go/v1/messages"},
	} {
		t.Run(test.profile+"/"+test.model, func(t *testing.T) {
			key := "0123456789abcdef0123456789abcdef.CONTROLLEDopencode"
			f := newCodingPlanFixture(t, test.plugin, test.profile, key)
			scriptedVendor(t, f, map[codemode.Adapter]string{adapterOf(test.profile): key})
			configuration := f.clientConfiguration(t, codeadapter.ClientOpenCode, test.model, "")
			opencode := codecli.NewOpenCode(t, f.key)
			note := filepath.Join(opencode.Work, "note.txt")
			if err := os.WriteFile(note, []byte("CONTROLLED_NOTE_CONTENT"), 0600); err != nil {
				t.Fatal(err)
			}
			read := opencode.Run(t, configuration, "run", `Read the note. [[olp:tool read {"filePath":"`+note+`"}]]`, "--format", "json")
			if !bytes.Contains(read, []byte("CONTROLLED_NOTE_CONTENT")) || bytes.Contains(read, []byte(`"type":"error"`)) {
				t.Fatalf("tool loop: %s", read)
			}
			child := opencode.Run(t, configuration, "run", `Delegate. [[olp:tool task {"description":"child","prompt":"Report done.","subagent_type":"general"}]]`, "--format", "json")
			if bytes.Contains(child, []byte(`"type":"error"`)) {
				t.Fatalf("subagent: %s", child)
			}
			resumed := opencode.Run(t, configuration, "run", "--continue", "Continue.", "--format", "json")
			if bytes.Contains(resumed, []byte(`"type":"error"`)) {
				t.Fatalf("resume: %s", resumed)
			}
			bindings := f.qualified(t, opencode, 2)
			var nested bool
			for _, binding := range bindings {
				nested = nested || binding["parent_id"] != nil
			}
			if !nested {
				t.Fatalf("the child session was not bound beneath its parent: %v", bindings)
			}
			for _, request := range f.peer.received() {
				if request.Path != test.path || request.Header.Get("X-Opencode-Session-Id") == "" {
					t.Fatalf("OpenCode request %s lost its endpoint or session: %v", request.Path, request.Header)
				}
				if strings.HasSuffix(request.Path, "/chat/completions") && !bytes.Contains(request.Body, []byte(`"include_usage":true`)) {
					t.Fatal("OpenCode did not ask for final usage")
				}
			}
			if _, err := os.Stat(filepath.Join(opencode.Home, ".local", "share", "opencode", "auth.json")); !os.IsNotExist(err) {
				t.Fatalf("an OpenCode login was stored: %v", err)
			}
		})
	}
}

// TestCodeQualificationMixedSubscriptionJourneys plans on an OpenCode Go model
// and implements on a GLM Coding Plan model within one client session. Each
// model reaches its own subscription with that subscription's key, and the
// session's tree pins one account to each model.
func TestCodeQualificationMixedSubscriptionJourneys(t *testing.T) {
	glmKey, goKey := "0123456789abcdef0123456789abcdef.CONTROLLEDmixed", "opencode0123456789abcdef0123456789"
	for _, test := range []struct {
		client string
		plan   func(*testing.T, *codingPlanFixture, string) *codecli.Agent
	}{
		{codeadapter.ClientClaudeCode, func(t *testing.T, f *codingPlanFixture, configuration string) *codecli.Agent {
			claude := codecli.NewClaudeCode(t, f.key)
			for _, args := range [][]string{
				{"-p", "Plan the change.", "--output-format", "json", "--permission-mode", "plan"},
				{"-p", "Implement it.", "--continue", "--output-format", "json", "--permission-mode", "default"},
			} {
				if out := claude.Run(t, configuration, args...); bytes.Contains(out, []byte(`"is_error":true`)) {
					t.Fatalf("%s: %s", args[1], out)
				}
			}
			return claude
		}},
		{codeadapter.ClientOpenCode, func(t *testing.T, f *codingPlanFixture, configuration string) *codecli.Agent {
			opencode := codecli.NewOpenCode(t, f.key)
			for _, args := range [][]string{
				{"run", "--agent", "plan", "Plan the change.", "--format", "json"},
				{"run", "--continue", "Implement it.", "--format", "json"},
			} {
				if out := opencode.Run(t, configuration, args...); bytes.Contains(out, []byte(`"type":"error"`)) {
					t.Fatalf("%s: %s", args[len(args)-3], out)
				}
			}
			return opencode
		}},
	} {
		t.Run(test.client, func(t *testing.T) {
			f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, glmKey)
			glm, other := f.mix(t, goKey)
			scriptedVendor(t, f, map[codemode.Adapter]string{codemode.AdapterZAICoding: glmKey, codemode.AdapterOpenCodeGo: goKey})
			agent := test.plan(t, f, f.clientConfiguration(t, test.client, "glm-5.3", "minimax-m3"))
			bindings := f.qualified(t, agent, 1)
			hosts := map[string]string{"glm-5.3": "api.z.ai", "minimax-m3": "opencode.ai"}
			seen := map[string]bool{}
			for _, request := range f.peer.received() {
				var body struct{ Model string }
				if json.Unmarshal(request.Body, &body) != nil || hosts[body.Model] != request.Host {
					t.Fatalf("model %q reached %s%s", body.Model, request.Host, request.Path)
				}
				seen[body.Model] = true
			}
			if !seen["glm-5.3"] || !seen["minimax-m3"] {
				t.Fatalf("the session did not use both subscriptions: %v", seen)
			}
			accounts := map[string]any{"glm-5.3": glm["id"], "minimax-m3": other["id"]}
			for _, binding := range bindings {
				pins := binding["pins"].([]any)
				for _, item := range pins {
					pin := item.(map[string]any)
					if accounts[pin["model"].(string)] != pin["account_id"] {
						t.Fatalf("pin %v is not its model's account", pin)
					}
				}
				if binding["parent_id"] == nil && len(pins) != 2 {
					t.Fatalf("the session pinned %d models: %v", len(pins), binding)
				}
			}
		})
	}
}
