//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/management/contract"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func codeRepublishFleet(t *testing.T, f *codeFleet, models []string) {
	t.Helper()
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": models, "enabled": true}, etagHeader(f.route), 200)
	f.route = f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("republish-review")), 200)
	for i, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		f.gateways[i] = f.in.replica(fmt.Sprintf("review-%d", i))
	}
}

func codeReviewSocket(t *testing.T, ctx context.Context, f *codeFleet, replica int, key, conversation, model string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Authorization": {"Bearer " + key}, "Thread-Id": {conversation}}
	if model != "" {
		headers.Set("X-OLP-Code-Model", model)
	}
	conn, _, err := websocket.Dial(ctx, f.gateways[replica].PublicOrigin+"/code/qualification/responses", &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestCodeQualificationProviderLimitsAcrossKeysReplicasAndTransports(t *testing.T) {
	for _, dimension := range []string{"requests_per_minute", "max_concurrency", "tokens_per_minute"} {
		t.Run(dimension, func(t *testing.T) {
			f := newCodeFleet(t)
			key := f.h.want(f.owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Independent code key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("second-code-key"), 201)
			input := f.poolInput([]string{f.keyID, key["id"].(string)})
			input["account_ids"] = []string{f.accounts[0]["id"].(string)}
			f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), input, etagHeader(f.pool), 200)
			path := "/api/v1/providers/" + f.accounts[0]["provider_id"].(string)
			provider := f.h.want(f.owner, "GET", path, nil, nil, 200)
			configuration := provider["configuration"].(map[string]any)
			configuration["options"].(map[string]any)["limits"] = map[string]any{dimension: 1}
			f.h.want(f.owner, "PATCH", path, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
			codeRepublishFleet(t, f, []string{"gpt-5.4"})
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			if dimension == "max_concurrency" {
				f.peer.Mode("hold")
				request, _ := http.NewRequestWithContext(ctx, "POST", f.gateways[0].PublicOrigin+"/code/qualification/responses", bytes.NewReader(codeBody()))
				request.Header = http.Header{"Authorization": {"Bearer " + f.key}, "Thread-Id": {"held"}}
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("initial generation: %d", response.StatusCode)
				}
			} else if dimension == "requests_per_minute" {
				f.success(t, 0, "first-key", "")
			}
			before := len(f.peer.Requests())
			conn := codeReviewSocket(t, ctx, f, 1, key["secret"].(string), "second-key", "")
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)); err != nil {
				t.Fatal(err)
			}
			_, _, err := conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || !strings.Contains(err.Error(), "code_provider_rate_limited") {
				t.Fatalf("provider %s bypassed by second key/replica/transport: %v", dimension, err)
			}
			if len(f.peer.Requests()) != before {
				t.Fatal("refused generation reached upstream")
			}
			if dimension == "max_concurrency" {
				cancel()
				select {
				case <-f.peer.Canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("held generation was not canceled")
				}
			}
		})
	}
}

