//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/codecli"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

type codeFleet struct {
	*codePublicFixture
	in       *fleetInstall
	gateways []*testutil.Process
}

func newCodeFleet(t *testing.T) *codeFleet {
	t.Helper()
	f := newCodePublicFixtureWithPeer(t, codexfixture.New())
	installation, err := database.Installation(t.Context(), f.h.Pool)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, value := range map[string]string{"auth": f.h.AuthHex, "ring": f.h.Ring} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	in := &fleetInstall{t: t, h: f.h, owner: f.owner, secrets: dir, binary: required(t, "OLP_TEST_BINARY"), prefix: database.ValkeyNamespace(installation)}
	in.valkey = client(t, required(t, "OLP_TEST_VALKEY_URL"), 5*time.Second)
	t.Cleanup(func() { fleetPurge(t, in.valkey, in.prefix) })
	result := &codeFleet{codePublicFixture: f, in: in}
	result.gateways = []*testutil.Process{in.replica("code-one"), in.replica("code-two")}
	f.noSyntheticInference(t)
	return result
}

func codeBody() []byte {
	return []byte("{ \"model\" : \"gpt-5.4\", \"stream\":true, \"store\":false, \"input\":[{\"role\":\"user\",\"content\":\"CONTROLLED_PRIVATE_PROMPT\"}], \"metadata\":{\"unknown_future_field\":\"\\u0041\"} }\n")
}

func (f *codeFleet) request(ctx context.Context, replica int, conversation, parent string, body []byte, portable ...bool) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", f.gateways[replica].PublicOrigin+"/code/qualification/responses?fixture=opaque%2Fquery", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", conversation)
	req.Header.Set("Session-Id", conversation)
	if parent != "" {
		req.Header.Set("X-Codex-Parent-Thread-Id", parent)
	}
	req.Header["X-Controlled-Multi"] = []string{"one", "two"}
	if len(portable) == 0 || !portable[0] {
		req.Header.Set("X-Codex-Turn-State", "opaque-state")
	}
	req.Header.Set("Connection", "X-Controlled-Hop")
	req.Header.Set("X-Controlled-Hop", "must-not-forward")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	out, err := io.ReadAll(response.Body)
	return response, out, err
}

func (f *codeFleet) success(t *testing.T, replica int, conversation, parent string) {
	t.Helper()
	response, body, err := f.request(t.Context(), replica, conversation, parent, codeBody())
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		t.Fatalf("integrated /code transport/authorizer required: status=%d body=%s", response.StatusCode, body)
	}
	if string(body) != codexfixture.SSE {
		t.Fatal("response bytes changed")
	}
	if !reflect.DeepEqual(response.Header.Values("X-Controlled-Multi"), []string{"first", "second"}) || response.Header.Get("X-Request-Id") != "controlled-upstream-request" {
		t.Fatal("end-to-end response headers changed")
	}
}

func (f *codeFleet) refused(t *testing.T, replica int, conversation, parent string) {
	t.Helper()
	before := len(f.peer.Requests())
	response, body, err := f.request(t.Context(), replica, conversation, parent, codeBody())
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 400 || response.StatusCode > 503 || !bytes.Contains(body, []byte("code_")) {
		t.Fatalf("expected typed refusal: %d %s", response.StatusCode, body)
	}
	if len(f.peer.Requests()) != before {
		t.Fatal("refused generation reached upstream")
	}
}

func TestCodeQualificationFleetWeightedDistribution(t *testing.T) {
	f := newCodeFleet(t)
	pool := codeDiscoveryResponse[contract.CodePool](t, f.pool)
	first := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[0]).Id.String()
	second := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[1]).Id.String()
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+pool.Id.String(), codePoolWeightInput(pool, map[string]int64{first: 5, second: 1}), etagHeader(f.pool), 200)
	for i := range 200 {
		f.success(t, i%2, fmt.Sprintf("weighted-%d", i), "")
	}
	firstCount := len(slices.Collect(func(yield func(codexfixture.Request) bool) {
		for _, request := range f.peer.Requests() {
			if request.Header.Get("Authorization") == "Bearer "+f.codexAuthorities[0].accessToken && !yield(request) {
				return
			}
		}
	}))
	if firstCount < 140 || firstCount > 190 {
		t.Fatalf("5:1 weights selected the first account %d times in 200 new roots", firstCount)
	}
}

