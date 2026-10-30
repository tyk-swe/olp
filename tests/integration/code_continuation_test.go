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
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/testutil"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestCodeQualificationGenericAffinityPreservesAuthority(t *testing.T) {
	f := newCodeFleet(t)
	second := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Second coding key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("generic-second-key"), 201)
	foreignProject := createProject(f.h, f.owner, "Other coding project")
	foreign := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Other project key", "project_id": foreignProject, "scopes": []string{"inference"}, "allowed_routes": []string{}}, idem("generic-foreign-key"), 201)
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput([]string{f.keyID, second["id"].(string)}), etagHeader(f.pool), 200)
	if err := f.gateways[1].Stop(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	f.gateways[1] = f.in.replica("generic-keys")
	request := func(key, affinity string, body []byte, status int) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.gateways[1].PublicOrigin+"/code/qualification/responses", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Olp-Code-Affinity", affinity)
		response, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status {
			t.Fatalf("generic authority request returned %d body %s error %v", response.StatusCode, data, err)
		}
	}
	request(f.key, "same-client-affinity", codeBody(), http.StatusOK)
	continuation := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-controlled","input":[]}`)
	request(f.key, "other-affinity", continuation, http.StatusConflict)
	recognizedResponse, recognizedBody := f.clientRequest(t, 1, "responses", http.Header{"Thread-Id": {"recognized-adopter"}}, continuation)
	if recognizedResponse.StatusCode != http.StatusConflict {
		t.Fatalf("recognized identity adopted generic state, status %d body %s", recognizedResponse.StatusCode, recognizedBody)
	}
	request(second["secret"].(string), "same-client-affinity", continuation, http.StatusConflict)
	if len(f.peer.Requests()) != 1 {
		t.Fatal("another key reused a generic response reference")
	}
	request(second["secret"].(string), "same-client-affinity", codeBody(), http.StatusOK)
	request(f.key, "same-client-affinity", continuation, http.StatusOK)
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 2 || bindings[0].RootID == bindings[1].RootID || bindings[0].APIKeyID == bindings[1].APIKeyID || bindings[0].Conversation != bindings[1].Conversation {
		t.Fatal("equal generic affinity escaped API-key isolation")
	}
	request(foreign["secret"].(string), "same-client-affinity", codeBody(), http.StatusForbidden)
	if len(f.peer.Requests()) != 3 {
		t.Fatal("another project's generic affinity reached upstream")
	}
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-recognized","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`)}
	})
	rootResponse, rootBody := f.clientRequest(t, 1, "responses", http.Header{"Thread-Id": {"recognized-root"}}, codeBody())
	if rootResponse.StatusCode != http.StatusOK {
		t.Fatalf("recognized root refused, status %d body %s", rootResponse.StatusCode, rootBody)
	}
	request(f.key, "recognized-root-adopter", []byte(`{"model":"gpt-5.4","previous_response_id":"resp-recognized","input":[]}`), http.StatusConflict)
	if len(f.peer.Requests()) != 4 || len(codePublicDecode[[]codemode.Binding](t, f.list("bindings"))) != 3 {
		t.Fatal("generic affinity adopted a recognized root")
	}
}

func TestCodeQualificationPortableEstablishedRecoveryPreservesOwners(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 1)
	var executions atomic.Int64
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		if executions.Add(1) == 1 {
			return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-before-recovery","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)}
		}
		return nil
	})
	initial, _, initialErr := f.request(t.Context(), 0, "portable-established", "", codeBody(), true)
	if initialErr != nil || initial.StatusCode != http.StatusOK {
		t.Fatal("initial established request failed", initialErr)
	}
	original := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))[0]
	originalAttempt := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))[0]
	f.peer.Faults("initial_quota")
	response, body, recoveryErr := f.request(t.Context(), 1, "portable-established", "", codeBody(), true)
	if recoveryErr != nil || response.StatusCode != http.StatusOK || string(body) != codexfixture.SSE {
		t.Fatalf("portable recovery response=%v body=%s err=%v", response, body, recoveryErr)
	}
	captures := f.peer.Requests()
	if len(captures) != 3 || executions.Load() != 2 || captures[0].Header.Get("Authorization") != captures[1].Header.Get("Authorization") || captures[1].Header.Get("Authorization") == captures[2].Header.Get("Authorization") {
		t.Fatal("established rejection did not recover exactly once on another account")
	}
	for _, capture := range captures {
		if !bytes.Equal(capture.Body, codeBody()) {
			t.Fatal("established recovery changed native model or payload")
		}
	}
	recovered := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))[0]
	if recovered.ID != original.ID || recovered.AccountID == original.AccountID {
		t.Fatal("established recovery lost durable identity or did not move account")
	}
	attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
	if len(attempts) != 3 {
		t.Fatalf("recovery attempts=%d", len(attempts))
	}
	for _, attempt := range attempts {
		if attempt.BindingID != original.ID || attempt.ReportedTokens == nil {
			t.Fatal("established recovery lost settlement or binding attribution")
		}
		if attempt.ID == originalAttempt.ID && attempt.AccountID != originalAttempt.AccountID {
			t.Fatal("migration changed an executed turn's historical account")
		}
		if attempt.UpstreamStatus != nil && *attempt.UpstreamStatus == http.StatusTooManyRequests && (attempt.AccountID != original.AccountID || *attempt.ReportedTokens != 0) {
			t.Fatal("proven rejected turn changed historical owner or consumed tokens")
		}
	}
	f.restart(t, "portable-recovery")
	previous := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-before-recovery","input":[]}`)
	foreign, guidance, ownershipErr := f.request(t.Context(), 0, "portable-established", "", previous, true)
	if ownershipErr != nil || foreign.StatusCode != http.StatusConflict || !bytes.Contains(guidance, []byte("code_response_account_conflict")) || len(f.peer.Requests()) != len(captures) {
		t.Fatalf("old response owner changed response=%v body=%s err=%v", foreign, guidance, ownershipErr)
	}
	f.success(t, 1, "portable-child", "portable-established")
	for _, binding := range codePublicDecode[[]codemode.Binding](t, f.list("bindings")) {
		if binding.RootID != recovered.RootID || binding.AccountID != recovered.AccountID {
			t.Fatal("new child did not inherit recovered authority across replicas")
		}
	}
	for _, collection := range []string{"attempts", "bindings", "refusals"} {
		_, diagnostic := f.h.do(f.owner, "GET", "/api/v1/code/"+collection+"?project_id="+f.project, nil, nil)
		if bytes.Contains(diagnostic, []byte("CONTROLLED_PRIVATE_PROMPT")) {
			t.Fatalf("%s persisted portable input", collection)
		}
	}
}

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

