//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestCodeQualificationDurableResponseReferencesAndGeneratedConfiguration(t *testing.T) {
	f := newCodeFleet(t)
	path := "/api/v1/code/routes/" + f.route["id"].(string) + "/client-config?gateway_url=" + f.gateways[0].PublicOrigin
	configuration := f.h.want(f.owner, "GET", path, nil, nil, 200)
	if configuration["base_url"] != f.gateways[0].PublicOrigin+"/code/qualification" ||
		!bytes.Contains([]byte(configuration["configuration"].(string)), []byte("OLP_API_KEY")) {
		t.Fatal("configuration lost public route or OLP-only authentication")
	}
	f.success(t, 0, "reference-root", "")
	if err := f.gateways[0].Stop(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("reference-restarted")
	body := []byte(`{"model":"gpt-5.4","stream":true,"previous_response_id":"resp-controlled","input":[]}`)
	for _, conversation := range []string{"reference-root", "reference-child"} {
		response, data, err := f.request(t.Context(), 0, conversation, "", body)
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("durable reference refused after restart: %v %s", err, data)
		}
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 2 || bindings[0].RootID != bindings[1].RootID || bindings[0].AccountID != bindings[1].AccountID {
		t.Fatal("response continuation did not inherit the durable tree")
	}
	missing := []byte(`{"model":"gpt-5.4","previous_response_id":"never-observed","input":[]}`)
	before := len(f.peer.Requests())
	response, data, err := f.request(t.Context(), 1, "orphan-reference", "", missing)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("unknown reference reached upstream")
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+bindings[0].ID+"/retire", nil, idem("reference-retirement"), 200)
	response, _, err = f.request(t.Context(), 1, "retired-reference", "", body)
	if err != nil || response.StatusCode != 410 || len(f.peer.Requests()) != before {
		t.Fatal("reference revived a retired tree")
	}
	f.success(t, 1, "new-reference-root", "")
	before = len(f.peer.Requests())
	response, data, err = f.request(t.Context(), 1, "new-reference-root", "", body)
	if err != nil || response.StatusCode != 409 || !bytes.Contains(data, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != before {
		t.Fatal("ambiguous upstream response ID authorized a continuation")
	}
}

func (f *codeFleet) clientRequest(t *testing.T, replica int, path string, header http.Header, body []byte) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.gateways[replica].PublicOrigin+"/code/qualification/"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = header.Clone()
	request.Header.Set("Authorization", "Bearer "+f.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, out
}

func (f *codeFleet) translateMessages(t *testing.T) {
	t.Helper()
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4"}, "enabled": true, "messages_to_codex": true}, etagHeader(f.route), 200)
	f.route = f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("translated-continuity")), 200)
	for i, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		f.gateways[i] = f.in.replica(fmt.Sprintf("translated-%d", i))
	}
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-translated","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"CONTROLLED_PRIVATE_OUTPUT"}]}],"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}`)}
	})
}