func TestCodeQualificationFleetWeightsPreserveBindingsAndEligibility(t *testing.T) {
	f := newCodeFleet(t)
	pool := codeDiscoveryResponse[contract.CodePool](t, f.pool)
	first := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[0]).Id.String()
	secondAccount := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[1])
	second := secondAccount.Id.String()
	poolPath := "/api/v1/code/pools/" + pool.Id.String()
	firstHeavyResponse := f.h.want(f.owner, "PUT", poolPath, codePoolWeightInput(pool, map[string]int64{first: 2147483647, second: 1}), etagHeader(f.pool), 200)
	firstHeavy := codeDiscoveryResponse[contract.CodePool](t, firstHeavyResponse)
	for i := range 4 {
		f.success(t, i%2, fmt.Sprintf("first-heavy-%d", i), "")
	}
	firstBindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	for _, binding := range firstBindings {
		if binding.AccountID != first {
			t.Fatal("new roots did not follow account weights")
		}
	}
	f.h.want(f.owner, "PUT", poolPath, codePoolWeightInput(firstHeavy, map[string]int64{first: 1, second: 2147483647}), etagHeader(firstHeavyResponse), 200)
	f.success(t, 1, "first-heavy-0", "")
	f.success(t, 0, "weighted-child", "first-heavy-0")
	for i := range 4 {
		f.success(t, i%2, fmt.Sprintf("second-heavy-%d", i), "")
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	for _, binding := range bindings {
		if strings.HasPrefix(binding.Conversation, "second-heavy-") {
			if binding.AccountID != second {
				t.Fatal("new roots ignored updated weight")
			}
			continue
		}
		if binding.AccountID != first {
			t.Fatal("weight update moved a durable binding")
		}
	}
	accountPath := "/api/v1/code/accounts/" + second
	otherModelResponse := f.h.want(f.owner, "PUT", accountPath, codeDiscoveryAccountInput(secondAccount, true, []string{"gpt-5.4-mini"}), etagHeader(f.accounts[1]), 200)
	otherModel := codeDiscoveryResponse[contract.CodeAccount](t, otherModelResponse)
	f.success(t, 0, "exact-model-root", "")
	f.h.want(f.owner, "PUT", accountPath, codeDiscoveryAccountInput(otherModel, true, []string{"gpt-5.4"}), etagHeader(otherModelResponse), 200)
	f.peer.Mode("quota")
	response, _, err := f.request(t.Context(), 1, "second-heavy-0", "", codeBody())
	if err != nil || response.StatusCode != 429 {
		t.Fatalf("quota response: %v %v", response, err)
	}
	f.peer.Mode("")
	f.success(t, 1, "available-root", "")
	f.refused(t, 0, "second-heavy-0", "")
	latest := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	for _, binding := range latest {
		if (binding.Conversation == "available-root" || binding.Conversation == "exact-model-root") && binding.AccountID != first {
			t.Fatal("weight overrode account eligibility")
		}
	}
}