func TestCodeQualificationWebSocketModelSelectionAndPinnedReconnect(t *testing.T) {
	f := newCodeFleet(t)
	second := f.accounts[1]
	f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+second["id"].(string), map[string]any{"project_id": f.project, "provider_id": second["provider_id"], "credential_id": second["credential_id"], "name": second["name"], "enabled": true, "models": []string{"gpt-5.4-mini"}}, etagHeader(second), 200)
	codeRepublishFleet(t, f, []string{"gpt-5.4", "gpt-5.4-mini"})
	f.success(t, 0, "http-root", "")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for _, test := range []struct {
		thread, model, selection, account string
	}{
		{"http-root", "gpt-5.4", "", f.accounts[0]["id"].(string)},
		{"new-websocket", "gpt-5.4-mini", "gpt-5.4-mini", second["id"].(string)},
	} {
		conn := codeReviewSocket(t, ctx, f, 1, f.key, test.thread, test.selection)
		if err := conn.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"type":"response.create","model":%q,"input":[]}`, test.model)); err != nil {
			t.Fatal(err)
		}
		if _, err := codexfixture.ReadGeneration(ctx, conn); err != nil {
			t.Fatal(err)
		}
		bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
		found := false
		for _, binding := range bindings {
			if binding.Conversation == test.thread {
				found = true
				if binding.AccountID != test.account {
					t.Fatal("requested model selected wrong account or changed pin")
				}
			}
		}
		if !found {
			t.Fatal("missing durable binding")
		}
		_ = conn.Close(websocket.StatusNormalClosure, "completed")
	}
	for _, capture := range f.peer.Requests() {
		if capture.Header.Get("X-OLP-Code-Model") != "" {
			t.Fatal("OLP model control leaked upstream")
		}
	}
	before := len(f.peer.Requests())
	conn := codeReviewSocket(t, ctx, f, 0, f.key, "new-websocket", "gpt-5.4")
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation || len(f.peer.Requests()) != before {
		t.Fatalf("model change bypassed pinned account permission: %v", err)
	}
}

func TestCodePaidFallbackRetainsOnePaidOwnerAcrossReplicas(t *testing.T) {
	f := newCodeFleet(t)
	route := codePublicDecode[codemode.Route](t, f.route)
	model := route.Models[0]
	var paid atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-paid-secret" {
			t.Error("fallback lost paid credential")
		}
		paid.Store(true)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"fleet-paid","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)
	}))
	t.Cleanup(upstream.Close)
	provider, slot := newCodePaidProvider(t, f.h, f.owner, f.project, upstream.URL, model)
	price := contract.PricingRevisionRequest{EffectiveAt: time.Now().UTC(), Prices: []contract.PriceRequest{{ProviderKind: "openai", Model: model, Operation: "generation", Currency: "USD", InputPerMillion: nullable.NewNullableWithValue("1"), OutputPerMillion: nullable.NewNullableWithValue("2")}}}
	f.h.want(f.owner, "POST", "/api/v1/pricing/revisions", price, idem("fleet-fallback-price"), 201)
	targets := map[string]contract.CodePaidTarget{model: {ProviderId: uuid.MustParse(provider), SlotId: uuid.MustParse(slot.ID)}}
	write := contract.CodeRouteWrite{ProjectId: uuid.MustParse(f.project), PoolId: uuid.MustParse(route.PoolID), Slug: route.Slug, Models: route.Models, Enabled: true, PaidParticipation: new(true), InitialRecoveryAttempts: new(1), PaidFallbackTargets: &targets}
	path := "/api/v1/code/routes/" + route.ID
	saved := f.h.want(f.owner, "PUT", path, write, etagHeader(f.route), 200)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(saved, idem("fleet-paid-fallback")), 200)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.code_accounts SET health='unavailable' WHERE project_id=$1`, f.project); err != nil {
		t.Fatal(err)
	}
	for i, process := range f.gateways {
		if err := process.Kill(); err != nil {
			t.Fatal(err)
		}
		f.gateways[i] = f.in.replica(fmt.Sprintf("paid-fallback-%d", i))
	}
	bodyInput := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"}],"stream":false,"store":false}`, model))
	response, body, err := f.request(t.Context(), 0, "paid-fleet-root", "", bodyInput, true)
	if err != nil || response.StatusCode != 200 || !strings.Contains(string(body), "fleet-paid") || !paid.Load() {
		t.Fatalf("first replica fallback %v %s %v paid=%v", response, body, err, paid.Load())
	}
	response, body, err = f.request(t.Context(), 1, "paid-fleet-root", "", bodyInput, true)
	if err != nil || response.StatusCode != 200 {
		t.Fatalf("paid pin failed on second replica %v %s %v", response, body, err)
	}
	bindings := codePublicDecode[[]contract.CodeBinding](t, f.h.want(f.owner, "GET", "/api/v1/code/bindings?api_key_id="+f.keyID, nil, nil, 200)["items"])
	if len(bindings) != 1 || bindings[0].PaidProviderId.GetOrEmpty().String() != provider || bindings[0].Conversation != "paid-fleet-root" {
		t.Fatalf("replicas split paid ownership %+v", bindings)
	}
	attempts := codePublicDecode[[]contract.CodeAttempt](t, f.h.want(f.owner, "GET", "/api/v1/code/attempts?project_id="+f.project+"&binding_id="+bindings[0].Id.String(), nil, nil, 200)["items"])
	if len(attempts) != 2 {
		t.Fatal("paid binding did not retain both attempts")
	}
	for _, attempt := range attempts {
		if attempt.PaidBindingId.GetOrEmpty() != bindings[0].Id || attempt.PaidProviderId.GetOrEmpty().String() != provider || attempt.PaidSlotId.GetOrEmpty().String() != slot.ID || !attempt.AccountId.IsNull() || attempt.Nonexecution || !attempt.RecoveryFrom.IsNull() || !attempt.RecoveryReason.IsNull() {
			t.Fatal("paid diagnostics lost target attribution or invented rejection evidence")
		}
	}
	if slices.ContainsFunc(f.peer.Requests(), func(request codexfixture.Request) bool { return request.Header.Get("Session-Id") == "paid-fleet-root" }) {
		t.Fatal("initial exhausted subscription work dispatched")
	}
	f.h.want(f.owner, "POST", "/api/v1/code/bindings/"+bindings[0].Id.String()+"/retire", nil, idem("retire-fleet-paid"), 200)
	response, body, err = f.request(t.Context(), 1, "paid-fleet-root", "", bodyInput, true)
	if err != nil || response.StatusCode != 410 {
		t.Fatalf("retirement changed across replicas %v %s %v", response, body, err)
	}
}