func TestCodeQualificationTranslatedLineageColdResumeAndSummaryCompaction(t *testing.T) {
	f := newCodeFleet(t)
	f.translateMessages(t)
	header := http.Header{"X-Claude-Code-Session-Id": {"translated-session"}}
	toolHistory := []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"},{"role":"assistant","content":[{"type":"tool_use","id":"call-owned","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-owned","content":"CONTROLLED_PRIVATE_TOOL"}]}]}`)
	for i, identity := range []struct{ agent, parent string }{{}, {agent: "child"}, {agent: "grandchild", parent: "child"}} {
		requestHeader := header.Clone()
		if identity.agent != "" {
			requestHeader.Set("X-Claude-Code-Agent-Id", identity.agent)
		}
		if identity.parent != "" {
			requestHeader.Set("X-Claude-Code-Parent-Agent-Id", identity.parent)
		}
		response, body := f.clientRequest(t, i%2, "v1/messages", requestHeader, toolHistory)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("CONTROLLED_PRIVATE_OUTPUT")) {
			t.Fatalf("translated lineage status %d body %s", response.StatusCode, body)
		}
	}
	for i, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		f.gateways[i] = f.in.replica(fmt.Sprintf("translated-cold-%d", i))
	}
	child := header.Clone()
	child.Set("X-Claude-Code-Agent-Id", "child")
	response, body := f.clientRequest(t, 1, "v1/messages", child, toolHistory)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cold child resume status %d body %s", response.StatusCode, body)
	}
	summary := []byte(`{"model":"gpt-5.4","max_tokens":64,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_SUMMARY"}]}`)
	response, body = f.clientRequest(t, 0, "v1/messages", child, summary)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("summary continuation status %d body %s", response.StatusCode, body)
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 3 {
		t.Fatalf("translated lineage created %d bindings", len(bindings))
	}
	root := slices.IndexFunc(bindings, func(b codemode.Binding) bool { return b.ParentID == nil })
	if root < 0 {
		t.Fatal("translated lineage lost root")
	}
	childBinding := slices.IndexFunc(bindings, func(b codemode.Binding) bool { return b.ParentID != nil && *b.ParentID == bindings[root].ID })
	if childBinding < 0 || !slices.ContainsFunc(bindings, func(b codemode.Binding) bool { return b.ParentID != nil && *b.ParentID == bindings[childBinding].ID }) {
		t.Fatal("translated child or grandchild lost its parent")
	}
	for _, binding := range bindings {
		if binding.RootID != bindings[0].RootID || binding.AccountID != bindings[0].AccountID {
			t.Fatal("translated resume or compaction changed account ownership")
		}
	}
	captures := f.peer.Requests()
	if len(captures) != 5 {
		t.Fatalf("translated requests dispatched %d times", len(captures))
	}
	for _, capture := range captures {
		if capture.Header.Get("Authorization") != captures[0].Header.Get("Authorization") {
			t.Fatal("translated tree switched credentials")
		}
	}
	if !bytes.Contains(captures[3].Body, []byte(`"call_id":"call-owned"`)) || !bytes.Contains(captures[3].Body, []byte("CONTROLLED_PRIVATE_TOOL")) || !bytes.Contains(captures[4].Body, []byte("CONTROLLED_PRIVATE_SUMMARY")) || !bytes.Contains(captures[4].Body, []byte(`"effort":"high"`)) {
		t.Fatal("cold tool history or explicit summary reasoning was lost")
	}
	response, body = f.clientRequest(t, 1, "v1/messages", child, []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"CONTROLLED_PRIVATE_REASONING","signature":"CONTROLLED_PRIVATE_SIGNATURE"}]},{"role":"user","content":"Continue"}]}`))
	if response.StatusCode != http.StatusUnprocessableEntity || !bytes.Contains(body, []byte("code_translation_unsupported_semantics")) || len(f.peer.Requests()) != len(captures) {
		t.Fatalf("opaque continuation dispatched or lost state, status %d body %s", response.StatusCode, body)
	}
	for _, collection := range []string{"attempts", "bindings", "refusals"} {
		_, raw := f.h.do(f.owner, "GET", "/api/v1/code/"+collection+"?project_id="+f.project, nil, nil)
		for _, private := range []string{"CONTROLLED_PRIVATE_PROMPT", "CONTROLLED_PRIVATE_TOOL", "CONTROLLED_PRIVATE_OUTPUT", "CONTROLLED_PRIVATE_SUMMARY", "CONTROLLED_PRIVATE_REASONING", "CONTROLLED_PRIVATE_SIGNATURE"} {
			if bytes.Contains(raw, []byte(private)) {
				t.Fatalf("%s persisted conversation content", collection)
			}
		}
	}
}

func TestCodeQualificationTranslatedResponseReferencesStayOnTheirProducingAccount(t *testing.T) {
	f := newCodeFleet(t)
	f.translateMessages(t)
	plans := &codingPlanFixture{h: f.h, owner: f.owner, project: f.project, peer: newFixedHostPeer(t, "opencode.ai")}
	plans.digest = installPlugin(t, f.h, f.owner, testutil.BuildPlugin(t, "./plugins/opencode-go"))
	provider := plans.provider(t, "Controlled other family", codeplans.OpenCodeGoProfile)
	credential := plans.enroll(t, provider, "opencode0123456789abcdef0123456789")
	account := f.h.want(f.owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Controlled response owner", "enabled": true, "models": []string{"gpt-5.5"}}, idem("reference-owner"), 201)
	accounts := []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string), account["id"].(string)}
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), map[string]any{"project_id": f.project, "name": "Controlled mixed pool", "kind": "shared", "owner_user_id": nil, "account_ids": accounts, "api_key_ids": []string{f.keyID}}, etagHeader(f.pool), 200)
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4", "gpt-5.5"}, "enabled": true, "messages_to_codex": true}, etagHeader(f.route), 200)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("reference-owner-publish")), 200)
	if err := f.gateways[0].Kill(); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("reference-owner")
	header := http.Header{"X-Claude-Code-Session-Id": {"owned-translated-response"}}
	response, body := f.clientRequest(t, 0, "v1/messages", header, []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"controlled"}]}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("translated response status %d body %s", response.StatusCode, body)
	}
	response, body = f.clientRequest(t, 0, "v1/responses", header, []byte(`{"model":"gpt-5.5","previous_response_id":"resp-translated","input":[]}`))
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("code_response_account_conflict")) || len(plans.peer.received()) != 0 {
		t.Fatalf("foreign response reached another account, status %d body %s", response.StatusCode, body)
	}
	nativeHeader := http.Header{"Thread-Id": {"owned-translated-response"}, "Session-Id": {"owned-translated-response"}}
	nativeBody := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-translated","input":[]}`)
	response, body = f.clientRequest(t, 0, "responses", nativeHeader, nativeBody)
	if response.StatusCode != http.StatusOK || len(f.peer.Requests()) != 2 || !bytes.Equal(f.peer.Requests()[1].Body, nativeBody) {
		t.Fatalf("owned native continuation changed or refused, status %d body %s", response.StatusCode, body)
	}
	plans.peer.mu.Lock()
	plans.peer.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(codingPlanResponsesSSE, "resp_fixture", "resp-translated"))
	}
	plans.peer.mu.Unlock()
	response, body = f.clientRequest(t, 0, "v1/responses", header, []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"full controlled context"}]}`))
	if response.StatusCode != http.StatusOK || len(plans.peer.received()) != 1 {
		t.Fatalf("full-context recovery status %d body %s", response.StatusCode, body)
	}
	response, body = f.clientRequest(t, 0, "responses", nativeHeader, nativeBody)
	if response.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != 2 {
		t.Fatalf("ambiguous response ownership dispatched, status %d body %s", response.StatusCode, body)
	}
}

func TestCodeQualificationTranslatedContinuationRechecksLiveAuthority(t *testing.T) {
	for _, target := range []string{"pool", "account", "key", "project", "retirement", "pool-account", "personal-owner"} {
		t.Run(target, func(t *testing.T) {
			f := newCodeFleet(t)
			key, memberID := f.memberKey(t)
			if target == "personal-owner" {
				input := map[string]any{"project_id": f.project, "name": "Controlled personal pool", "kind": "personal", "owner_user_id": memberID, "account_ids": []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string)}, "api_key_ids": []string{f.keyID}}
				f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), input, etagHeader(f.pool), 200)
			}
			f.translateMessages(t)
			header := http.Header{"X-Claude-Code-Session-Id": {"translated-authority"}}
			body := []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"controlled"}]}`)
			response, out := f.clientRequest(t, 0, "v1/messages", header, body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("initial translated authority status %d body %s", response.StatusCode, out)
			}
			switch target {
			case "pool-account":
				binding := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))[0]
				other := func() string {
					if f.accounts[0]["id"] != binding.AccountID {
						return f.accounts[0]["id"].(string)
					}
					return f.accounts[1]["id"].(string)
				}()
				input := map[string]any{"project_id": f.project, "name": "Controlled changed pool", "kind": "shared", "owner_user_id": nil, "account_ids": []string{other}, "api_key_ids": []string{f.keyID}}
				f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), input, etagHeader(f.pool), 200)
			case "personal-owner":
				owner := f.h.want(f.owner, "GET", "/api/v1/profile", nil, nil, 200)
				input := map[string]any{"project_id": f.project, "name": "Controlled changed owner", "kind": "personal", "owner_user_id": owner["id"], "account_ids": []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string)}, "api_key_ids": []string{f.keyID}}
				f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), input, etagHeader(f.pool), 200)
			default:
				f.revoke(t, target, key, memberID)
			}
			child := header.Clone()
			child.Set("X-Claude-Code-Agent-Id", "revoked-child")
			for i, identity := range []http.Header{header, child} {
				response, out := f.clientRequest(t, i, "v1/messages", identity, body)
				if response.StatusCode < 400 || !bytes.Contains(out, []byte("code_")) || len(f.peer.Requests()) != 1 {
					t.Fatalf("revoked translated request reached upstream, status %d body %s", response.StatusCode, out)
				}
			}
		})
	}
}