func TestCodeQualificationFleetWeightedPersonalAndSharedIsolation(t *testing.T) {
	f := newCodeFleet(t)
	pool := codeDiscoveryResponse[contract.CodePool](t, f.pool)
	firstAccount := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[0])
	first := firstAccount.Id.String()
	second := codeDiscoveryResponse[contract.CodeAccount](t, f.accounts[1]).Id.String()
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+pool.Id.String(), codePoolWeightInput(pool, map[string]int64{first: 1, second: 2147483647}), etagHeader(f.pool), 200)
	owner := codeDiscoveryResponse[contract.UserDetailResponse](t, f.h.want(f.owner, "GET", "/api/v1/profile", nil, nil, 200))
	key := codeDiscoveryResponse[contract.CreateApiKeyResponse](t, f.h.want(f.owner, "POST", "/api/v1/api-keys", contract.CreateApiKeyRequest{
		ProjectId: nullable.NewNullableWithValue(pool.ProjectId), Name: "Personal key", Scopes: &[]string{"inference"},
		AllowedRoutes: &[]string{"qualification", "qualification-personal"},
	}, idem("weighted-personal-key"), 201))
	personal := codeDiscoveryResponse[contract.CodePool](t, f.h.want(f.owner, "POST", "/api/v1/code/pools", contract.CodePoolWrite{
		ProjectId: pool.ProjectId, Name: "Weighted personal", Kind: contract.CodePoolWriteKindPersonal, OwnerUserId: nullable.NewNullableWithValue(owner.Id),
		AccountIds: []uuid.UUID{firstAccount.Id}, AccountWeights: &map[string]int64{first: 1}, ApiKeyIds: []uuid.UUID{key.Id},
	}, idem("weighted-personal-pool"), 201))
	draftResponse := f.h.want(f.owner, "POST", "/api/v1/code/routes", contract.CodeRouteWrite{
		ProjectId: pool.ProjectId, Slug: "qualification-personal", PoolId: personal.Id,
		Models: []string{"gpt-5.4"}, Enabled: true,
	}, idem("weighted-personal-route"), 201)
	draft := codeDiscoveryResponse[contract.CodeRoute](t, draftResponse)
	f.h.want(f.owner, "POST", "/api/v1/code/routes/"+draft.Id.String()+"/publish", nil, withMatch(draftResponse, idem("weighted-personal-publish")), 200)
	for _, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	updatedFleet := &codeFleet{codePublicFixture: f.codePublicFixture, in: f.in, gateways: []*testutil.Process{f.in.replica("weighted-pools-0"), f.in.replica("weighted-pools-1")}}
	for i := range 2 {
		updatedFleet.success(t, i, fmt.Sprintf("shared-isolated-%d", i), "")
		req, err := http.NewRequestWithContext(t.Context(), "POST", updatedFleet.gateways[i].PublicOrigin+"/code/qualification-personal/responses", bytes.NewReader(codeBody()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key.Secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", fmt.Sprintf("personal-isolated-%d", i))
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil || response.StatusCode != 200 || string(body) != codexfixture.SSE {
			t.Fatalf("personal pool request status %d, read error %v", response.StatusCode, readErr)
		}
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 4 {
		t.Fatalf("expected four independent pool bindings, got %d", len(bindings))
	}
	for _, binding := range bindings {
		if strings.HasPrefix(binding.Conversation, "personal-") {
			if binding.AccountID != first {
				t.Fatal("personal pool selected the weighted shared account")
			}
			continue
		}
		if binding.AccountID != second {
			t.Fatal("shared pool selected outside its weighted membership")
		}
	}
	for _, binding := range bindings {
		if !strings.HasPrefix(binding.Conversation, "personal-") {
			continue
		}
		path := "/api/v1/code/attempts?binding_id=" + binding.ID
		visible := codePublicDecode[[]codemode.Attempt](t, f.h.want(f.owner, "GET", path+"&project_id="+f.project, nil, nil, 200)["items"])
		if len(visible) != 1 || visible[0].BindingID != binding.ID || visible[0].AccountID != first {
			t.Fatal("personal-pool diagnostics lost account or binding attribution")
		}
		invitationSession := f.h.invite(f.owner, "diagnostics-outsider@example.test", "operator")
		profile := codePublicDecode[contract.UserDetailResponse](t, f.h.want(invitationSession, "GET", "/api/v1/profile", nil, nil, 200))
		f.assignOnly(profile.Id.String())
		assignedSession := login(f.h, "diagnostics-outsider@example.test")
		f.h.want(assignedSession, "GET", path+"&project_id="+f.project, nil, nil, 404)
		invisible := f.h.want(assignedSession, "GET", path, nil, nil, 200)["items"].([]any)
		if len(invisible) != 0 {
			t.Fatal("personal-pool diagnostics crossed project visibility")
		}
		break
	}
}

func TestCodeQualificationFleetAtomicTreeRestartAndRetirement(t *testing.T) {
	f := newCodeFleet(t)
	const requests = 12
	var wg sync.WaitGroup
	errs := make(chan error, requests)
	for i := range requests {
		wg.Go(func() {
			response, body, err := f.request(t.Context(), i%2, "root-controlled", "", codeBody())
			if err == nil && (response.StatusCode != 200 || string(body) != codexfixture.SSE) {
				err = fmt.Errorf("replica %d returned %d (integrated transport/authorizer required)", i%2, response.StatusCode)
			}
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	f.success(t, 0, "child-controlled", "root-controlled")
	f.success(t, 1, "grandchild-controlled", "child-controlled")
	f.refused(t, 1, "orphan-controlled", "missing-parent")
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 3 {
		t.Fatalf("bindings=%d", len(bindings))
	}
	root := bindings[0].RootID
	account := bindings[0].AccountID
	for _, binding := range bindings {
		if binding.RootID != root || binding.AccountID != account {
			t.Fatal("conversation tree changed pin")
		}
	}
	if err := f.gateways[0].Kill(); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("code-restarted")
	f.success(t, 0, "grandchild-controlled", "child-controlled")
	captures := f.peer.Requests()
	if len(captures) != requests+3 {
		t.Fatalf("dispatch count %d: replay or loss", len(captures))
	}
	for _, capture := range captures {
		if !bytes.Equal(capture.Body, codeBody()) || !strings.HasSuffix(capture.Path, "?fixture=opaque%2Fquery") {
			t.Fatal("request payload/query changed")
		}
		if !reflect.DeepEqual(capture.Header.Values("X-Controlled-Multi"), []string{"one", "two"}) || capture.Header.Get("X-Codex-Turn-State") != "opaque-state" {
			t.Fatal("end-to-end request headers changed")
		}
		if capture.Header.Get("X-Controlled-Hop") != "" || strings.Contains(capture.Header.Get("Authorization"), f.key) {
			t.Fatal("hop-by-hop or OLP credential leaked")
		}
		if capture.Header.Get("Authorization") != captures[0].Header.Get("Authorization") {
			t.Fatal("upstream account switched")
		}
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+root+"/retire", nil, idem("retire-root"), 200)
	f.refused(t, 0, "root-controlled", "")
	f.refused(t, 1, "grandchild-controlled", "child-controlled")
	for _, collection := range []string{"attempts", "bindings", "refusals"} {
		_, raw := f.h.do(f.owner, "GET", "/api/v1/code/"+collection+"?project_id="+f.project, nil, nil)
		for _, forbidden := range []string{"CONTROLLED_PRIVATE_PROMPT", "CONTROLLED_PRIVATE_OUTPUT", "opaque-state", f.key} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("%s stored private payload/header data", collection)
			}
		}
	}
}

func TestCodeQualificationFleetNoFailoverAndUsageUncertainty(t *testing.T) {
	for _, mode := range []string{"unavailable", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			f := newCodeFleet(t)
			f.success(t, 0, "uncertain-root", "")
			f.peer.Mode(mode)
			response, body, err := f.request(t.Context(), 1, "uncertain-root", "", codeBody())
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unavailable" && response.StatusCode != 503 {
				t.Fatalf("upstream status changed: %d", response.StatusCode)
			}
			if mode == "unavailable" && string(body) != `{"error":{"code":"controlled_unavailable"}}` {
				t.Fatal("upstream error payload changed")
			}
			captures := f.peer.Requests()
			if len(captures) != 2 || captures[0].Header.Get("Authorization") != captures[1].Header.Get("Authorization") {
				t.Fatal("inference retried, failed over or changed account")
			}
			attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
			if len(attempts) != 2 {
				t.Fatalf("attempts=%d", len(attempts))
			}
			unknown := 0
			for _, a := range attempts {
				if a.ReportedTokens == nil && a.State == "uncertain" {
					unknown++
				}
			}
			if unknown != 1 {
				t.Fatal("missing final usage was not preserved as uncertainty")
			}
		})
	}
}

