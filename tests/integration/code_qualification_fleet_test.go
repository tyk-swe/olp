//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/database"
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

func (f *codeFleet) request(ctx context.Context, replica int, conversation, parent string, body []byte) (*http.Response, []byte, error) {
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
	req.Header.Set("X-Codex-Turn-State", "opaque-state")
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