func (f *codeFleet) restart(t *testing.T, name string) {
	t.Helper()
	for i, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		f.gateways[i] = f.in.replica(fmt.Sprintf("%s-%d", name, i))
	}
}

func (f *codeFleet) translateMessages(t *testing.T, poolID string) map[string]any {
	t.Helper()
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": poolID, "models": []string{"gpt-5.4"}, "enabled": true, "messages_to_codex": true}, etagHeader(f.route), 200)
	route := f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("translated-continuity")), 200)
	f.restart(t, "translated")
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		return [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp-translated","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"CONTROLLED_PRIVATE_OUTPUT"}]}],"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}`)}
	})
	return route
}

func TestCodeQualificationTranslatedLineageColdResumeAndSummaryCompaction(t *testing.T) {
	f := newCodeFleet(t)
	f.translateMessages(t, f.pool["id"].(string))
	header := http.Header{"X-Claude-Code-Session-Id": {"translated-session"}}
	toolHistory := []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"},{"role":"assistant","content":[{"type":"tool_use","id":"call-owned","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-owned","content":"CONTROLLED_PRIVATE_TOOL"}]}]}`)
	for i, requestHeader := range []http.Header{
		header,
		{"X-Claude-Code-Session-Id": {"translated-session"}, "X-Claude-Code-Agent-Id": {"child"}},
		{"X-Claude-Code-Session-Id": {"translated-session"}, "X-Claude-Code-Agent-Id": {"grandchild"}, "X-Claude-Code-Parent-Agent-Id": {"child"}},
	} {
		response, body := f.clientRequest(t, i%2, "v1/messages", requestHeader, toolHistory)
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("CONTROLLED_PRIVATE_OUTPUT")) {
			t.Fatalf("translated lineage status %d body %s", response.StatusCode, body)
		}
	}
	f.restart(t, "translated-cold")
	child := http.Header{"X-Claude-Code-Session-Id": {"translated-session"}, "X-Claude-Code-Agent-Id": {"child"}}
	response, body := f.clientRequest(t, 1, "v1/messages", child, toolHistory)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cold child resume status %d body %s", response.StatusCode, body)
	}
	summary := []byte(`{"model":"gpt-5.4","max_tokens":64,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_SUMMARY"}]}`)
	summaryResponse, summaryBody := f.clientRequest(t, 0, "v1/messages", child, summary)
	if summaryResponse.StatusCode != http.StatusOK {
		t.Fatalf("summary continuation status %d body %s", summaryResponse.StatusCode, summaryBody)
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
	opaqueResponse, opaqueBody := f.clientRequest(t, 1, "v1/messages", child, []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"CONTROLLED_PRIVATE_REASONING","signature":"CONTROLLED_PRIVATE_SIGNATURE"}]},{"role":"user","content":"Continue"}]}`))
	if opaqueResponse.StatusCode != http.StatusUnprocessableEntity || !bytes.Contains(opaqueBody, []byte("code_translation_unsupported_semantics")) || len(f.peer.Requests()) != len(captures) {
		t.Fatalf("opaque continuation dispatched or lost state, status %d body %s", opaqueResponse.StatusCode, opaqueBody)
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
	translatedRoute := f.translateMessages(t, f.pool["id"].(string))
	plans := &codingPlanFixture{h: f.h, owner: f.owner, project: f.project, peer: newFixedHostPeer(t, "opencode.ai"), digest: installPlugin(t, f.h, f.owner, testutil.BuildPlugin(t, "./plugins/opencode-go"))}
	provider := plans.provider(t, "Controlled other family", codeplans.OpenCodeGoProfile)
	credential := plans.enroll(t, provider, "opencode0123456789abcdef0123456789")
	account := f.h.want(f.owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Controlled response owner", "enabled": true, "models": []string{"gpt-5.5"}}, idem("reference-owner"), 201)
	accounts := []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string), account["id"].(string)}
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), map[string]any{"project_id": f.project, "name": "Controlled mixed pool", "kind": "shared", "owner_user_id": nil, "account_ids": accounts, "api_key_ids": []string{f.keyID}}, etagHeader(f.pool), 200)
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4", "gpt-5.5"}, "enabled": true, "messages_to_codex": true}, etagHeader(translatedRoute), 200)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("reference-owner-publish")), 200)
	f.restart(t, "reference-owner")
	header := http.Header{"X-Claude-Code-Session-Id": {"owned-translated-response"}}
	response, body := f.clientRequest(t, 0, "v1/messages", header, []byte(`{"model":"gpt-5.4","max_tokens":64,"messages":[{"role":"user","content":"controlled"}]}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("translated response status %d body %s", response.StatusCode, body)
	}
	foreignResponse, foreignBody := f.clientRequest(t, 0, "v1/responses", header, []byte(`{"model":"gpt-5.5","previous_response_id":"resp-translated","input":[]}`))
	if foreignResponse.StatusCode != http.StatusConflict || !bytes.Contains(foreignBody, []byte("code_response_account_conflict")) || len(plans.peer.received()) != 0 {
		t.Fatalf("foreign response reached another account, status %d body %s", foreignResponse.StatusCode, foreignBody)
	}
	nativeHeader := http.Header{"Thread-Id": {"owned-translated-response"}, "Session-Id": {"owned-translated-response"}}
	nativeBody := []byte(`{"model":"gpt-5.4","previous_response_id":"resp-translated","input":[]}`)
	nativeResponse, nativeOutput := f.clientRequest(t, 0, "responses", nativeHeader, nativeBody)
	if nativeResponse.StatusCode != http.StatusOK || len(f.peer.Requests()) != 2 || !bytes.Equal(f.peer.Requests()[1].Body, nativeBody) {
		t.Fatalf("owned native continuation changed or refused, status %d body %s", nativeResponse.StatusCode, nativeOutput)
	}
	plans.peer.mu.Lock()
	plans.peer.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.ReplaceAll(codingPlanResponsesSSE, "resp_fixture", "resp-translated"))
	}
	plans.peer.mu.Unlock()
	recoveryResponse, recoveryBody := f.clientRequest(t, 0, "v1/responses", header, []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"full controlled context"}]}`))
	if recoveryResponse.StatusCode != http.StatusOK || len(plans.peer.received()) != 1 {
		t.Fatalf("full-context recovery status %d body %s", recoveryResponse.StatusCode, recoveryBody)
	}
	ambiguousResponse, ambiguousBody := f.clientRequest(t, 0, "responses", nativeHeader, nativeBody)
	if ambiguousResponse.StatusCode != http.StatusConflict || !bytes.Contains(ambiguousBody, []byte("code_parent_unresolved")) || len(f.peer.Requests()) != 2 {
		t.Fatalf("ambiguous response ownership dispatched, status %d body %s", ambiguousResponse.StatusCode, ambiguousBody)
	}
}

func TestCodeQualificationTranslatedContinuationRechecksLiveAuthority(t *testing.T) {
	for _, target := range []string{"pool", "account", "key", "project", "retirement", "pool-account", "personal-owner"} {
		t.Run(target, func(t *testing.T) {
			f := newCodeFleet(t)
			key, memberID := f.memberKey(t)
			pool := func() map[string]any {
				if target != "personal-owner" {
					return f.pool
				}
				input := map[string]any{"project_id": f.project, "name": "Controlled personal pool", "kind": "personal", "owner_user_id": memberID, "account_ids": []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string)}, "api_key_ids": []string{f.keyID}}
				return f.h.want(f.owner, "POST", "/api/v1/code/pools", input, idem("translated-personal-pool"), 201)
			}()
			f.translateMessages(t, pool["id"].(string))
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
				input := map[string]any{"project_id": f.project, "name": "Controlled changed owner", "kind": "personal", "owner_user_id": owner["id"], "account_ids": []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string)}, "api_key_ids": []string{}}
				poolPath := "/api/v1/code/pools/" + pool["id"].(string)
				transferResponse, refusal := f.h.do(f.owner, "PUT", poolPath, input, etagHeader(pool))
				if transferResponse.StatusCode != http.StatusUnprocessableEntity || !bytes.Contains(refusal, []byte("ownership is immutable")) {
					t.Fatalf("personal pool owner transfer status %d body %s", transferResponse.StatusCode, refusal)
				}
				pools := codePublicDecode[[]contract.CodePool](t, f.list("pools"))
				index := slices.IndexFunc(pools, func(candidate contract.CodePool) bool { return candidate.Id.String() == pool["id"] })
				if index < 0 {
					t.Fatal("personal pool disappeared after refused transfer")
				}
				unchanged := pools[index]
				ownerID, ownerErr := unchanged.OwnerUserId.Get()
				if ownerErr != nil || ownerID.String() != memberID || unchanged.Etag.String() != pool["etag"] {
					t.Fatal("personal pool owner or revision changed after refused transfer")
				}
				removedKey := map[string]any{"project_id": f.project, "name": "Controlled personal membership removed", "kind": "personal", "owner_user_id": memberID, "account_ids": []string{f.accounts[0]["id"].(string), f.accounts[1]["id"].(string)}, "api_key_ids": []string{}}
				f.h.want(f.owner, "PUT", poolPath, removedKey, map[string]string{"If-Match": fmt.Sprintf("%q", unchanged.Etag.String())}, 200)
			default:
				f.revoke(t, target, key, memberID)
			}
			child := http.Header{"X-Claude-Code-Session-Id": {"translated-authority"}, "X-Claude-Code-Agent-Id": {"revoked-child"}}
			for i, identity := range []http.Header{header, child} {
				response, out := f.clientRequest(t, i, "v1/messages", identity, body)
				if response.StatusCode < 400 || !bytes.Contains(out, []byte("code_")) || len(f.peer.Requests()) != 1 {
					t.Fatalf("revoked translated request reached upstream, status %d body %s", response.StatusCode, out)
				}
			}
		})
	}
}