func TestCodeQualificationWebSocketEveryGenerationRechecksAuthority(t *testing.T) {
	for _, target := range []string{"pool", "account", "key", "project", "retirement"} {
		t.Run(target, func(t *testing.T) {
			f := newCodeFleet(t)
			key, memberID := f.memberKey(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			url := strings.Replace(f.gateways[0].PublicOrigin, "http", "ws", 1) + "/code/qualification/responses"
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.key}, "Thread-Id": []string{"ws-root"}, "Session-Id": []string{"ws-root"}, "Openai-Beta": []string{"responses_websockets=2026-02-06"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			body := []byte(`{"type":"response.create","model":"gpt-5.4","input":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"}]}`)
			if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
				t.Fatal(err)
			}
			events, err := codexfixture.ReadGeneration(ctx, conn)
			if err != nil || len(events) != 4 {
				t.Fatalf("generation: %d events %v", len(events), err)
			}
			var expected [][]byte
			for _, line := range strings.Split(codexfixture.SSE, "\n") {
				if data, ok := strings.CutPrefix(line, "data: "); ok {
					expected = append(expected, []byte(data))
				}
			}
			if !reflect.DeepEqual(events, expected) {
				t.Fatal("WebSocket response payload/order changed")
			}
			requests := f.peer.Requests()
			if len(requests) != 1 || !bytes.Equal(requests[0].Body, body) {
				t.Fatal("WebSocket frame rewritten/replayed")
			}
			f.revoke(t, target, key, memberID)
			if err := conn.Write(ctx, websocket.MessageText, body); err == nil {
				_, event, readErr := conn.Read(ctx)
				if errors.Is(readErr, context.DeadlineExceeded) {
					t.Fatal("revoked generation hung instead of refusing")
				}
				if readErr == nil && !bytes.Contains(event, []byte("code_")) {
					t.Fatal("second generation was not refused")
				}
			}
			if len(f.peer.Requests()) != 1 {
				t.Fatal("WebSocket authority cached at upgrade")
			}
		})
	}
}

func (f *codeFleet) memberKey(t *testing.T) (map[string]any, string) {
	t.Helper()
	member := f.h.invite(f.owner, "code-issuer@example.test", "operator")
	memberID := f.h.want(member, "GET", "/api/v1/profile", nil, nil, 200)["id"].(string)
	f.assignOnly(memberID)
	member = login(f.h, "code-issuer@example.test")
	addMember(f.h, f.owner, f.project, memberID, "manager")
	key := f.h.want(member, "POST", "/api/v1/api-keys", map[string]any{"name": "Member key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("member-key"), 201)
	f.keyID, f.key = key["id"].(string), key["secret"].(string)
	key = f.h.want(member, "GET", "/api/v1/api-keys/"+f.keyID, nil, nil, 200)
	f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput([]string{f.keyID}), etagHeader(f.pool), 200)
	if err := f.gateways[0].Kill(); err != nil {
		t.Fatal(err)
	}
	f.gateways[0] = f.in.replica("code-member")
	return key, memberID
}

func (f *codeFleet) revoke(t *testing.T, target string, key map[string]any, memberID string) {
	t.Helper()
	switch target {
	case "pool":
		f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput([]string{}), etagHeader(f.pool), 200)
	case "account":
		bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
		for _, account := range f.accounts {
			if account["id"] == bindings[0].AccountID {
				body := map[string]any{"project_id": f.project, "provider_id": account["provider_id"], "credential_id": account["credential_id"], "name": account["name"], "enabled": false, "models": account["models"]}
				f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+account["id"].(string), body, etagHeader(account), 200)
				return
			}
		}
		t.Fatal("bound account missing")
	case "key":
		f.h.want(f.owner, "POST", "/api/v1/api-keys/"+f.keyID+"/revoke", nil, withMatch(key, idem("revoke-key")), 200)
	case "project":
		f.h.want(f.owner, "DELETE", "/api/v1/projects/"+f.project+"/members/"+memberID, nil, projectEtag(f.h, f.owner, f.project), 204)
	case "retirement":
		bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
		f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+bindings[0].RootID+"/retire", nil, idem("retire-websocket"), 200)
	default:
		t.Fatalf("unknown revocation target %q", target)
	}
}

func TestCodeQualificationUnknownBoundRefusesOnlyBudgetedOperation(t *testing.T) {
	f := newCodeFleet(t)
	input := map[string]any{"project_id": f.project, "route_id": f.route["id"], "api_key_id": f.keyID, "daily_tokens": 1000, "monthly_tokens": 10000, "enabled": true}
	budget := f.h.want(f.owner, "POST", "/api/v1/code/budgets", input, idem("hard-budget"), 201)
	response, body, err := f.request(t.Context(), 0, "budget-root", "", codeBody())
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 422 || !bytes.Contains(body, []byte("code_token_bound_unavailable")) {
		t.Fatalf("unproven native bound accepted: %d %s", response.StatusCode, body)
	}
	if len(f.peer.Requests()) != 0 {
		t.Fatal("budgeted request dispatched without a qualified bound")
	}
	input["enabled"] = false
	f.h.want(f.owner, "PUT", "/api/v1/code/budgets/"+budget["id"].(string), input, etagHeader(budget), 200)
	f.success(t, 1, "budget-root", "")
}

func TestCodeQualificationOfficialCLIThroughProcess(t *testing.T) {
	f := newCodeFleet(t)
	for _, ws := range []bool{false, true} {
		client := codecli.New(t, f.gateways[0].PublicOrigin+"/code/qualification", f.key, ws)
		client.Exec(t, "Reply with the controlled fixture result.")
		client.Exec(t, "resume", "--last", "Continue the same conversation.")
	}
	if len(f.list("bindings")) != 2 {
		t.Fatal("official CLI resume did not reuse root bindings")
	}
}

func TestCodeQualificationFleetLiveAccountKeyAndProjectRevocation(t *testing.T) {
	for _, target := range []string{"account", "key", "project"} {
		t.Run(target, func(t *testing.T) {
			f := newCodeFleet(t)
			key, memberID := f.memberKey(t)
			f.success(t, 0, "revocation-root", "")
			f.revoke(t, target, key, memberID)
			response, _, err := f.request(t.Context(), 0, "revocation-root", "", codeBody())
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode < 400 || response.StatusCode == 404 || len(f.peer.Requests()) != 1 {
				t.Fatalf("revocation admitted another generation: %d", response.StatusCode)
			}
			if target == "account" {
				f.success(t, 0, "explicitly-new-root", "")
			}
		})
	}
}

func TestCodeQualificationCancellationPropagatesToUpstream(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.Mode("hold")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			if ws {
				url := strings.Replace(f.gateways[0].PublicOrigin, "http", "ws", 1) + "/code/qualification/responses"
				conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.key}, "Thread-Id": []string{"cancel-root"}, "Session-Id": []string{"cancel-root"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)); err != nil {
					t.Fatal(err)
				}
				for len(f.peer.Requests()) == 0 {
					select {
					case <-time.After(10 * time.Millisecond):
					case <-ctx.Done():
						t.Fatal("no upstream generation")
					}
				}
				conn.CloseNow()
			} else {
				req, err := http.NewRequestWithContext(ctx, "POST", f.gateways[0].PublicOrigin+"/code/qualification/responses", bytes.NewReader(codeBody()))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+f.key)
				req.Header.Set("Thread-Id", "cancel-root")
				req.Header.Set("Session-Id", "cancel-root")
				req.Header.Set("Content-Type", "application/json")
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					response.Body.Close()
					t.Fatalf("status %d", response.StatusCode)
				}
				response.Body.Close()
			}
			select {
			case <-f.peer.Canceled:
			case <-ctx.Done():
				t.Fatal("downstream cancellation did not close upstream")
			}
			if len(f.peer.Requests()) != 1 {
				t.Fatal("canceled inference was replayed")
			}
		})
	}
}

func (f *codeFleet) initialRecovery(t *testing.T, attempts int) *codeFleet {
	t.Helper()
	route := codeDiscoveryResponse[contract.CodeRoute](t, f.route)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+route.Id.String(), contract.CodeRouteWrite{
		ProjectId: route.ProjectId, Slug: route.Slug, PoolId: route.PoolId, Models: route.Models, Enabled: true,
		InitialRecoveryAttempts: &attempts,
	}, etagHeader(f.route), 200)
	published := f.h.want(f.owner, "POST", "/api/v1/code/routes/"+route.Id.String()+"/publish", nil, withMatch(draft, idem("initial-recovery-publish")), 200)
	for _, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
	}
	return &codeFleet{codePublicFixture: &codePublicFixture{
		h: f.h, owner: f.owner, project: f.project, keyID: f.keyID, key: f.key,
		accounts: f.accounts, pool: f.pool, route: published, upstream: f.upstream,
		authority: f.authority, peer: f.peer, codexAuthorities: f.codexAuthorities,
	}, in: f.in, gateways: []*testutil.Process{f.in.replica("recovery-one"), f.in.replica("recovery-two")}}
}

func TestCodeQualificationFleetInitialRecoveryPreservesAttemptsAndBinding(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 1)
	f.peer.Faults("initial_quota")
	var executions atomic.Int64
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		executions.Add(1)
		return nil
	})
	response, body, err := f.request(t.Context(), 0, "initial-recovery", "", codeBody(), true)
	if err != nil || response.StatusCode != 200 || string(body) != codexfixture.SSE {
		t.Fatalf("initial recovery response=%v body=%s err=%v", response, body, err)
	}
	captures := f.peer.Requests()
	if len(captures) != 2 || executions.Load() != 1 || captures[0].Header.Get("Authorization") == captures[1].Header.Get("Authorization") {
		t.Fatal("proven rejection did not dispatch exactly once on another eligible account")
	}
	for _, capture := range captures {
		if !bytes.Equal(capture.Body, codeBody()) {
			t.Fatal("recovery changed requested model or payload")
		}
	}
	attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(attempts) != 2 || len(bindings) != 1 || attempts[0].BindingID != attempts[1].BindingID || attempts[0].AccountID == attempts[1].AccountID {
		t.Fatalf("attempt attribution or durable binding changed: attempts=%+v bindings=%+v", attempts, bindings)
	}
	for _, attempt := range attempts {
		if attempt.UpstreamStatus != nil && *attempt.UpstreamStatus == 429 {
			if attempt.State != "settled" || attempt.ReportedTokens == nil || *attempt.ReportedTokens != 0 || attempt.AccountID == bindings[0].AccountID {
				t.Fatal("rejected attempt lost original attribution or retained uncertain reservations")
			}
			if !attempt.InitialRecovery || !attempt.Nonexecution || attempt.RecoveryFrom != nil || attempt.RecoveryReason != nil {
				t.Fatal("definite rejection lost its recorded nonexecution evidence")
			}
		} else if attempt.AccountID != bindings[0].AccountID || attempt.ReportedTokens == nil || *attempt.ReportedTokens <= 0 {
			t.Fatal("executed attempt lost usage or recovered binding")
		} else {
			if attempt.RecoveryFrom == nil || attempt.RecoveryReason == nil || *attempt.RecoveryReason != "quota_rejected" || attempt.Nonexecution {
				t.Fatal("recovered attempt lost its safe recovery reason")
			}
			if !slices.ContainsFunc(attempts, func(source codemode.Attempt) bool {
				return source.ID == *attempt.RecoveryFrom && source.Nonexecution && source.RouteRevisionID == attempt.RouteRevisionID && source.RequestedModel != nil && attempt.RequestedModel != nil && *source.RequestedModel == *attempt.RequestedModel
			}) {
				t.Fatal("recovery source did not retain the route and requested-model evidence")
			}
		}
	}
	diagnostics, err := json.Marshal(f.list("attempts"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CONTROLLED_PRIVATE_PROMPT", "CONTROLLED_PRIVATE_OUTPUT", captures[0].Header.Get("Authorization"), captures[1].Header.Get("Authorization"), "X-Controlled-Multi"} {
		if forbidden != "" && bytes.Contains(diagnostics, []byte(forbidden)) {
			t.Fatal("recovery diagnostics exposed private request, output or header data")
		}
	}
	f.success(t, 1, "initial-recovery", "")
	captures = f.peer.Requests()
	if len(captures) != 3 || captures[1].Header.Get("Authorization") != captures[2].Header.Get("Authorization") {
		t.Fatal("another replica lost the recovered account binding")
	}
}

func TestCodeQualificationFleetConcurrentEstablishedRecoveryNeverMovesInflightBinding(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 2)
	f.success(t, 0, "concurrent-established", "")
	var dispatches atomic.Int64
	both := make(chan struct{})
	release := make(chan struct{})
	f.peer.SetRejector(func(ctx context.Context, _ http.Header, _ []byte) (int, []byte) {
		if dispatches.Add(1) == 2 {
			close(both)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return 0, nil
		}
		select {
		case <-both:
		case <-ctx.Done():
		}
		return 429, []byte(`{"error":{"type":"usage_limit_reached"}}`)
	})
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 2)
	for replica := range 2 {
		go func() {
			response, body, err := f.request(t.Context(), replica, "concurrent-established", "", codeBody(), true)
			results <- result{response, body, err}
		}()
	}
	first := <-results
	close(release)
	if first.err != nil || first.response.StatusCode != http.StatusConflict || !bytes.Contains(first.body, []byte("code_recovery_unsafe")) {
		t.Fatalf("inflight recovery response=%v body=%s err=%v", first.response, first.body, first.err)
	}
	second := <-results
	if second.err != nil || second.response.StatusCode != http.StatusOK {
		t.Fatalf("inflight original work response=%v body=%s err=%v", second.response, second.body, second.err)
	}
	captures := f.peer.Requests()
	if len(captures) != 3 || captures[0].Header.Get("Authorization") != captures[1].Header.Get("Authorization") || captures[1].Header.Get("Authorization") != captures[2].Header.Get("Authorization") {
		t.Fatal("concurrent turns replayed or moved an in-flight binding")
	}
	attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(attempts) != 3 || len(bindings) != 1 {
		t.Fatalf("concurrent attempts=%d bindings=%d", len(attempts), len(bindings))
	}
	for _, attempt := range attempts {
		if attempt.AccountID != bindings[0].AccountID || attempt.BindingID != bindings[0].ID {
			t.Fatal("concurrent established binding lost attempt ownership")
		}
	}
}

func TestCodeQualificationFleetEstablishedRecoveryRefusesOwnedState(t *testing.T) {
	for _, kind := range []string{"existing-child", "child-request", "multiple-pins", "socket-during-rejection"} {
		t.Run(kind, func(t *testing.T) {
			f := newCodeFleet(t).initialRecovery(t, 1)
			f.success(t, 0, "owned-root", "")
			if kind == "existing-child" || kind == "child-request" {
				f.success(t, 1, "owned-child", "owned-root")
			}
			if kind == "multiple-pins" {
				route := codeDiscoveryResponse[contract.CodeRoute](t, f.route)
				path := "/api/v1/code/routes/" + route.Id.String()
				aliases, rewriting := map[string]string{"other-code": route.Models[0]}, true
				draft := f.h.want(f.owner, "PUT", path, contract.CodeRouteWrite{
					ProjectId: route.ProjectId, Slug: route.Slug, PoolId: route.PoolId, Models: route.Models, Enabled: route.Enabled,
					Aliases: &aliases, ModelRewriting: &rewriting, InitialRecoveryAttempts: route.InitialRecoveryAttempts,
				}, etagHeader(f.route), 200)
				f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("multipin-recovery")), 200)
				f.restart(t, "multipin")
				response, data, err := f.request(t.Context(), 0, "owned-root", "", []byte(`{"model":"other-code","input":"complete context"}`), true)
				if err != nil || response.StatusCode != http.StatusOK {
					t.Fatalf("second public model pin response=%v body=%s err=%v", response, data, err)
				}
			}
			before := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
			entered, release := make(chan struct{}), make(chan struct{})
			f.peer.SetRejector(func(ctx context.Context, _ http.Header, _ []byte) (int, []byte) {
				if kind == "socket-during-rejection" {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return http.StatusTooManyRequests, []byte(`{"error":{"type":"usage_limit_reached"}}`)
			})
			conversation, parent := func() (string, string) {
				if kind == "child-request" {
					return "owned-child", "owned-root"
				}
				return "owned-root", ""
			}()
			requests := len(f.peer.Requests())
			type result struct {
				response *http.Response
				body     []byte
				err      error
			}
			results := make(chan result, 1)
			go func() {
				response, body, err := f.request(t.Context(), 1, conversation, parent, codeBody(), true)
				results <- result{response, body, err}
			}()
			if kind == "socket-during-rejection" {
				select {
				case <-entered:
				case <-t.Context().Done():
					t.Fatal("rejection did not reach upstream")
				}
				url := strings.Replace(f.gateways[0].PublicOrigin, "http", "ws", 1) + "/code/qualification/responses"
				conn, _, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + f.key}, "Thread-Id": {"owned-root"}, "Session-Id": {"owned-root"}}})
				if err != nil {
					close(release)
					t.Fatal(err)
				}
				defer conn.CloseNow()
				close(release)
			}
			got := <-results
			if got.err != nil || got.response.StatusCode != http.StatusConflict || !bytes.Contains(got.body, []byte("code_recovery_unsafe")) || !bytes.Contains(got.body, []byte("full context")) || len(f.peer.Requests()) != requests+1 {
				t.Fatalf("owned state recovery response=%v body=%s err=%v", got.response, got.body, got.err)
			}
			after := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("unsafe recovery changed root or child authority")
			}
			f.peer.SetRejector(nil)
			if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.code_accounts SET health='healthy',unavailable_until=NULL WHERE project_id=$1`, f.project); err != nil {
				t.Fatal(err)
			}
			f.success(t, 0, "owned-root", "")
			if kind == "existing-child" || kind == "child-request" {
				f.success(t, 1, "owned-child", "owned-root")
			}
		})
	}
}

func TestCodeQualificationFleetInitialRecoveryObservesNewGrantGeneration(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 1)
	f.in.worker("code-initial-refresh")
	f.peer.Faults("initial_auth")
	var executions atomic.Int64
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		executions.Add(1)
		return nil
	})
	response, body, err := f.request(t.Context(), 0, "initial-refresh", "", codeBody(), true)
	if err != nil || response.StatusCode != 200 || string(body) != codexfixture.SSE {
		t.Fatalf("refresh recovery response=%v body=%s err=%v", response, body, err)
	}
	captures := f.peer.Requests()
	if len(captures) != 2 || executions.Load() != 1 || captures[0].Header.Get("Authorization") == captures[1].Header.Get("Authorization") || captures[0].Header.Get("Chatgpt-Account-Id") != captures[1].Header.Get("Chatgpt-Account-Id") {
		t.Fatal("refresh recovery replayed or did not use a new grant for the same principal")
	}
	attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
	if len(attempts) != 2 || attempts[0].AccountID != attempts[1].AccountID {
		t.Fatal("supported grant refresh changed account ownership")
	}
	for _, attempt := range attempts {
		if attempt.RecoveryFrom == nil {
			if !attempt.Nonexecution || attempt.UpstreamStatus == nil || *attempt.UpstreamStatus != 401 {
				t.Fatal("credential rejection lost its proven nonexecution")
			}
			continue
		}
		if attempt.RecoveryReason == nil || *attempt.RecoveryReason != "credential_rejected" || !slices.ContainsFunc(attempts, func(source codemode.Attempt) bool { return source.ID == *attempt.RecoveryFrom && source.Nonexecution }) {
			t.Fatal("refreshed attempt lost its credential-rejection source")
		}
	}
}

func TestCodeQualificationFleetInitialRecoveryRechecksPoolAuthority(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 1)
	started, release := make(chan struct{}), make(chan struct{})
	f.peer.SetRejector(func(ctx context.Context, _ http.Header, _ []byte) (int, []byte) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 429, []byte(`{"error":{"type":"usage_limit_reached"}}`)
	})
	type result struct {
		response *http.Response
		body     []byte
		err      error
	}
	results := make(chan result, 1)
	go func() {
		response, body, err := f.request(t.Context(), 0, "initial-live-pool", "", codeBody(), true)
		results <- result{response: response, body: body, err: err}
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("initial dispatch never reached the controlled peer")
	}
	f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput([]string{}), etagHeader(f.pool), 200)
	close(release)
	got := <-results
	if got.err != nil || got.response.StatusCode != 403 || !bytes.Contains(got.body, []byte("code_pool_denied")) || len(f.peer.Requests()) != 1 {
		t.Fatalf("revoked pool dispatched recovery response=%v body=%s err=%v", got.response, got.body, got.err)
	}
	refusals := f.list("refusals")
	if len(refusals) != 1 {
		t.Fatalf("replacement refusal missing from diagnostics %+v", refusals)
	}
	refusal := refusals[0].(map[string]any)
	if refusal["code"] != "code_pool_denied" || refusal["api_key_id"] != f.keyID || refusal["project_id"] != f.project || refusal["route_id"] != f.route["id"] {
		t.Fatalf("replacement refusal lost admission attribution %+v", refusal)
	}
}
