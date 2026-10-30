//go:build integration && oidctest

package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/tyk-swe/olp/internal/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	agentworkflows "github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/mcpgrants"
	"github.com/tyk-swe/olp/internal/mcpservers"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"

	"github.com/coder/websocket"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/gateway"
)

func TestWorkloadAgentConsentRequiresSessionCSRFAndFreshAuthentication(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Agent consent")
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "consent key", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{}}, idem("consent-key"), http.StatusCreated)
	consent := map[string]any{"agent_id": uuid.NewString(), "api_key_id": key["id"], "allowed_routes": []string{"chat"}}
	response, body := h.do(owner, "POST", "/api/v1/delegation-consents", consent, map[string]string{"X-CSRF-Token": "invalid"})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF proof accepted: %d %s", response.StatusCode, body)
	}
	unconfirmedResponse, unconfirmedBody := h.do(owner, "POST", "/api/v1/delegation-consents", consent, nil)
	if unconfirmedResponse.StatusCode != http.StatusPreconditionRequired {
		t.Fatalf("consent without fresh authentication accepted: %d %s", unconfirmedResponse.StatusCode, unconfirmedBody)
	}
}

func TestWorkloadAgentMemberConsentIssuesAgentBoundGrant(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.ClientIP = func(request *http.Request) string {
		if source := request.Header.Get("X-Test-Client-IP"); source != "" {
			return source
		}
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		return host
	}
	_, _, limiter := limSetup(t, "agent-member-consent")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, nil)
	type savedEvent struct {
		payload []byte
		err     error
	}
	persisted := make(chan savedEvent, 4)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		persisted <- savedEvent{payload: payload, err: err}
		return err
	}}
	owner := h.owner()
	project := createProject(h, owner, "Member delegation")
	otherProject := createProject(h, owner, "Other member delegation")
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other member key", "project_id": otherProject, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"member-chat"}}, idem("other-member-key"), http.StatusCreated)
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{"member-chat"}})
	vendor := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Member provider", vendor.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
		"prices": []any{map[string]any{
			"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel,
			"operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000",
		}},
	}, idem("member-pair-budget-price"), http.StatusCreated)
	eventually(t, "member delegated provider price", 30*time.Second, func() bool {
		h.refresh()
		inputs := h.Runtime.RoutingInputs()
		return inputs != nil && inputs.Price("azure_openai", provider["id"].(string), "", vendorModel, "generation", time.Now()) != nil
	})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "member-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("member-route"), http.StatusCreated)
	routePath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", routePath+"/validate", nil, etagHeader(draft), http.StatusOK)
	h.want(owner, "POST", routePath+"/activate", nil, withMatch(validated, idem("activate-member-route")), http.StatusOK)
	otherDraft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "unapproved-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("unapproved-member-route"), http.StatusCreated)
	otherRoutePath := "/api/v1/route-drafts/" + otherDraft["id"].(string)
	otherValidated := h.want(owner, "POST", otherRoutePath+"/validate", nil, etagHeader(otherDraft), http.StatusOK)
	h.want(owner, "POST", otherRoutePath+"/activate", nil, withMatch(otherValidated, idem("activate-unapproved-member-route")), http.StatusOK)
	issuer := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := h.want(owner, "GET", issuerPath, nil, nil, http.StatusOK)
	issuerInput := map[string]any{
		"name": "Portable CI", "issuer": issuer.endpoint.URL, "jwks_url": issuer.endpoint.URL,
		"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"},
		"max_lifetime_seconds": 600, "disabled_key_ids": []string{},
		"mappings": []any{
			map[string]any{"name": "agent", "match": map[string]string{"/sub": "member-agent", "/client_id": "member-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference"}, "agent_principal": true},
			map[string]any{"name": "ordinary", "match": map[string]string{"/sub": "ordinary-workload"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference"}},
		},
	}
	h.want(owner, "PUT", issuerPath, issuerInput, etagHeader(current), http.StatusOK)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "member key", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"member-chat", "unapproved-chat"}}, idem("member-key"), http.StatusCreated)
	member := h.invite(owner, "member@example.com", "developer")
	profile := h.want(member, "GET", "/api/v1/profile", nil, nil, http.StatusOK)
	memberID := profile["id"].(string)
	memberRecord := h.want(owner, "GET", "/api/v1/users/"+memberID, nil, nil, http.StatusOK)
	h.want(owner, "PATCH", "/api/v1/users/"+memberID, map[string]any{"access_scope": "assigned"}, etagHeader(memberRecord), http.StatusOK)
	addMember(h, owner, project, memberID, "viewer")
	h.want(member, "POST", "/api/v1/sessions", map[string]any{"email": "member@example.com", "password": accessPassword}, nil, http.StatusCreated)
	h.refresh()
	signer, signerErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
	if signerErr != nil {
		t.Fatal(signerErr)
	}
	claims, claimsErr := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": "member-agent", "client_id": "member-client", "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
	if claimsErr != nil {
		t.Fatal(claimsErr)
	}
	signed, signErr := signer.Sign(claims)
	if signErr != nil {
		t.Fatal(signErr)
	}
	assertion, serializeErr := signed.CompactSerialize()
	if serializeErr != nil {
		t.Fatal(serializeErr)
	}
	response, raw := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": "invalid"})
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key accepted: %d %s", response.StatusCode, raw)
	}
	ordinaryClaims, ordinaryClaimsErr := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": "ordinary-workload", "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
	if ordinaryClaimsErr != nil {
		t.Fatal(ordinaryClaimsErr)
	}
	ordinarySigned, ordinarySignErr := signer.Sign(ordinaryClaims)
	if ordinarySignErr != nil {
		t.Fatal(ordinarySignErr)
	}
	ordinaryAssertion, ordinarySerializeErr := ordinarySigned.CompactSerialize()
	if ordinarySerializeErr != nil {
		t.Fatal(ordinarySerializeErr)
	}
	ordinaryResponse, ordinaryBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + ordinaryAssertion, "X-OLP-Delegating-Key": "invalid"})
	if ordinaryResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ordinary workload unexpectedly delegated: %d %s", ordinaryResponse.StatusCode, ordinaryBody)
	}
	principals := h.want(owner, "GET", "/api/v1/api-keys", nil, nil, http.StatusOK)["items"].([]any)
	agentID := func() string {
		for _, item := range principals {
			principal := item.(map[string]any)
			if principal["workload_issuer_id"] == issuer.issuer["id"] && principal["workload_mapping"] == "agent" {
				return principal["id"].(string)
			}
		}
		t.Fatal("authenticated workload principal is absent")
		return ""
	}()
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": otherKey["id"]}, nil, http.StatusNoContent)
	h.want(owner, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": otherKey["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusForbidden)
	h.want(member, "GET", "/api/v1/delegation-options?api_key_id="+key["id"].(string), nil, nil, http.StatusForbidden)
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": key["id"]}, nil, http.StatusNoContent)
	h.want(member, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusForbidden)
	addMember(h, owner, project, memberID, "manager")
	h.want(member, "POST", "/api/v1/sessions", map[string]any{"email": "member@example.com", "password": accessPassword}, nil, http.StatusCreated)
	h.refresh()
	foreignOptions := h.want(owner, "GET", "/api/v1/delegation-options?api_key_id="+otherKey["id"].(string), nil, nil, http.StatusOK)
	if foreignAgents, ok := foreignOptions["agents"].([]any); !ok || len(foreignAgents) != 0 {
		t.Fatalf("cross-project agents must not be offered: %v", foreignOptions)
	}
	options := h.want(member, "GET", "/api/v1/delegation-options?api_key_id="+key["id"].(string), nil, nil, http.StatusOK)
	agents, ok := options["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("consent options should contain only the agent-enabled workload: %v", options)
	}
	eligible, ok := agents[0].(map[string]any)
	if !ok || eligible["id"] != agentID || fmt.Sprint(eligible["allowed_routes"]) != "[member-chat]" {
		t.Fatalf("consent options must show only routes both principals can use: %v", options)
	}
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": key["id"]}, nil, http.StatusNoContent)
	consent := h.want(member, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
	code, ok := consent["consent"].(string)
	if !ok || code == "" {
		t.Fatal("consent code missing")
	}
	issued, body := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": code})
	if issued.StatusCode != http.StatusCreated {
		t.Fatalf("authenticated agent could not exchange consent: %d %s", issued.StatusCode, body)
	}
	var grant runtime.DelegationGrant
	if err := json.Unmarshal(body, &grant); err != nil || grant.Token == "" {
		t.Fatalf("delegation grant missing: %v", err)
	}
	var grantMetadata struct {
		Pair string `json:"delegation_pair_digest"`
	}
	if err := json.Unmarshal(body, &grantMetadata); err != nil || grantMetadata.Pair == "" || grantMetadata.Pair != consent["delegation_pair_digest"] {
		t.Fatalf("consent and grant did not expose the same pair identity: %v", err)
	}
	inference, inferenceBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "member-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + grant.Token, "X-OLP-Agent-Identity": assertion})
	if inference.StatusCode != http.StatusOK {
		t.Fatalf("consented agent could not perform inference: %d %s", inference.StatusCode, inferenceBody)
	}
	firstEvent := <-persisted
	if firstEvent.err != nil || strings.Contains(string(firstEvent.payload), code) || strings.Contains(string(firstEvent.payload), grant.Token) {
		t.Fatalf("consent or grant appeared in exported usage: %v", firstEvent.err)
	}
	issueAndInfer := func(consenter *browser, keyID string) {
		t.Helper()
		h.want(consenter, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": keyID}, nil, http.StatusNoContent)
		nextConsent := h.want(consenter, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": keyID, "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
		if consenter == member && nextConsent["delegation_pair_digest"] != consent["delegation_pair_digest"] {
			t.Fatal("member pair identity changed across keys")
		}
		next, nextBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": nextConsent["consent"].(string)})
		if next.StatusCode != http.StatusCreated {
			t.Fatalf("renew consent: %d %s", next.StatusCode, nextBody)
		}
		var nextGrant runtime.DelegationGrant
		if err := json.Unmarshal(nextBody, &nextGrant); err != nil || nextGrant.Token == "" {
			t.Fatalf("renewed grant missing: %v", err)
		}
		response, responseBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "member-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + nextGrant.Token, "X-OLP-Agent-Identity": assertion})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("renewed inference: %d %s", response.StatusCode, responseBody)
		}
		saved := <-persisted
		if saved.err != nil || strings.Contains(string(saved.payload), nextConsent["consent"].(string)) || strings.Contains(string(saved.payload), nextGrant.Token) {
			t.Fatalf("consent or grant appeared in exported usage: %v", saved.err)
		}
	}
	secondKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "replacement member key", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"member-chat", "member-workflow"}}, idem("replacement-member-key"), http.StatusCreated)
	h.refresh()
	issueAndInfer(member, secondKey["id"].(string))
	issueAndInfer(owner, key["id"].(string))
	ownerID := h.want(owner, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	var memberAttempts, memberPairs, ownerAttempts, ownerPairs, distinctPairs int
	accountingErr := h.Pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE member_principal_id=$2),count(DISTINCT delegation_pair_digest) FILTER (WHERE member_principal_id=$2),count(*) FILTER (WHERE member_principal_id=$3),count(DISTINCT delegation_pair_digest) FILTER (WHERE member_principal_id=$3),count(DISTINCT delegation_pair_digest) FROM olp.attempt_usage_facts WHERE api_key_id IN ($1,$5) AND agent_principal_id=$4`, key["id"], memberID, ownerID, agentID, secondKey["id"]).Scan(&memberAttempts, &memberPairs, &ownerAttempts, &ownerPairs, &distinctPairs)
	if accountingErr != nil || memberAttempts != 2 || memberPairs != 1 || ownerAttempts != 1 || ownerPairs != 1 || distinctPairs != 2 {
		t.Fatalf("member attribution or stable pair: %d %d %d %d %d %v", memberAttempts, memberPairs, ownerAttempts, ownerPairs, distinctPairs, accountingErr)
	}
	memberPair := consent["delegation_pair_digest"].(string)
	budgetPath := "/api/v1/projects/" + project + "/attribution-budgets"
	budget := h.want(owner, "GET", budgetPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", budgetPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{memberPair: map[string]any{"daily_cost_limit": "0.000001"}}}}, etagHeader(budget), http.StatusOK)
	h.refresh()
	budgetConnection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	budgetSnapshots, err := limits.ReconciliationSnapshots(t.Context(), budgetConnection.Conn(), time.Now())
	budgetConnection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range budgetSnapshots {
		if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	memberBudget := h.want(owner, "GET", budgetPath, nil, nil, http.StatusOK)
	memberUsage := memberBudget["usage"].(map[string]any)["delegation_pair"].(map[string]any)[memberPair].(map[string]any)
	memberAccrued := memberUsage["daily"].(map[string]any)["accrued"].(string)
	memberAmount, validAmount := new(big.Rat).SetString(memberAccrued)
	if !validAmount || memberAmount.Sign() <= 0 {
		t.Fatalf("member pair did not retain previously settled spend: %s", memberAccrued)
	}
	for _, keyID := range []string{key["id"].(string), secondKey["id"].(string)} {
		h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": keyID}, map[string]string{"X-Test-Client-IP": "192.0.2.8"}, http.StatusNoContent)
		renewedConsent := h.want(member, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": keyID, "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
		renewed, renewedBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": renewedConsent["consent"].(string)})
		if renewed.StatusCode != http.StatusCreated {
			t.Fatalf("renew member consent across keys: %d %s", renewed.StatusCode, renewedBody)
		}
		var renewedGrant runtime.DelegationGrant
		if err := json.Unmarshal(renewedBody, &renewedGrant); err != nil || renewedGrant.Token == "" {
			t.Fatalf("renewed member grant missing: %v", err)
		}
		limited, limitedBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "member-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + renewedGrant.Token, "X-OLP-Agent-Identity": assertion})
		if limited.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("member pair budget bypassed after key rotation: %d %s", limited.StatusCode, limitedBody)
		}
		if saved := <-persisted; saved.err != nil {
			t.Fatal(saved.err)
		}
	}
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{"member-chat", "member-workflow"}})
	workflowVendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/responses") {
			writeResponsesFixture(w, vendorModel, "probe", false)
			return
		}
		var input map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode model request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "done"}
		finish := "stop"
		if len(input["tool_choice"]) > 0 {
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "probe", "type": "function", "function": map[string]any{"name": "olp_certification_probe", "arguments": "{}"}}}}
			finish = "tool_calls"
		}
		writeJSON(w, map[string]any{"id": "member-workflow", "object": "chat.completion", "created": 1, "model": vendorModel, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}})
	}))
	defer workflowVendor.Close()
	workflowProvider := activeAzureProviderInProject(t, h, owner, "Member workflow provider", workflowVendor.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	addBatchPrice(t, h, owner, workflowProvider["id"].(string), "", time.Now().Add(-time.Hour))
	workflowDraft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "member-workflow", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "tool_execution": "gateway", "targets": []any{map[string]any{"provider_id": workflowProvider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("member-workflow-route"), http.StatusCreated)
	workflowPath := "/api/v1/route-drafts/" + workflowDraft["id"].(string)
	workflowValidated := h.want(owner, "POST", workflowPath+"/validate", nil, etagHeader(workflowDraft), http.StatusOK)
	h.want(owner, "POST", workflowPath+"/activate", nil, withMatch(workflowValidated, idem("activate-member-workflow")), http.StatusOK)
	workflowKey := secondKey
	store := &limits.DebitStore{Pool: h.Pool, Limiter: limiter}
	h.Gateway.Debits = gateway.DurableSessionDebits(store)
	h.Gateway.WorkflowRuns = &agentworkflows.WorkflowStore{Pool: h.Pool, Auth: h.AuthKey, Debits: store}
	h.Gateway.WorkflowTools = &mcp.Server{Runtime: h.Runtime}
	var ownerPair string
	if err := h.Pool.QueryRow(t.Context(), `SELECT delegation_pair_digest FROM olp.attempt_usage_facts WHERE agent_principal_id=$1 AND member_principal_id=$2 LIMIT 1`, agentID, ownerID).Scan(&ownerPair); err != nil {
		t.Fatal(err)
	}
	workflowBudget := h.want(owner, "GET", budgetPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", budgetPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{
		memberPair: map[string]any{"daily_cost_limit": "0.000001"}, ownerPair: map[string]any{"daily_cost_limit": "1000000"},
	}}}, etagHeader(workflowBudget), http.StatusOK)
	workflowTemplatesPath := "/api/v1/projects/" + project + "/limit-templates"
	workflowTemplates := h.want(owner, "GET", workflowTemplatesPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", workflowTemplatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "1000000"}}}, etagHeader(workflowTemplates), http.StatusOK)
	workflowKeyPath := "/api/v1/api-keys/" + workflowKey["id"].(string)
	workflowKeyPolicy := h.want(owner, "GET", workflowKeyPath, nil, nil, http.StatusOK)
	h.want(owner, "PATCH", workflowKeyPath, map[string]any{"daily_cost_limit": "1000000"}, etagHeader(workflowKeyPolicy), http.StatusOK)
	h.refresh()
	workflowConnection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	workflowSnapshots, err := limits.ReconciliationSnapshots(t.Context(), workflowConnection.Conn(), time.Now())
	workflowConnection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range workflowSnapshots {
		if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	pairBefore := h.want(owner, "GET", budgetPath, nil, nil, http.StatusOK)["usage"].(map[string]any)["delegation_pair"].(map[string]any)[ownerPair].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	agentBefore := h.want(owner, "GET", "/api/v1/api-keys/"+agentID, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	keyBefore := h.want(owner, "GET", workflowKeyPath, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": workflowKey["id"]}, nil, http.StatusNoContent)
	workflowConsent := h.want(owner, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": workflowKey["id"], "allowed_routes": []string{"member-workflow"}}, nil, http.StatusCreated)
	workflowIssued, workflowBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-workflow"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": workflowConsent["consent"].(string)})
	if workflowIssued.StatusCode != http.StatusCreated {
		t.Fatalf("workflow consent exchange: %d %s", workflowIssued.StatusCode, workflowBody)
	}
	var workflowGrant runtime.DelegationGrant
	if err := json.Unmarshal(workflowBody, &workflowGrant); err != nil || workflowGrant.Token == "" {
		t.Fatalf("workflow grant missing: %v", err)
	}
	previousSink := h.Gateway.Sink
	workflowPersisted := make(chan error, 1)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		var storedAgent, storedMember, storedPair string
		if err := h.Pool.QueryRow(ctx, `SELECT COALESCE(agent_principal_id::text,''),COALESCE(member_principal_id::text,''),COALESCE(delegation_pair_digest,'') FROM olp.attempt_usage_facts WHERE request_id=$1`, event.RequestID).Scan(&storedAgent, &storedMember, &storedPair); err != nil {
			workflowPersisted <- err
			return err
		}
		if storedAgent != agentID || storedMember != ownerID || storedPair != event.DelegationPairDigest || event.MemberPrincipalID != ownerID {
			err := fmt.Errorf("workflow model attribution missing before terminal persistence: agent=%s member=%s pair_matches=%t", storedAgent, storedMember, storedPair == event.DelegationPairDigest)
			workflowPersisted <- err
			return err
		}
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		workflowPersisted <- err
		return err
	}}
	workflowResponse, workflowResponseBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "member-workflow", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + workflowGrant.Token, "X-OLP-Agent-Identity": assertion})
	h.Gateway.Sink = previousSink
	if workflowResponse.StatusCode != http.StatusOK {
		t.Fatalf("consented workflow inference: %d %s", workflowResponse.StatusCode, workflowResponseBody)
	}
	if err := <-workflowPersisted; err != nil {
		t.Fatal(err)
	}
	pairAfter := h.want(owner, "GET", budgetPath, nil, nil, http.StatusOK)["usage"].(map[string]any)["delegation_pair"].(map[string]any)[ownerPair].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	agentAfter := h.want(owner, "GET", "/api/v1/api-keys/"+agentID, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	keyAfter := h.want(owner, "GET", workflowKeyPath, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	delta := func(before, after string) *big.Rat {
		t.Helper()
		start, startOK := new(big.Rat).SetString(before)
		end, endOK := new(big.Rat).SetString(after)
		if !startOK || !endOK {
			t.Fatalf("invalid workflow budget amounts: %s %s", before, after)
		}
		return new(big.Rat).Sub(end, start)
	}
	pairDelta := delta(pairBefore, pairAfter)
	if pairDelta.Sign() <= 0 || pairDelta.Cmp(delta(agentBefore, agentAfter)) != 0 || pairDelta.Cmp(delta(keyBefore, keyAfter)) != 0 {
		t.Fatalf("workflow spend differs across pair, agent and key: %s to %s, %s to %s, %s to %s", pairBefore, pairAfter, agentBefore, agentAfter, keyBefore, keyAfter)
	}
	var workflowPairs int
	if err := h.Pool.QueryRow(context.Background(), `SELECT count(DISTINCT delegation_pair_digest) FROM olp.attempt_usage_facts WHERE api_key_id IN ($1,$2) AND agent_principal_id=$3 AND member_principal_id=$4`, key["id"], workflowKey["id"], agentID, ownerID).Scan(&workflowPairs); err != nil || workflowPairs != 1 {
		t.Fatalf("workflow member pair changed across keys: %d %v", workflowPairs, err)
	}
	var leaked bool
	privacyErr := h.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM olp.agent_delegations d WHERE position($1 in to_jsonb(d)::text)>0 OR position($2 in to_jsonb(d)::text)>0) OR EXISTS(SELECT 1 FROM olp.audit a WHERE position($1 in to_jsonb(a)::text)>0 OR position($2 in to_jsonb(a)::text)>0) OR EXISTS(SELECT 1 FROM olp.attempt_usage_facts f WHERE position($1 in to_jsonb(f)::text)>0 OR position($2 in to_jsonb(f)::text)>0)`, code, grant.Token).Scan(&leaked)
	if privacyErr != nil || leaked {
		t.Fatalf("consent or grant appeared in stored records: %v", privacyErr)
	}
	export := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, http.StatusOK)
	exportJSON, exportErr := json.Marshal(export)
	if exportErr != nil || strings.Contains(string(exportJSON), code) || strings.Contains(string(exportJSON), grant.Token) {
		t.Fatalf("consent or grant appeared in configuration export: %v", exportErr)
	}
	oidcIssuer := newTestIssuer(t)
	h.want(owner, "PUT", "/api/v1/oidc/configuration", map[string]any{"issuer": oidcIssuer.Server.URL, "discovery_url": oidcIssuer.Server.URL + "/.well-known/openid-configuration", "client_id": "test-client", "client_secret": "write-only-client-secret", "enabled": true, "default_role": "developer"}, nil, http.StatusOK)
	oidcMember := &browser{}
	oidcLogin := h.want(oidcMember, "POST", "/api/v1/oidc/login", map[string]any{}, nil, http.StatusOK)["authorization_url"].(string)
	h.want(oidcMember, "GET", oidcIssuer.callback(t, oidcLogin, nil), nil, nil, http.StatusSeeOther)
	oidcMemberID := h.want(oidcMember, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	addMember(h, owner, project, oidcMemberID, "manager")
	oidcRelogin := h.want(oidcMember, "POST", "/api/v1/oidc/login", map[string]any{}, nil, http.StatusOK)["authorization_url"].(string)
	h.want(oidcMember, "GET", oidcIssuer.callback(t, oidcRelogin, nil), nil, nil, http.StatusSeeOther)
	h.refresh()
	oidcVerify := h.want(oidcMember, "POST", "/api/v1/oidc/reauthenticate", map[string]any{"purpose": "agent_delegation", "resource_id": key["id"]}, nil, http.StatusOK)["authorization_url"].(string)
	oidcStatus, _, oidcHeaders := h.request(oidcMember, "GET", oidcIssuer.callback(t, oidcVerify, nil), nil, nil)
	if oidcStatus != http.StatusSeeOther || oidcHeaders.Get("Location") != "/agents/grants?reauthenticated=agent_delegation&resource_id="+key["id"].(string) {
		t.Fatalf("OIDC consent continuation: %d %s", oidcStatus, oidcHeaders.Get("Location"))
	}
	h.want(oidcMember, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": otherKey["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusPreconditionRequired)
	h.want(oidcMember, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
	samlIssuer := newSAMLFixture(t)
	samlIssuer.configure(t, h, owner)
	samlMember := &browser{}
	samlLogin, samlLoginState := samlBegin(t, h, samlMember, "/api/v1/saml/login", map[string]any{})
	samlLoginPath := samlReceive(t, h, samlLoginState, samlIssuer.response(t, samlLogin, "consent-saml", "saml-consent@example.com", []string{"developers"}, nil), http.StatusSeeOther)
	h.want(samlMember, "GET", samlLoginPath, nil, nil, http.StatusSeeOther)
	samlMemberID := h.want(samlMember, "GET", "/api/v1/profile", nil, nil, http.StatusOK)["id"].(string)
	addMember(h, owner, project, samlMemberID, "manager")
	samlRelogin, samlReloginState := samlBegin(t, h, samlMember, "/api/v1/saml/login", map[string]any{})
	samlReloginPath := samlReceive(t, h, samlReloginState, samlIssuer.response(t, samlRelogin, "consent-saml", "saml-consent@example.com", []string{"developers"}, nil), http.StatusSeeOther)
	h.want(samlMember, "GET", samlReloginPath, nil, nil, http.StatusSeeOther)
	h.refresh()
	samlVerify, samlVerifyState := samlBegin(t, h, samlMember, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "agent_delegation", "resource_id": key["id"]})
	samlVerifyPath := samlReceive(t, h, samlVerifyState, samlIssuer.response(t, samlVerify, "consent-saml", "saml-consent@example.com", []string{"developers"}, nil), http.StatusSeeOther)
	samlStatus, _, samlHeaders := h.request(samlMember, "GET", samlVerifyPath, nil, nil)
	if samlStatus != http.StatusSeeOther || samlHeaders.Get("Location") != "/agents/grants?reauthenticated=agent_delegation&resource_id="+key["id"].(string) {
		t.Fatalf("SAML consent continuation: %d %s", samlStatus, samlHeaders.Get("Location"))
	}
	h.want(samlMember, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": otherKey["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusPreconditionRequired)
	h.want(samlMember, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
	replay, replayBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": code})
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("consent replay accepted: %d %s", replay.StatusCode, replayBody)
	}
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": key["id"]}, nil, http.StatusNoContent)
	revocationConsent := h.want(member, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
	revocationIssued, revocationBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": revocationConsent["consent"].(string)})
	if revocationIssued.StatusCode != http.StatusCreated {
		t.Fatalf("authenticated agent could not exchange consent: %d %s", revocationIssued.StatusCode, revocationBody)
	}
	var revocationGrant runtime.DelegationGrant
	if err := json.Unmarshal(revocationBody, &revocationGrant); err != nil || revocationGrant.Token == "" {
		t.Fatalf("delegation grant missing: %s %v", revocationBody, err)
	}
	h.want(member, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": key["id"]}, nil, http.StatusNoContent)
	pending := h.want(member, "POST", "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": key["id"], "allowed_routes": []string{"member-chat"}}, nil, http.StatusCreated)
	assertMemberAuthority := func(want bool) {
		t.Helper()
		views := h.want(owner, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusOK)
		found := false
		items := mcpExecItems(t, views, "items")
		for i := range items {
			view := mcpExecItem(t, items, i, views)
			if view["member_id"] == memberID {
				found = true
				if view["member_authorized"] != want {
					t.Fatalf("member authority mismatch: %v", view)
				}
			}
		}
		if !found {
			t.Fatal("member delegations missing")
		}
	}
	assertMemberAuthority(true)
	h.want(owner, "DELETE", "/api/v1/projects/"+project+"/members/"+memberID, nil, projectEtag(h, owner, project), http.StatusNoContent)
	assertMemberAuthority(false)
	h.refresh()
	denied, deniedBody := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"member-chat"}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": pending["consent"].(string)})
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("consent survived member authority revocation: %d %s", denied.StatusCode, deniedBody)
	}
	revoked, revokedBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "member-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + revocationGrant.Token, "X-OLP-Agent-Identity": assertion})
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delegation survived member authority revocation: %d %s", revoked.StatusCode, revokedBody)
	}
}

func TestWorkloadConfigurationPublishesBeforeGrantingResources(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := createProject(h, owner, "Published workloads")
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{}})
	fixture := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	created := h.want(owner, "POST", "/api/v1/toolsets", map[string]any{"project_id": project, "name": "workflow", "toolset": map[string]any{"tools": []any{}}}, idem("workload-toolset"), 201)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	publication := document["agent_publications"].([]any)[0].(map[string]any)
	publication["published"] = publication["draft"]
	issuer := document["workload_issuers"].([]any)[0].(map[string]any)
	mapping := issuer["mappings"].([]any)[0].(map[string]any)
	mapping["scopes"] = []string{"tools"}
	mapping["allowed_toolsets"] = []string{created["id"].(string)}
	body := map[string]any{"document": document}
	plan := h.want(owner, "POST", "/api/v1/configuration/plan", body, nil, 200)
	if len(plan["blockers"].([]any)) != 0 || len(plan["conflicts"].([]any)) != 0 {
		t.Fatalf("combined publication and workload promotion cannot be planned: %v", plan)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", body, idem("workload-publication-apply"), 200)
	read := h.want(owner, "GET", "/api/v1/toolsets/"+created["id"].(string), nil, nil, 200)
	if read["revision_id"] == nil {
		t.Fatal("workload grant committed without its publication")
	}
	h.want(owner, "DELETE", "/api/v1/toolsets/"+created["id"].(string), nil, withMatch(read, idem("retire-workload-toolset")), 204)
	currentIssuer := h.want(owner, "GET", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), nil, nil, 200)
	inputMapping := fixture.input["mappings"].([]any)[0].(map[string]any)
	inputMapping["scopes"] = []string{"tools"}
	inputMapping["allowed_toolsets"] = []string{created["id"].(string)}
	fixture.input["disabled_key_ids"] = []string{"current"}
	fixture.endpoint.Close()
	updated := h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(currentIssuer), 200)
	inputMapping["allowed_toolsets"] = []string{}
	updated = h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(updated), 200)
	inputMapping["allowed_toolsets"] = []string{created["id"].(string)}
	h.want(owner, "PUT", "/api/v1/workload-issuers/"+fixture.issuer["id"].(string), fixture.input, etagHeader(updated), 422)
}

func TestWorkloadConfigurationValidatesFinalTemplateReferences(t *testing.T) {
	for _, mode := range []string{"disable", "migrate"} {
		t.Run(mode, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			project := createProject(h, owner, "Workload template promotion")
			workloadProject(t, h, owner, project, map[string]any{"generation": []string{}})
			newWorkloadFixture(t, h, owner, project, []string{"generation"})
			exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
			document := exported["document"].(map[string]any)
			projectEntry := document["projects"].([]any)[0].(map[string]any)
			issuer := document["workload_issuers"].([]any)[0].(map[string]any)
			if mode == "disable" {
				issuer["enabled"] = false
				projectEntry["limit_templates"] = map[string]any{}
			} else {
				issuer["mappings"].([]any)[0].(map[string]any)["limit_template"] = "replacement"
				projectEntry["limit_templates"] = map[string]any{"replacement": map[string]any{"requests_per_minute": 100}}
			}
			h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("workload-template-"+mode), 200)
		})
	}
}

func TestWorkloadIdentityMapsAndAccountsWithoutRetainingCredentials(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Workloads"}, idem("project"), 201)
	projectID := project["id"].(string)
	base := "/api/v1/projects/" + projectID
	templates := h.want(owner, "GET", base+"/limit-templates", nil, nil, 200)
	h.want(owner, "PUT", base+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 2}}}, etagHeader(templates), 200)
	groups := h.want(owner, "GET", base+"/route-groups", nil, nil, 200)
	h.want(owner, "PUT", base+"/route-groups", map[string]any{"groups": map[string]any{"generation": []string{"workload-chat"}}}, etagHeader(groups), 200)
	vendor := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Workload provider", vendor.URL, &projectID, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": projectID, "slug": "workload-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("draft"), 201)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(validated, idem("activate")), 200)
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: "current", Algorithm: "EdDSA", Use: "sig"}}})
	}))
	defer jwks.Close()
	input := map[string]any{"name": "CI issuer", "issuer": jwks.URL, "jwks_url": jwks.URL, "enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{}, "mappings": []any{map[string]any{"name": "build", "match": map[string]string{"/repository": "example/repo"}, "project_id": projectID, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference", "models_read"}, "end_user_claim": "/customer"}}}
	issuer := h.want(owner, "POST", "/api/v1/workload-issuers", input, idem("issuer"), 201)
	issuerPath := "/api/v1/workload-issuers/" + issuer["id"].(string)
	client, _, limiter := limSetup(t, "workload")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	await := endUserAccounting(t, h, limiter)
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	replica.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	replica.Gateway.Sink = h.Gateway.Sink
	h.refresh()
	replica.refresh()
	limSettleInMinute(t, client, 5*time.Second)
	subject := "private-workload-subject-sentinel"
	sign := func(subject, repository string) string {
		s, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: private}, (&jose.SignerOptions{}).WithHeader("kid", "current"))
		if e != nil {
			t.Fatal(e)
		}
		now := time.Now()
		body, _ := json.Marshal(map[string]any{"iss": jwks.URL, "aud": "olp", "sub": subject, "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "repository": repository, "customer": "customer-1"})
		j, e := s.Sign(body)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := j.CompactSerialize()
		if e != nil {
			t.Fatal(e)
		}
		return raw
	}
	token := sign(subject, "example/repo")
	for i, host := range []*accessHarness{h, replica, h} {
		want := 200
		if i == 2 {
			want = 429
		}
		status, body := attributionCall(t, host, "workload-chat", token, nil)
		if status != want {
			t.Fatalf("request %d: %d %s", i, status, body)
		}
		await()
	}
	var principal, digest string
	var count int
	if e = h.Pool.QueryRow(context.Background(), "SELECT id::text,workload_digest FROM olp.api_keys WHERE workload_issuer_id=$1", issuer["id"]).Scan(&principal, &digest); e != nil || len(digest) != 64 {
		t.Fatalf("principal: %v", e)
	}
	if e = h.Pool.QueryRow(context.Background(), "SELECT count(*) FROM olp.requests WHERE api_key_id=$1", principal).Scan(&count); e != nil || count != 3 {
		t.Fatalf("accounting owners: %d %v", count, e)
	}
	for _, table := range []string{"api_keys", "requests", "attempt_usage_facts", "workload_issuers", "audit"} {
		var leaked bool
		e = h.Pool.QueryRow(context.Background(), "SELECT EXISTS(SELECT 1 FROM olp."+table+" r WHERE position($1 in to_jsonb(r)::text)>0 OR position($2 in to_jsonb(r)::text)>0)", subject, token).Scan(&leaked)
		if e != nil || leaked {
			t.Fatalf("privacy in %s: %v", table, e)
		}
	}
	key := h.want(owner, "GET", "/api/v1/api-keys/"+principal, nil, nil, 200)
	h.want(owner, "POST", "/api/v1/api-keys/"+principal+"/rotate", nil, withMatch(key, idem("rotate-managed")), 409)
	if status, _ := attributionCall(t, h, "workload-chat", sign(subject, "foreign/repo"), nil); status != 401 {
		t.Fatalf("mapping mismatch: %d", status)
	}
	jwks.Close() // Emergency key-ID disablement must not depend on the issuer endpoint.
	input["disabled_key_ids"] = []string{"current"}
	current := h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, input, etagHeader(current), 200)
	replica.refresh()
	if status, _ := attributionCall(t, replica, "workload-chat", token, nil); status != 401 {
		t.Fatalf("disabled signing key: %d", status)
	}
	input["disabled_key_ids"] = []string{}
	input["enabled"] = false
	jwks.Close()
	current = h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, input, etagHeader(current), 200)
	h.refresh()
	if status, body := attributionCall(t, h, "workload-chat", token, nil); status != 401 || strings.Contains(string(body), subject) {
		t.Fatalf("disabled issuer: %d", status)
	}
}

type workloadFixture struct {
	input    map[string]any
	issuer   map[string]any
	endpoint *httptest.Server
	private  ed25519.PrivateKey
	keys     *atomic.Value
	kid      string
}

func newWorkloadFixture(t *testing.T, h *accessHarness, owner *browser, project string, groups []string) *workloadFixture {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := new(atomic.Value)
	keys.Store(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "current", Algorithm: "EdDSA", Use: "sig"}}})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(keys.Load())
	}))
	t.Cleanup(endpoint.Close)
	input := map[string]any{"name": "Portable CI", "issuer": endpoint.URL, "jwks_url": endpoint.URL, "enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{}, "mappings": []any{map[string]any{"name": "worker", "match": map[string]string{}, "project_id": project, "limit_template": "worker", "route_groups": groups, "scopes": []string{"inference", "models_read"}, "end_user_claim": "/customer", "allow_provider_state": true}}}
	issuer := h.want(owner, "POST", "/api/v1/workload-issuers", input, idem("workload"), 201)
	return &workloadFixture{input: input, issuer: issuer, endpoint: endpoint, private: private, keys: keys, kid: "current"}
}
func (f *workloadFixture) token(t *testing.T, subject string, expiry time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: f.private}, (&jose.SignerOptions{}).WithHeader("kid", f.kid))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"iss": f.endpoint.URL, "aud": "olp", "sub": subject, "iat": time.Now().Unix(), "exp": expiry.Unix(), "customer": "worker-customer"})
	sig, err := signer.Sign(body)
	if err != nil {
		t.Fatal(err)
	}
	token, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func workloadProject(t *testing.T, h *accessHarness, owner *browser, project string, groups map[string]any) {
	t.Helper()
	base := "/api/v1/projects/" + project
	templates := h.want(owner, "GET", base+"/limit-templates", nil, nil, 200)
	h.want(owner, "PUT", base+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(templates), 200)
	current := h.want(owner, "GET", base+"/route-groups", nil, nil, 200)
	h.want(owner, "PUT", base+"/route-groups", map[string]any{"groups": groups}, etagHeader(current), 200)
}

type workloadCodeRuntime struct {
	*codeForwardRuntime
	manager *runtime.Manager
}

func (r workloadCodeRuntime) Authenticate(token string) (*access.Authority, error) {
	return r.manager.Authenticate(token)
}

func TestWorkloadSubscriptionRebindsCurrentPolicyAndRetainsDigest(t *testing.T) {
	f := newCodeFixture(t)
	workloadProject(t, f.h, f.owner, f.project, map[string]any{"subscription": []string{"coding"}})
	issuer := newWorkloadFixture(t, f.h, f.owner, f.project, []string{"subscription"})
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-subscription" {
			t.Error("JWT reached upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", calls.Add(1))
	}))
	defer upstream.Close()
	server, _, _ := codeForwardServer(t, f, upstream.URL)
	f.h.Gateway.Runtime = workloadCodeRuntime{f.h.Gateway.Runtime.(*codeForwardRuntime), f.h.Runtime}
	f.h.refresh()
	token := issuer.token(t, "private-subscription-subject", time.Now().Add(5*time.Minute))
	principal, err := f.h.Runtime.Authenticate(token)
	if err != nil {
		t.Fatal(err)
	}
	request := func(thread string, want int) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"hello","stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Thread-Id", thread)
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("subscription %d: %s", response.StatusCode, body)
		}
	}
	request("unassigned", 403)
	f.exec(t, `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, f.pool, principal.ID)
	request("first", 200)
	codeForwardAwait(t, f, "settled", 1)
	base := "/api/v1/projects/" + f.project + "/limit-templates"
	templates := f.h.want(f.owner, "GET", base, nil, nil, 200)
	f.h.want(f.owner, "PUT", base, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 2}}}, etagHeader(templates), 200)
	// Leave runtime cached. The durable admission must rebind current limits
	// without dropping the already verified end-user claim.
	request("second", 200)
	codeForwardAwait(t, f, "settled", 2)
	var count int
	if err = f.h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.code_attempts WHERE api_key_id=$1 AND end_user_digest=$2 AND state='settled'", principal.ID, principal.EndUserDigest).Scan(&count); err != nil || count != 2 {
		t.Fatalf("workload accounting: %d %v", count, err)
	}
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := f.h.want(f.owner, "GET", issuerPath, nil, nil, 200)
	issuer.input["enabled"] = false
	f.h.want(f.owner, "PUT", issuerPath, issuer.input, etagHeader(current), 200)
	request("disabled", 403)
	if calls.Load() != 2 {
		t.Fatal("disabled workload dispatched")
	}
}

func TestWorkloadAgentDelegatesKeyForOneInference(t *testing.T) {
	h := newAccessHarness(t)
	client, _, limiter := limSetup(t, "agent-delegation")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, nil)
	persisted := make(chan error, 2)
	h.Gateway.Sink = &gateway.PersistingSink{Persist: func(ctx context.Context, event *usage.Event, payload []byte) error {
		_, err := usage.PersistEvent(ctx, h.Pool, event, payload)
		persisted <- err
		return err
	}}
	owner := h.owner()
	project := createProject(h, owner, "Agent delegation")
	workloadProject(t, h, owner, project, map[string]any{"generation": []string{"delegated-chat"}})
	templatesPath := "/api/v1/projects/" + project + "/limit-templates"
	templates := h.want(owner, "GET", templatesPath, nil, nil, 200)
	h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 2}}}, etagHeader(templates), 200)
	vendor := newOpenAIFixture(t, "")
	provider := activeAzureProviderInProject(t, h, owner, "Delegated provider", vendor.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "delegated-chat", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": provider["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("delegated-route"), 201)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, "POST", path+"/validate", nil, etagHeader(draft), 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(validated, idem("activate-delegated-route")), 200)
	issuer := newWorkloadFixture(t, h, owner, project, []string{"generation"})
	issuerInput := func(match map[string]string) map[string]any {
		return map[string]any{
			"name": "Portable CI", "issuer": issuer.endpoint.URL, "jwks_url": issuer.endpoint.URL,
			"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"},
			"max_lifetime_seconds": 1200, "disabled_key_ids": []string{},
			"mappings": []any{map[string]any{"name": "agent", "match": match, "project_id": project, "limit_template": "worker", "route_groups": []string{"generation"}, "scopes": []string{"inference"}, "agent_principal": true}},
		}
	}
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, issuerInput(map[string]string{"/sub": "approved-agent"}), etagHeader(current), 422)
	h.want(owner, "PUT", issuerPath, issuerInput(map[string]string{"/client_id": "approved-client"}), etagHeader(current), 422)
	h.want(owner, "PUT", issuerPath, issuerInput(map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}), etagHeader(current), 200)
	exported := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	exportedIssuer := document["workload_issuers"].([]any)[0].(map[string]any)
	exportedMapping := exportedIssuer["mappings"].([]any)[0].(map[string]any)
	if exportedMapping["agent_principal"] != true {
		t.Fatal("configuration export lost the agent principal setting")
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("apply-agent-issuer"), 200)
	delegating := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "delegating", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"delegated-chat"}}, idem("delegating-key"), 201)
	limitedKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "limited-agent-key", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"delegated-chat"}}, idem("limited-agent-key"), 201)
	ordinary := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "ordinary", "project_id": project, "scopes": []string{"inference"}, "allowed_routes": []string{"delegated-chat"}}, idem("ordinary-key"), 201)
	otherProject := createProject(h, owner, "Other delegation project")
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other-project", "project_id": otherProject, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{}}, idem("other-project-key"), 201)
	prompt := h.want(owner, "POST", "/api/v1/prompts", map[string]any{"project_id": project, "name": "delegated prompt", "prompt": map[string]any{"template": "Hello {{name}}", "variables": []string{"name"}}}, idem("delegated-prompt"), 201)
	promptPath := "/api/v1/prompts/" + prompt["id"].(string)
	published := h.want(owner, "POST", promptPath+"/publish", nil, withMatch(prompt, idem("publish-delegated-prompt")), 200)
	h.want(owner, "PUT", promptPath+"/labels/production", map[string]any{"revision_id": published["revision_id"]}, withMatch(published, idem("label-delegated-prompt")), 200)
	h.refresh()
	sign := func(subject, client, audience string) string {
		t.Helper()
		signer, signerErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
		if signerErr != nil {
			t.Fatal(signerErr)
		}
		now := time.Now()
		body, marshalErr := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": audience, "sub": subject, "client_id": client, "iat": now.Unix(), "exp": now.Add(19 * time.Minute).Unix()})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		signed, signErr := signer.Sign(body)
		if signErr != nil {
			t.Fatal(signErr)
		}
		raw, serializeErr := signed.CompactSerialize()
		if serializeErr != nil {
			t.Fatal(serializeErr)
		}
		return raw
	}
	agent := sign("approved-agent", "approved-client", "olp")
	bounds := runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"delegated-chat"}}
	exchangeBounds := func(assertion, key string, body runtime.DelegationBounds, want int) string {
		t.Helper()
		started := time.Now()
		response, raw := h.do(nil, "POST", "/v1/delegations", body, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key})
		if response.StatusCode != want {
			t.Fatalf("delegation exchange: %d %s", response.StatusCode, raw)
		}
		if want != http.StatusCreated {
			return ""
		}
		var grant struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
			Pair      string    `json:"delegation_pair_digest"`
		}
		if err := json.Unmarshal(raw, &grant); err != nil || grant.Token == "" || grant.Pair == "" {
			t.Fatalf("delegation grant missing token or pair identity: %v", err)
		}
		ttl := func() int64 {
			if body.TTLSeconds > 0 {
				return body.TTLSeconds
			}
			return 300
		}()
		if grant.ExpiresAt.Before(started.Add(time.Duration(ttl)*time.Second-time.Second)) || grant.ExpiresAt.After(time.Now().Add(time.Duration(ttl)*time.Second+time.Second)) {
			t.Fatalf("delegation expiry outside requested bounds: %v", grant.ExpiresAt)
		}
		return grant.Token
	}
	exchange := func(assertion, key string, want int) string {
		t.Helper()
		return exchangeBounds(assertion, key, bounds, want)
	}
	exchange(agent, ordinary["secret"].(string), http.StatusForbidden)
	exchange(agent, otherKey["secret"].(string), http.StatusForbidden)
	exchange(sign("other-agent", "approved-client", "olp"), delegating["secret"].(string), http.StatusUnauthorized)
	exchange(sign("approved-agent", "other-client", "olp"), delegating["secret"].(string), http.StatusUnauthorized)
	exchange(sign("approved-agent", "approved-client", "foreign"), delegating["secret"].(string), http.StatusUnauthorized)
	exchangeBounds(agent, delegating["secret"].(string), runtime.DelegationBounds{Scopes: []string{"delegate"}, AllowedRoutes: []string{"delegated-chat"}}, http.StatusBadRequest)
	exchangeBounds(agent, delegating["secret"].(string), runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"delegated-chat", "other-route"}}, http.StatusForbidden)
	exchangeBounds(agent, delegating["secret"].(string), runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"delegated-chat"}, TTLSeconds: 901}, http.StatusBadRequest)
	grant := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	exchange(agent, grant, http.StatusUnauthorized)
	var leaked bool
	err := h.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM olp.agent_delegations d WHERE position($1 in to_jsonb(d)::text)>0 OR position($2 in to_jsonb(d)::text)>0 OR position($3 in to_jsonb(d)::text)>0 OR position($4 in to_jsonb(d)::text)>0)`, grant, agent, "approved-agent", "approved-client").Scan(&leaked)
	if err != nil || leaked {
		t.Fatalf("grant retained raw credentials or claims: %v", err)
	}
	infer := func(token, assertion string, want int) {
		t.Helper()
		response, raw := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + token, "X-OLP-Agent-Identity": assertion})
		if response.StatusCode != want {
			t.Fatalf("delegated inference: %d %s", response.StatusCode, raw)
		}
	}
	limSettleInMinute(t, client, 5*time.Second)
	infer(grant, sign("other-agent", "approved-client", "olp"), http.StatusUnauthorized)
	infer(grant, agent, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	infer(grant, agent, http.StatusUnauthorized)
	renewed := exchangeBounds(agent, delegating["secret"].(string), runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{"delegated-chat"}, TTLSeconds: 900}, http.StatusCreated)
	promptResponse, promptBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "olp_prompt": map[string]any{"id": prompt["id"], "label": "production", "variables": map[string]string{"name": "agent"}}}, map[string]string{"Authorization": "Bearer " + renewed, "X-OLP-Agent-Identity": agent})
	if promptResponse.StatusCode != http.StatusOK {
		t.Fatalf("delegated prompt inference: %d %s", promptResponse.StatusCode, promptBody)
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	var agentID, ownerID string
	var attempts, pairCount int
	accountingErr := h.Pool.QueryRow(context.Background(), `SELECT min(agent_principal_id::text),min(api_key_id::text),count(*),count(DISTINCT delegation_pair_digest) FROM olp.attempt_usage_facts WHERE api_key_id=$1`, delegating["id"]).Scan(&agentID, &ownerID, &attempts, &pairCount)
	if accountingErr != nil || agentID == "" || ownerID != delegating["id"] || attempts != 2 || pairCount != 1 {
		t.Fatalf("delegated accounting owners and stable pair: %s %s %d %d %v", agentID, ownerID, attempts, pairCount, accountingErr)
	}
	var promptAttempts int
	promptErr := h.Pool.QueryRow(context.Background(), `SELECT count(*) FROM olp.attempts a JOIN olp.attempt_usage_facts f ON f.attempt_id=a.id WHERE f.agent_principal_id=$1 AND f.api_key_id=$2 AND a.routing->'prompt'->>'id'=$3 AND a.routing->'prompt'->>'revision_id'=$4`, agentID, ownerID, prompt["id"], published["revision_id"]).Scan(&promptAttempts)
	if promptErr != nil || promptAttempts != 1 {
		t.Fatalf("delegated prompt attempt attribution: %d %v", promptAttempts, promptErr)
	}
	limitedGrant := exchange(agent, limitedKey["secret"].(string), http.StatusCreated)
	infer(limitedGrant, agent, http.StatusTooManyRequests)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	keyPath := "/api/v1/api-keys/" + delegating["id"].(string)
	key := h.want(owner, "GET", keyPath, nil, nil, 200)
	restricted := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	limited := h.want(owner, "PATCH", keyPath, map[string]any{"scopes": []string{"delegate"}}, etagHeader(key), 200)
	h.refresh()
	infer(restricted, agent, http.StatusUnauthorized)
	restored := h.want(owner, "PATCH", keyPath, map[string]any{"scopes": []string{"inference", "delegate"}}, etagHeader(limited), 200)
	h.refresh()
	groupRestricted := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	groupsPath := "/api/v1/projects/" + project + "/route-groups"
	groups := h.want(owner, "GET", groupsPath, nil, nil, 200)
	emptyGroups := h.want(owner, "PUT", groupsPath, map[string]any{"groups": map[string]any{"generation": []string{}}}, etagHeader(groups), 200)
	h.refresh()
	infer(groupRestricted, agent, http.StatusUnauthorized)
	h.want(owner, "PUT", groupsPath, map[string]any{"groups": map[string]any{"generation": []string{"delegated-chat"}}}, etagHeader(emptyGroups), 200)
	h.refresh()
	revokedKeyGrant := exchange(agent, delegating["secret"].(string), http.StatusCreated)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(restored, idem("revoke-delegating")), 200)
	h.refresh()
	infer(revokedKeyGrant, agent, http.StatusUnauthorized)
	secondKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "second-delegating", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{"delegated-chat"}}, idem("second-delegating"), 201)
	h.refresh()
	revokedAgentGrant := exchange(agent, secondKey["secret"].(string), http.StatusCreated)
	agentPath := "/api/v1/api-keys/" + agentID
	agentKey := h.want(owner, "GET", agentPath, nil, nil, 200)
	h.want(owner, "POST", agentPath+"/revoke", nil, withMatch(agentKey, idem("revoke-agent")), 200)
	h.refresh()
	infer(revokedAgentGrant, agent, http.StatusUnauthorized)
	currentIssuer := h.want(owner, "GET", issuerPath, nil, nil, 200)
	h.want(owner, "PUT", issuerPath, issuerInput(map[string]string{"/sub": "approved-agent", "/client_id": "replacement-client"}), etagHeader(currentIssuer), 200)
	h.refresh()
	replacement := sign("approved-agent", "replacement-client", "olp")
	replacementGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	infer(replacementGrant, replacement, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	var replacementID string
	replacementErr := h.Pool.QueryRow(context.Background(), `SELECT agent_principal_id::text FROM olp.attempt_usage_facts WHERE api_key_id=$1`, secondKey["id"]).Scan(&replacementID)
	if replacementErr != nil || replacementID == agentID {
		t.Fatalf("distinct clients must have distinct agent principals: %s %v", replacementID, replacementErr)
	}
	currentTemplates := h.want(owner, "GET", templatesPath, nil, nil, 200)
	h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"tokens_per_minute": 1}}}, etagHeader(currentTemplates), 200)
	h.refresh()
	tokenLimitedGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	tokenResponse, tokenBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + tokenLimitedGrant, "X-OLP-Agent-Identity": replacement})
	if tokenResponse.StatusCode != http.StatusBadRequest || !strings.Contains(string(tokenBody), "request_exceeds_token_limit") {
		t.Fatalf("agent token limit: %d %s", tokenResponse.StatusCode, tokenBody)
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	priceEffective := time.Now().Add(-time.Second)
	h.want(owner, "POST", "/api/v1/pricing/revisions", map[string]any{
		"effective_at": priceEffective.UTC().Format(time.RFC3339Nano),
		"prices": []any{map[string]any{
			"provider_kind": "azure_openai", "provider_id": provider["id"], "model": vendorModel,
			"operation": "generation", "currency": "USD", "input_per_million": "1000000", "output_per_million": "1000000",
		}},
	}, idem("delegated-budget-price"), http.StatusCreated)
	eventually(t, "delegated provider price", 30*time.Second, func() bool {
		h.refresh()
		inputs := h.Runtime.RoutingInputs()
		return inputs != nil && inputs.Price("azure_openai", provider["id"].(string), "", vendorModel, "generation", time.Now()) != nil
	})
	budgetTemplates := h.want(owner, "GET", templatesPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"daily_cost_limit": "0.000001"}}}, etagHeader(budgetTemplates), http.StatusOK)
	h.refresh()
	connection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), connection.Conn(), time.Now())
	connection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	budgetGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	beforeAgentRefusal := vendor.chatCalls.Load()
	infer(budgetGrant, replacement, http.StatusTooManyRequests)
	if vendor.chatCalls.Load() != beforeAgentRefusal {
		t.Fatal("agent budget refusal dispatched to the provider")
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	increase := h.want(owner, "POST", "/api/v1/budget-increases", map[string]any{"target": map[string]any{"kind": "api_key", "id": replacementID}, "window": "day", "amount": "1000000", "reason": "delegated agent test"}, idem("delegated-agent-increase"), http.StatusCreated)
	h.refresh()
	increasedGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	infer(increasedGrant, replacement, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	h.want(owner, "DELETE", "/api/v1/budget-increases/"+increase["id"].(string), nil, etagHeader(increase), http.StatusNoContent)
	for _, scenario := range []struct {
		name  string
		limit string
		basis string
	}{
		{name: "weekly agent cost", limit: "weekly_cost_limit", basis: "cost"},
		{name: "monthly agent cost", limit: "monthly_cost_limit", basis: "cost"},
		{name: "weekly agent charge", limit: "weekly_cost_limit", basis: "charge"},
		{name: "monthly agent charge", limit: "monthly_cost_limit", basis: "charge"},
	} {
		currentTemplates := h.want(owner, "GET", templatesPath, nil, nil, http.StatusOK)
		h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{scenario.limit: "0.000001", "cost_basis": scenario.basis}}}, etagHeader(currentTemplates), http.StatusOK)
		h.refresh()
		grant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
		before := vendor.chatCalls.Load()
		infer(grant, replacement, http.StatusTooManyRequests)
		if vendor.chatCalls.Load() != before {
			t.Fatalf("%s budget refusal dispatched to the provider", scenario.name)
		}
		if err := <-persisted; err != nil {
			t.Fatal(err)
		}
	}
	clearedTemplates := h.want(owner, "GET", templatesPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{}}}, etagHeader(clearedTemplates), http.StatusOK)
	h.refresh()
	pairResponse, pairResponseBody := h.do(nil, "POST", "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + replacement, "X-OLP-Delegating-Key": secondKey["secret"].(string)})
	var pairMetadata struct {
		Digest string `json:"delegation_pair_digest"`
	}
	if pairResponse.StatusCode != http.StatusCreated || json.Unmarshal(pairResponseBody, &pairMetadata) != nil || pairMetadata.Digest == "" {
		t.Fatalf("delegation did not expose a pair identity: %d", pairResponse.StatusCode)
	}
	pairDigest := pairMetadata.Digest
	pairPath := "/api/v1/projects/" + project + "/attribution-budgets"
	pairBudget := h.want(owner, "GET", pairPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{pairDigest: map[string]any{"daily_cost_limit": "1000000"}}}}, etagHeader(pairBudget), http.StatusOK)
	h.refresh()
	pairConnection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pairSnapshots, err := limits.ReconciliationSnapshots(t.Context(), pairConnection.Conn(), time.Now())
	pairConnection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range pairSnapshots {
		if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	pairGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	infer(pairGrant, replacement, http.StatusOK)
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	pairReport := h.want(owner, "GET", pairPath, nil, nil, http.StatusOK)
	pairUsage := pairReport["usage"].(map[string]any)["delegation_pair"].(map[string]any)[pairDigest].(map[string]any)
	pairAccrued := pairUsage["daily"].(map[string]any)["accrued"].(string)
	callerReport := h.want(owner, "GET", "/api/v1/api-keys/"+secondKey["id"].(string), nil, nil, http.StatusOK)
	agentReport := h.want(owner, "GET", "/api/v1/api-keys/"+replacementID, nil, nil, http.StatusOK)
	callerAccrued := callerReport["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	agentAccrued := agentReport["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	pairAmount, pairOK := new(big.Rat).SetString(pairAccrued)
	callerAmount, callerOK := new(big.Rat).SetString(callerAccrued)
	agentAmount, agentOK := new(big.Rat).SetString(agentAccrued)
	if !pairOK || !callerOK || !agentOK || pairAmount.Sign() <= 0 || pairAmount.Cmp(callerAmount) != 0 || pairAmount.Cmp(agentAmount) != 0 {
		t.Fatalf("delegated spend differs across pair, caller and agent: %s %s %s", pairAccrued, callerAccrued, agentAccrued)
	}
	keyPolicy := h.want(owner, "GET", "/api/v1/api-keys/"+secondKey["id"].(string), nil, nil, http.StatusOK)
	h.want(owner, "PATCH", "/api/v1/api-keys/"+secondKey["id"].(string), map[string]any{"allowed_attribution_keys": []string{"team", "delegation_pair"}}, etagHeader(keyPolicy), http.StatusOK)
	h.want(owner, "PUT", pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{pairDigest: map[string]any{"daily_cost_limit": "1000000"}}, "team": map[string]any{"core": map[string]any{"daily_cost_limit": "0.000001"}}}}, etagHeader(pairReport), http.StatusOK)
	h.refresh()
	teamConnection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	teamSnapshots, err := limits.ReconciliationSnapshots(t.Context(), teamConnection.Conn(), time.Now())
	teamConnection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range teamSnapshots {
		if _, _, err := limiter.ApplyCostSnapshot(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	teamGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	beforeTeamRefusal := vendor.chatCalls.Load()
	teamResponse, teamBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + teamGrant, "X-OLP-Agent-Identity": replacement, usage.AttributionHeader: `{"team":"core"}`})
	if teamResponse.StatusCode != http.StatusTooManyRequests || vendor.chatCalls.Load() != beforeTeamRefusal {
		t.Fatalf("ordinary attribution budget was not enforced with a delegated pair: %d %s", teamResponse.StatusCode, teamBody)
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	updatedBudget := h.want(owner, "GET", pairPath, nil, nil, http.StatusOK)
	h.want(owner, "PUT", pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{pairDigest: map[string]any{"daily_cost_limit": "0.000001"}}, "team": map[string]any{"core": map[string]any{"daily_cost_limit": "1000000"}}}}, etagHeader(updatedBudget), http.StatusOK)
	h.refresh()
	renewedGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	beforePairRefusal := vendor.chatCalls.Load()
	infer(renewedGrant, replacement, http.StatusTooManyRequests)
	if vendor.chatCalls.Load() != beforePairRefusal {
		t.Fatal("pair budget refusal dispatched to the provider")
	}
	if err := <-persisted; err != nil {
		t.Fatal(err)
	}
	spoofGrant := exchange(replacement, secondKey["secret"].(string), http.StatusCreated)
	spoofResponse, spoofBody := h.do(nil, "POST", "/v1/chat/completions", map[string]any{"model": "delegated-chat", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + spoofGrant, "X-OLP-Agent-Identity": replacement, usage.AttributionHeader: `{"delegation_pair":"other"}`})
	if spoofResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("caller could supply the reserved delegation pair: %d %s", spoofResponse.StatusCode, spoofBody)
	}
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.settings(key,value,etag,updated_by) SELECT 'retention.usage_days','1',gen_random_uuid(),created_by FROM olp.api_keys WHERE id=$1 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, secondKey["id"]); err != nil {
		t.Fatal(err)
	}
	maintenance, err := usage.RunMaintenance(t.Context(), h.Pool, time.Now().Add(48*time.Hour))
	if err != nil || maintenance.UsageRows == 0 {
		t.Fatalf("delegated spend rollup: %+v %v", maintenance, err)
	}
	retainedOwner := &browser{}
	h.want(retainedOwner, "POST", "/api/v1/sessions", map[string]any{"email": "owner@example.com", "password": accessPassword}, nil, http.StatusCreated)
	retainedPair := h.want(retainedOwner, "GET", pairPath, nil, nil, http.StatusOK)["usage"].(map[string]any)["delegation_pair"].(map[string]any)[pairDigest].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	retainedCaller := h.want(retainedOwner, "GET", "/api/v1/api-keys/"+secondKey["id"].(string), nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	retainedAgent := h.want(retainedOwner, "GET", "/api/v1/api-keys/"+replacementID, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	for _, retained := range []string{retainedPair, retainedCaller, retainedAgent} {
		amount, ok := new(big.Rat).SetString(retained)
		if !ok || amount.Cmp(pairAmount) != 0 {
			t.Fatalf("delegated spend changed after retention: %s, want %s", retained, pairAccrued)
		}
	}
}

func TestWorkloadDelegationRealtimeRevalidatesAndRevokes(t *testing.T) {
	h := newAccessHarness(t)
	_, _, limiter := limSetup(t, "agent-delegation-realtime")
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, nil)
	owner := h.owner()
	project := createProject(h, owner, "Delegated realtime")
	upstream := newStrictRealtimeFixture(t, "openai")
	slug, _ := provisionStrictRealtimeInProject(t, h, owner, "openai", upstream.URL+"/v1", nil, &project)
	workloadProject(t, h, owner, project, map[string]any{"realtime": []string{slug}})
	templatesPath := "/api/v1/projects/" + project + "/limit-templates"
	templates := h.want(owner, "GET", templatesPath, nil, nil, 200)
	h.want(owner, "PUT", templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "max_concurrency": 1}}}, etagHeader(templates), 200)
	issuer := newWorkloadFixture(t, h, owner, project, []string{"realtime"})
	config := map[string]any{
		"name": "Portable CI", "issuer": issuer.endpoint.URL, "jwks_url": issuer.endpoint.URL,
		"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"},
		"max_lifetime_seconds": 600, "disabled_key_ids": []string{},
		"mappings": []any{map[string]any{"name": "agent", "match": map[string]string{"/sub": "realtime-agent", "/client_id": "realtime-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"realtime"}, "scopes": []string{"inference"}, "agent_principal": true}},
	}
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	h.want(owner, "PUT", issuerPath, config, etagHeader(issuer.issuer), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "realtime delegate", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{slug}}, idem("realtime-delegate"), 201)
	otherKey := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "other realtime delegate", "project_id": project, "scopes": []string{"inference", "delegate"}, "allowed_routes": []string{slug}}, idem("other-realtime-delegate"), 201)
	h.refresh()
	signer, signerErr := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
	if signerErr != nil {
		t.Fatal(signerErr)
	}
	now := time.Now()
	body, marshalErr := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": "realtime-agent", "client_id": "realtime-client", "iat": now.Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	signed, signErr := signer.Sign(body)
	if signErr != nil {
		t.Fatal(signErr)
	}
	assertion, serializeErr := signed.CompactSerialize()
	if serializeErr != nil {
		t.Fatal(serializeErr)
	}
	response, raw := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{slug}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key["secret"].(string)})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("delegation exchange: %d %s", response.StatusCode, raw)
	}
	var grant runtime.DelegationGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	url := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
	headers := http.Header{"Authorization": {"Bearer " + grant.Token}, "X-OLP-Agent-Identity": {assertion}}
	conn, handshake, dialErr := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if dialErr != nil {
		if handshake != nil {
			detail, _ := io.ReadAll(handshake.Body)
			handshake.Body.Close()
			t.Fatalf("delegated realtime dial: %s %v", detail, dialErr)
		}
		t.Fatalf("delegated realtime dial: %v", dialErr)
	}
	defer conn.CloseNow()
	otherResponse, otherRaw := h.do(nil, "POST", "/v1/delegations", runtime.DelegationBounds{Scopes: []string{"inference"}, AllowedRoutes: []string{slug}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": otherKey["secret"].(string)})
	if otherResponse.StatusCode != http.StatusCreated {
		t.Fatalf("second realtime delegation exchange: %d %s", otherResponse.StatusCode, otherRaw)
	}
	var otherGrant runtime.DelegationGrant
	if err := json.Unmarshal(otherRaw, &otherGrant); err != nil {
		t.Fatal(err)
	}
	otherConn, otherHandshake, otherErr := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + otherGrant.Token}, "X-OLP-Agent-Identity": {assertion}}})
	if otherConn != nil {
		otherConn.CloseNow()
	}
	if otherErr == nil || otherHandshake == nil || otherHandshake.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("agent concurrency limit: %v %v", otherHandshake, otherErr)
	}
	for i, frame := range strictRealtimeClientFrames {
		if i > 0 {
			time.Sleep(1600 * time.Millisecond)
		}
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatalf("delegated realtime frame %d: %v", i, err)
		}
		_, received, err := conn.Read(ctx)
		if err != nil || string(received) != string(strictRealtimeProviderFrames[i]) {
			t.Fatalf("delegated realtime response %d: %q %v", i, received, err)
		}
		if i == 1 {
			_, received, err := conn.Read(ctx)
			if err != nil || string(received) != string(strictRealtimeToolCall) {
				t.Fatalf("delegated realtime tool event: %q %v", received, err)
			}
		}
	}
	replayed, replayResponse, replayErr := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: headers})
	if replayed != nil {
		replayed.CloseNow()
	}
	if replayErr == nil || replayResponse == nil || replayResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("delegation replay was accepted: %v %v", replayResponse, replayErr)
	}
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	current := h.want(owner, "GET", keyPath, nil, nil, 200)
	h.want(owner, "POST", keyPath+"/revoke", nil, withMatch(current, idem("revoke-realtime-delegate")), 200)
	h.refresh()
	_, _, closeErr := conn.Read(ctx)
	if websocket.CloseStatus(closeErr) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked realtime session remained open: %v", closeErr)
	}
}

func TestWorkloadAgentDelegatesKeyForTools(t *testing.T) {
	upstreamCalls := make(chan struct{}, 32)
	type observedToken struct {
		value   string
		expiry  time.Time
		subject string
	}
	signedTokens := make(chan observedToken, 32)
	holdNext := make(chan (<-chan struct{}), 1)
	held := make(chan struct{}, 1)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	material := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	const signerIssuer = "https://gateway.example.test"
	const signerAudience = "https://tools.example.test/first"
	f := newPersonalOAuthFixtureWithCall(t, nil, personalOAuthCallbacks{Call: func(r *http.Request) mcpsdk.CallToolResult {
		upstreamCalls <- struct{}{}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims := &personalDelegatedClaims{}
		if _, parseErr := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithIssuer(signerIssuer), jwt.WithAudience(signerAudience), jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired()); parseErr != nil {
			t.Errorf("delegated signature rejected: %v", parseErr)
			return mcpsdk.CallToolResult{IsError: true}
		}
		if _, parseErr := jwt.Parse(token, func(*jwt.Token) (any, error) { return public, nil }, jwt.WithAudience("https://tools.example.test/second"), jwt.WithValidMethods([]string{"EdDSA"})); parseErr == nil {
			t.Error("delegated signature accepted by another audience")
		}
		signedTokens <- observedToken{token, claims.ExpiresAt.Time, claims.Subject}
		select {
		case release := <-holdNext:
			held <- struct{}{}
			<-release
		default:
		}
		return mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: claims.Subject}}}
	}})
	currentServer := f.h.want(f.owner, http.MethodGet, "/api/v1/mcp-servers/"+f.serverID, nil, nil, http.StatusOK)
	var server mcpservers.Definition
	if err := json.Unmarshal(mustJSON(currentServer), &server); err != nil {
		t.Fatal(err)
	}
	enabled := true
	price := "1"
	updatedServer := f.h.want(f.owner, http.MethodPut, "/api/v1/mcp-servers/"+f.serverID, mcpservers.Input{Name: server.Name, Endpoint: server.Endpoint, Enabled: &enabled, OAuth: server.OAuth, UnitPrice: &price, Delegation: &routing.MCPDelegation{Issuer: signerIssuer, Audience: signerAudience}, DelegationPrivateKey: &material}, withMatch(currentServer, idem("agent-signer")), http.StatusOK)
	currentToolset := f.h.want(f.owner, http.MethodGet, "/api/v1/toolsets/"+f.toolsetID, nil, nil, http.StatusOK)
	tool := agentcontract.Tool{ServerID: f.serverID, RevisionID: mcpExecString(t, updatedServer, "revision_id"), CatalogDigest: server.Catalog.Digest, Name: "identity", Alias: f.alias}
	updatedToolset := f.h.want(f.owner, http.MethodPut, "/api/v1/toolsets/"+f.toolsetID, personalOAuthToolsetInput{ProjectID: server.ProjectID, Name: "OAuth tools", Toolset: agentcontract.Toolset{Tools: []agentcontract.Tool{tool}}}, withMatch(currentToolset, idem("agent-signer-toolset")), http.StatusOK)
	f.h.want(f.owner, http.MethodPost, "/api/v1/toolsets/"+f.toolsetID+"/publish", nil, withMatch(updatedToolset, idem("agent-signer-publish")), http.StatusOK)
	project := f.h.want(f.owner, http.MethodGet, "/api/v1/toolsets/"+f.toolsetID, nil, nil, http.StatusOK)["project_id"].(string)
	workloadProject(t, f.h, f.owner, project, map[string]any{"bootstrap": []string{"placeholder"}})
	issuer := newWorkloadFixture(t, f.h, f.owner, project, []string{"bootstrap"})
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	current := f.h.want(f.owner, http.MethodGet, issuerPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, issuerPath, map[string]any{
		"name": "Portable CI", "issuer": issuer.endpoint.URL, "jwks_url": issuer.endpoint.URL,
		"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"},
		"max_lifetime_seconds": 600, "disabled_key_ids": []string{},
		"mappings": []any{
			map[string]any{"name": "agent", "match": map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{}, "allowed_toolsets": []string{f.toolsetID}, "scopes": []string{"tools"}, "agent_principal": true},
			map[string]any{"name": "second-agent", "match": map[string]string{"/sub": "second-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{}, "allowed_toolsets": []string{f.toolsetID}, "scopes": []string{"tools"}, "agent_principal": true},
		},
	}, etagHeader(current), http.StatusOK)
	delegating := f.h.want(f.owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "delegating tools", "project_id": project, "scopes": []string{"tools", "delegate"}, "allowed_toolsets": []string{f.toolsetID}}, idem("delegating-tools-key"), http.StatusCreated)
	f.h.refresh()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims, err := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": "approved-agent", "client_id": "approved-client", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	bounds := struct {
		Scopes          []string `json:"scopes"`
		AllowedToolsets []string `json:"allowed_toolsets"`
		TTLSeconds      int      `json:"ttl_seconds"`
	}{Scopes: []string{"tools"}, AllowedToolsets: []string{f.toolsetID}, TTLSeconds: 90}
	response, body := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": delegating["secret"].(string)})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("tool delegation exchange: %d %s", response.StatusCode, body)
	}
	var grant struct {
		Token      string    `json:"token"`
		PairDigest string    `json:"delegation_pair_digest"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &grant); err != nil || grant.Token == "" || grant.PairDigest == "" || grant.ExpiresAt.IsZero() {
		t.Fatalf("tool delegation missing token: %v", err)
	}
	pairPath := "/api/v1/projects/" + project + "/attribution-budgets"
	pairBudget := f.h.want(f.owner, http.MethodGet, pairPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.PairDigest: map[string]any{"daily_cost_limit": "1000"}}}}, etagHeader(pairBudget), http.StatusOK)
	f.h.refresh()
	principals := f.h.want(f.owner, http.MethodGet, "/api/v1/api-keys", nil, nil, http.StatusOK)["items"].([]any)
	agentID := func() string {
		for _, item := range principals {
			principal := item.(map[string]any)
			if principal["workload_issuer_id"] == issuer.issuer["id"] && principal["workload_mapping"] == "agent" {
				return principal["id"].(string)
			}
		}
		t.Fatal("authenticated agent principal is absent")
		return ""
	}()
	f.completePKCE(t, f.claimKey(t, mcpPersonalKey{ID: delegating["id"].(string), Secret: delegating["secret"].(string)}), testutil.OAuthIdentity{Subject: "first", Account: "agent-account"})
	handler := f.consumer(t)
	issueToolGrant := func(keySecret, agentAssertion string) (string, string) {
		t.Helper()
		response, body := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + agentAssertion, "X-OLP-Delegating-Key": keySecret})
		var value struct {
			Token string `json:"token"`
			Pair  string `json:"delegation_pair_digest"`
		}
		if response.StatusCode != http.StatusCreated || json.Unmarshal(body, &value) != nil || value.Token == "" || value.Pair == "" {
			t.Fatalf("tool delegation exchange failed: %d", response.StatusCode)
		}
		return value.Token, value.Pair
	}
	callTool := func(token, agentAssertion, callID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", bytes.NewReader(mustJSON(map[string]any{"toolset_id": f.toolsetID, "name": f.alias, "arguments": map[string]any{}, "call_id": callID})))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("X-OLP-Agent-Identity", agentAssertion)
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		return result
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(string(mustJSON(struct {
		ToolsetID string   `json:"toolset_id"`
		Name      string   `json:"name"`
		Arguments struct{} `json:"arguments"`
		CallID    string   `json:"call_id"`
	}{ToolsetID: f.toolsetID, Name: f.alias, CallID: "delegated-tool"}))))
	request.Header.Set("Authorization", "Bearer "+grant.Token)
	request.Header.Set("X-OLP-Agent-Identity", assertion)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "agent:"+agentID) {
		t.Fatalf("delegated tool call failed: %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case <-upstreamCalls:
	default:
		t.Fatal("delegated call did not reach the upstream")
	}
	if signedToken := <-signedTokens; signedToken.expiry.After(grant.ExpiresAt) {
		t.Fatalf("downstream signature outlived the delegation: %s > %s", signedToken.expiry, grant.ExpiresAt)
	}
	replay := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(string(mustJSON(struct {
		ToolsetID string   `json:"toolset_id"`
		Name      string   `json:"name"`
		Arguments struct{} `json:"arguments"`
		CallID    string   `json:"call_id"`
	}{ToolsetID: f.toolsetID, Name: f.alias, CallID: "delegated-replay"}))))
	replay.Header.Set("Authorization", "Bearer "+grant.Token)
	replay.Header.Set("X-OLP-Agent-Identity", assertion)
	replay.Header.Set("Content-Type", "application/json")
	replayed := httptest.NewRecorder()
	handler.ServeHTTP(replayed, replay)
	if replayed.Code != http.StatusUnauthorized {
		t.Fatalf("delegated grant was replayed: %d %s", replayed.Code, replayed.Body.String())
	}
	select {
	case <-upstreamCalls:
		t.Fatal("replayed grant reached the upstream")
	default:
	}
	pairReport := f.h.want(f.owner, http.MethodGet, pairPath, nil, nil, http.StatusOK)
	pairAccrued := pairReport["usage"].(map[string]any)["delegation_pair"].(map[string]any)[grant.PairDigest].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	delegations := f.h.want(f.owner, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusOK)
	items := mcpExecItems(t, delegations, "items")
	if len(items) != 1 {
		t.Fatalf("expected the delegated pair: %v", delegations)
	}
	delegation := mcpExecItem(t, items, 0, delegations)
	delegationID := mcpExecString(t, delegation, "id")
	delegationPath := "/api/v1/agent-delegations?project_id=" + project + "&delegation_id=" + delegationID
	filteredDelegations := f.h.want(f.owner, http.MethodGet, delegationPath, nil, nil, http.StatusOK)
	filteredItems := mcpExecItems(t, filteredDelegations, "items")
	if len(filteredItems) != 1 || mcpExecString(t, mcpExecItem(t, filteredItems, 0, filteredDelegations), "id") != delegationID {
		t.Fatalf("delegation filter did not return the requested record: %v", filteredDelegations)
	}
	emptyDelegations := f.h.want(f.owner, http.MethodGet, "/api/v1/agent-delegations?project_id="+project+"&delegation_id="+acctID(t), nil, nil, http.StatusOK)
	if len(mcpExecItems(t, emptyDelegations, "items")) != 0 {
		t.Fatalf("delegation filter returned an unrelated record: %v", emptyDelegations)
	}
	f.h.want(f.owner, http.MethodGet, "/api/v1/agent-delegations?delegation_id=invalid", nil, nil, http.StatusUnprocessableEntity)
	budget := mcpExecObject(t, delegation, "budget")
	if delegation["api_key_id"] != delegating["id"] || delegation["agent_id"] != agentID || delegation["revoked_at"] != nil || mcpExecObject(t, budget, "daily")["accrued"] != pairAccrued || mcpExecObject(t, delegation, "budget_policy")["daily_cost_limit"] != "1000" {
		t.Fatalf("delegation view did not report principals, spend and budget: %v", delegation)
	}
	for _, private := range []string{grant.Token, grant.PairDigest, assertion, "approved-agent", "approved-client"} {
		if strings.Contains(fmt.Sprint(delegations), private) {
			t.Fatal("delegation view exposed credentials or workload claims")
		}
	}
	otherProject := createProject(f.h, f.owner, "Other delegation project")
	delegationInvestigation := f.h.want(f.owner, http.MethodPost, "/api/v1/investigations", map[string]any{
		"project_id": project, "filters": map[string]any{}, "evidence": []any{map[string]any{"kind": "delegation", "id": delegationID}}, "summary": "Review the delegation",
	}, idem("delegation-investigation"), http.StatusCreated)
	delegationInvestigationID := mcpExecString(t, delegationInvestigation, "id")
	delegationEvidence := mcpExecItems(t, delegationInvestigation, "evidence")
	if len(delegationEvidence) != 1 || mcpExecString(t, mcpExecItem(t, delegationEvidence, 0, delegationInvestigation), "id") != delegationID {
		t.Fatalf("investigation did not retain its delegation reference: %v", delegationInvestigation)
	}
	f.h.want(f.owner, http.MethodPost, "/api/v1/investigations", map[string]any{
		"project_id": otherProject, "filters": map[string]any{}, "evidence": []any{map[string]any{"kind": "delegation", "id": delegationID}}, "summary": "Foreign delegation",
	}, idem("foreign-delegation-investigation"), http.StatusUnprocessableEntity)
	viewer := f.h.invite(f.owner, "delegation-viewer@example.com", "viewer")
	viewerSession := f.h.want(viewer, http.MethodGet, "/api/v1/sessions/current", nil, nil, http.StatusOK)
	viewerID := mcpExecString(t, mcpExecObject(t, viewerSession, "user"), "id")
	viewerRecord := f.h.want(f.owner, http.MethodGet, "/api/v1/users/"+viewerID, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPatch, "/api/v1/users/"+viewerID, map[string]any{"access_scope": "assigned"}, etagHeader(viewerRecord), http.StatusOK)
	addMember(f.h, f.owner, otherProject, viewerID, "viewer")
	assignedViewer := login(f.h, "delegation-viewer@example.com")
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/investigations/"+delegationInvestigationID, nil, nil, http.StatusNotFound)
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusNotFound)
	f.h.want(assignedViewer, http.MethodGet, delegationPath, nil, nil, http.StatusNotFound)
	if filtered := f.h.want(assignedViewer, http.MethodGet, "/api/v1/agent-delegations", nil, nil, http.StatusOK); len(mcpExecItems(t, filtered, "items")) != 0 {
		t.Fatalf("project viewer could see another project's delegations: %v", filtered)
	}
	addMember(f.h, f.owner, project, viewerID, "viewer")
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/investigations/"+delegationInvestigationID, nil, nil, http.StatusOK)
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusOK)
	f.h.want(assignedViewer, http.MethodGet, delegationPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodDelete, "/api/v1/projects/"+project+"/members/"+viewerID, nil, projectEtag(f.h, f.owner, project), http.StatusNoContent)
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/investigations/"+delegationInvestigationID, nil, nil, http.StatusNotFound)
	f.h.want(assignedViewer, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusNotFound)
	f.h.want(assignedViewer, http.MethodGet, delegationPath, nil, nil, http.StatusNotFound)
	f.h.refresh()
	callerReport := f.h.want(f.owner, http.MethodGet, "/api/v1/api-keys/"+delegating["id"].(string), nil, nil, http.StatusOK)
	agentReport := f.h.want(f.owner, http.MethodGet, "/api/v1/api-keys/"+agentID, nil, nil, http.StatusOK)
	callerAccrued := callerReport["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	agentAccrued := agentReport["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	pairCost, pairOK := new(big.Rat).SetString(pairAccrued)
	callerCost, callerOK := new(big.Rat).SetString(callerAccrued)
	agentCost, agentOK := new(big.Rat).SetString(agentAccrued)
	if !pairOK || !callerOK || !agentOK || pairCost.Sign() <= 0 || pairCost.Cmp(callerCost) != 0 || pairCost.Cmp(agentCost) != 0 {
		t.Fatalf("delegated tool spend differs across principals: pair=%s caller=%s agent=%s", pairAccrued, callerAccrued, agentAccrued)
	}
	connection, err := f.h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), connection.Conn(), time.Now())
	connection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{delegating["id"].(string), agentID, limits.AttributionBudgetID(project, "delegation_pair", grant.PairDigest)} {
		if !slices.ContainsFunc(snapshots, func(snapshot limits.CostSnapshot) bool {
			amount, ok := new(big.Rat).SetString(snapshot.DailyAccrued)
			return snapshot.CostOwnerID == owner && snapshot.Basis == limits.BasisCost && ok && amount.Cmp(pairCost) == 0
		}) {
			t.Fatalf("reconciliation lost settled tool spend for %s", owner)
		}
	}
	renewedResponse, renewedBody := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": delegating["secret"].(string)})
	var renewedGrant struct {
		Token      string `json:"token"`
		PairDigest string `json:"delegation_pair_digest"`
	}
	if renewedResponse.StatusCode != http.StatusCreated || json.Unmarshal(renewedBody, &renewedGrant) != nil || renewedGrant.Token == "" || renewedGrant.PairDigest != grant.PairDigest {
		t.Fatalf("renewal did not retain the same pair: %d", renewedResponse.StatusCode)
	}
	renewedCall := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", bytes.NewReader(mustJSON(map[string]any{"toolset_id": f.toolsetID, "name": f.alias, "arguments": map[string]any{}, "call_id": "delegated-tool"})))
	renewedCall.Header.Set("Authorization", "Bearer "+renewedGrant.Token)
	renewedCall.Header.Set("X-OLP-Agent-Identity", assertion)
	renewedCall.Header.Set("Content-Type", "application/json")
	renewedResult := httptest.NewRecorder()
	handler.ServeHTTP(renewedResult, renewedCall)
	var initialOutcome, renewedOutcome struct {
		AttemptID string `json:"attempt_id"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &initialOutcome) != nil || json.Unmarshal(renewedResult.Body.Bytes(), &renewedOutcome) != nil || renewedResult.Code != http.StatusOK || initialOutcome.AttemptID == "" || initialOutcome.AttemptID != renewedOutcome.AttemptID {
		t.Fatalf("same pair renewal did not retain the original result: %d %s", renewedResult.Code, renewedResult.Body.String())
	}
	select {
	case <-upstreamCalls:
		t.Fatal("same pair renewal dispatched the retained call twice")
	default:
	}
	options := f.h.want(f.owner, http.MethodGet, "/api/v1/delegation-options?api_key_id="+delegating["id"].(string), nil, nil, http.StatusOK)
	if !slices.ContainsFunc(options["agents"].([]any), func(item any) bool {
		option := item.(map[string]any)
		return option["id"] == agentID && slices.Contains(option["allowed_toolsets"].([]any), any(f.toolsetID))
	}) {
		t.Fatalf("member consent omitted an eligible toolset: %v", options)
	}
	f.h.want(f.owner, http.MethodPost, "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": delegating["id"]}, nil, http.StatusNoContent)
	consent := f.h.want(f.owner, http.MethodPost, "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": delegating["id"], "scopes": []string{"tools"}, "allowed_toolsets": []string{f.toolsetID}}, nil, http.StatusCreated)
	memberResponse, memberBody := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": consent["consent"].(string)})
	if memberResponse.StatusCode != http.StatusCreated {
		t.Fatalf("member tool delegation exchange: %d %s", memberResponse.StatusCode, memberBody)
	}
	var memberGrant struct {
		Token      string `json:"token"`
		PairDigest string `json:"delegation_pair_digest"`
	}
	if err := json.Unmarshal(memberBody, &memberGrant); err != nil || memberGrant.Token == "" {
		t.Fatalf("member tool delegation missing token: %v", err)
	}
	memberCall := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(string(mustJSON(struct {
		ToolsetID string   `json:"toolset_id"`
		Name      string   `json:"name"`
		Arguments struct{} `json:"arguments"`
		CallID    string   `json:"call_id"`
	}{ToolsetID: f.toolsetID, Name: f.alias, CallID: "delegated-tool"}))))
	memberCall.Header.Set("Authorization", "Bearer "+memberGrant.Token)
	memberCall.Header.Set("X-OLP-Agent-Identity", assertion)
	memberCall.Header.Set("Content-Type", "application/json")
	memberResult := httptest.NewRecorder()
	handler.ServeHTTP(memberResult, memberCall)
	if memberResult.Code != http.StatusOK {
		t.Fatalf("member tool delegation failed: %d %s", memberResult.Code, memberResult.Body.String())
	}
	<-upstreamCalls
	memberSignedToken := <-signedTokens
	var memberOutcome struct {
		AttemptID string `json:"attempt_id"`
	}
	if json.Unmarshal(memberResult.Body.Bytes(), &memberOutcome) != nil || memberOutcome.AttemptID == "" || memberOutcome.AttemptID == initialOutcome.AttemptID || memberGrant.PairDigest == "" || memberGrant.PairDigest == grant.PairDigest {
		t.Fatal("member delegation shared the key delegation's retained result or pair")
	}
	profile := f.h.want(f.owner, http.MethodGet, "/api/v1/profile", nil, nil, http.StatusOK)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.agent_delegations SET revoked_at=now() WHERE id=$1`, delegation["id"]); err != nil {
		t.Fatal(err)
	}
	delegations = f.h.want(f.owner, http.MethodGet, "/api/v1/agent-delegations?project_id="+project, nil, nil, http.StatusOK)
	items = mcpExecItems(t, delegations, "items")
	if !slices.ContainsFunc(items, func(value any) bool {
		item := value.(map[string]any)
		return item["id"] == delegation["id"] && item["revoked_at"] != nil
	}) || !slices.ContainsFunc(items, func(value any) bool {
		item := value.(map[string]any)
		amount, ok := new(big.Rat).SetString(mcpExecString(t, mcpExecObject(t, mcpExecObject(t, item, "budget"), "daily"), "accrued"))
		return item["member_id"] == profile["id"] && item["agent_id"] == agentID && item["budget_policy"] == nil && ok && amount.Sign() > 0
	}) {
		t.Fatalf("delegation view lost revocation or the member pair's unconfigured spend: %v", delegations)
	}
	if len(items) != 3 {
		t.Fatalf("expected the original, renewed and member delegations: %v", delegations)
	}
	for index := range items {
		item := mcpExecItem(t, items, index, delegations)
		if item["member_id"] == nil && (mcpExecObject(t, mcpExecObject(t, item, "budget"), "daily")["accrued"] != pairAccrued || mcpExecObject(t, item, "budget_policy")["daily_cost_limit"] != "1000") {
			t.Fatalf("renewed delegations lost their shared pair budget: %v", item)
		}
	}
	pagePath := "/api/v1/agent-delegations?project_id=" + project + "&limit=1"
	firstPage := f.h.want(f.owner, http.MethodGet, pagePath, nil, nil, http.StatusOK)
	secondPage := f.h.want(f.owner, http.MethodGet, pagePath+"&cursor="+mcpExecString(t, firstPage, "next_cursor"), nil, nil, http.StatusOK)
	thirdPage := f.h.want(f.owner, http.MethodGet, pagePath+"&cursor="+mcpExecString(t, secondPage, "next_cursor"), nil, nil, http.StatusOK)
	for index, page := range []map[string]any{firstPage, secondPage, thirdPage} {
		pageItems := mcpExecItems(t, page, "items")
		if len(pageItems) != 1 {
			t.Fatalf("delegation page did not contain one item: %v", page)
		}
		item := mcpExecItem(t, pageItems, 0, page)
		expected := mcpExecItem(t, items, index, delegations)
		if item["id"] != expected["id"] || item["member_id"] != expected["member_id"] || mcpExecObject(t, mcpExecObject(t, item, "budget"), "daily")["accrued"] != mcpExecObject(t, mcpExecObject(t, expected, "budget"), "daily")["accrued"] {
			t.Fatalf("delegation pagination changed order or pair spend: %v", page)
		}
	}
	if thirdPage["next_cursor"] != nil {
		t.Fatalf("last delegation page has a next cursor: %v", thirdPage)
	}
	var attemptAgent, attemptMember, attemptPair, stored string
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT agent_principal_id::text,member_principal_id::text,delegation_pair_digest FROM olp.mcp_tool_calls WHERE id=$1`, memberOutcome.AttemptID).Scan(&attemptAgent, &attemptMember, &attemptPair); err != nil {
		t.Fatal(err)
	}
	if attemptAgent != agentID || attemptMember != profile["id"].(string) || attemptPair != memberGrant.PairDigest {
		t.Fatal("member tool attempt did not retain its agent, member, and pair attribution")
	}
	if err := f.h.Pool.QueryRow(t.Context(), `SELECT string_agg(row_to_json(c)::text, E'\n') FROM olp.mcp_tool_calls c WHERE api_key_id=$1`, delegating["id"]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	export := f.h.want(f.owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	exported, err := json.Marshal(export)
	if err != nil || len(exported) == 0 {
		t.Fatal("configuration export is missing")
	}
	if !strings.HasPrefix(memberGrant.Token, "olpd_") || strings.Count(assertion, ".") != 2 || strings.Count(memberSignedToken.value, ".") != 2 {
		t.Fatal("credential positive controls are missing")
	}
	for name, secret := range map[string]string{"delegation grant": memberGrant.Token, "workload assertion": assertion, "downstream JWT": memberSignedToken.value} {
		control, err := json.Marshal(map[string]string{"credential": secret})
		if err != nil || !strings.Contains(string(control), secret) {
			t.Fatalf("%s was not detected in the positive control", name)
		}
		if strings.Contains(stored, secret) || strings.Contains(string(exported), secret) {
			t.Fatalf("%s appeared in durable tool attempt or export", name)
		}
	}
	secondClaims, err := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": "second-agent", "client_id": "approved-client", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	secondSigned, err := signer.Sign(secondClaims)
	if err != nil {
		t.Fatal(err)
	}
	secondAssertion, err := secondSigned.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	secondToken, secondPair := issueToolGrant(delegating["secret"].(string), secondAssertion)
	secondResult := callTool(secondToken, secondAssertion, "delegated-tool")
	var secondOutcome struct {
		AttemptID string `json:"attempt_id"`
	}
	if secondResult.Code != http.StatusOK || json.Unmarshal(secondResult.Body.Bytes(), &secondOutcome) != nil || secondOutcome.AttemptID == "" || secondOutcome.AttemptID == initialOutcome.AttemptID || secondPair == grant.PairDigest {
		t.Fatalf("distinct agent shared another agent's retained result: %d %s", secondResult.Code, secondResult.Body.String())
	}
	select {
	case <-upstreamCalls:
	default:
		t.Fatal("distinct agent did not dispatch its own call")
	}
	secondSignedToken := <-signedTokens
	secondAgentID := strings.TrimPrefix(secondSignedToken.subject, "agent:")
	if _, err := uuid.Parse(secondAgentID); err != nil || secondAgentID == agentID {
		t.Fatal("second agent received an invalid or shared downstream identity")
	}
	secondKey := f.h.want(f.owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "second delegating tools", "project_id": project, "scopes": []string{"tools", "delegate"}, "allowed_toolsets": []string{f.toolsetID}}, idem("second-delegating-tools-key"), http.StatusCreated)
	f.completePKCE(t, f.claimKey(t, mcpPersonalKey{ID: secondKey["id"].(string), Secret: secondKey["secret"].(string)}), testutil.OAuthIdentity{Subject: "second", Account: "agent-account"})
	templatesPath := "/api/v1/projects/" + project + "/limit-templates"
	concurrencyTemplates := f.h.want(f.owner, http.MethodGet, templatesPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "max_concurrency": 1}}}, etagHeader(concurrencyTemplates), http.StatusOK)
	f.h.refresh()
	firstOverlapToken, _ := issueToolGrant(delegating["secret"].(string), assertion)
	secondOverlapToken, _ := issueToolGrant(secondKey["secret"].(string), assertion)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHeld()
	holdNext <- release
	firstCompleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstCompleted <- callTool(firstOverlapToken, assertion, "concurrent-first") }()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("first agent call did not reach the upstream")
	}
	<-upstreamCalls
	concurrentResult := callTool(secondOverlapToken, assertion, "concurrent-second")
	if concurrentResult.Code != http.StatusTooManyRequests {
		t.Fatalf("agent concurrency did not refuse an overlapping second key: %d %s", concurrentResult.Code, concurrentResult.Body.String())
	}
	select {
	case <-upstreamCalls:
		t.Fatal("agent concurrency refusal reached the upstream")
	default:
	}
	releaseHeld()
	if completed := <-firstCompleted; completed.Code != http.StatusOK {
		t.Fatalf("held agent call did not finish: %d %s", completed.Code, completed.Body.String())
	}
	<-signedTokens
	retryToken, _ := issueToolGrant(secondKey["secret"].(string), assertion)
	if retried := callTool(retryToken, assertion, "concurrent-retry"); retried.Code != http.StatusOK {
		t.Fatalf("agent concurrency lease did not release: %d %s", retried.Code, retried.Body.String())
	}
	<-upstreamCalls
	<-signedTokens
	afterConcurrencyTemplates := f.h.want(f.owner, http.MethodGet, templatesPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(afterConcurrencyTemplates), http.StatusOK)
	group := f.h.want(f.owner, http.MethodPost, "/api/v1/budget-groups", map[string]any{"name": "delegated tool callers", "project_id": project, "requests_per_minute": 100, "max_concurrency": 1}, idem("delegated-tool-group"), http.StatusCreated)
	groupKey := func(name, idempotency string) map[string]any {
		return f.h.want(f.owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": name, "project_id": project, "budget_group_id": group["id"], "scopes": []string{"tools", "delegate"}, "allowed_toolsets": []string{f.toolsetID}}, idem(idempotency), http.StatusCreated)
	}
	firstGroupKey := groupKey("first group tool caller", "first-group-tool-caller")
	secondGroupKey := groupKey("second group tool caller", "second-group-tool-caller")
	f.completePKCE(t, f.claimKey(t, mcpPersonalKey{ID: firstGroupKey["id"].(string), Secret: firstGroupKey["secret"].(string)}), testutil.OAuthIdentity{Subject: "group", Account: "agent-account"})
	f.h.refresh()
	firstGroupToken, _ := issueToolGrant(firstGroupKey["secret"].(string), assertion)
	secondGroupToken, _ := issueToolGrant(secondGroupKey["secret"].(string), assertion)
	groupRelease := make(chan struct{})
	var groupReleaseOnce sync.Once
	releaseGroup := func() { groupReleaseOnce.Do(func() { close(groupRelease) }) }
	defer releaseGroup()
	holdNext <- groupRelease
	groupCompleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { groupCompleted <- callTool(firstGroupToken, assertion, "group-first") }()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("first group call did not reach the upstream")
	}
	<-upstreamCalls
	if denied := callTool(secondGroupToken, assertion, "group-concurrent"); denied.Code != http.StatusTooManyRequests {
		t.Fatalf("group concurrency did not refuse the overlapping call: %d %s", denied.Code, denied.Body.String())
	}
	select {
	case <-upstreamCalls:
		t.Fatal("group concurrency refusal reached the upstream")
	default:
	}
	releaseGroup()
	if completed := <-groupCompleted; completed.Code != http.StatusOK {
		t.Fatalf("held group call did not finish: %d %s", completed.Code, completed.Body.String())
	}
	<-signedTokens
	groupPath := "/api/v1/budget-groups/" + group["id"].(string)
	currentGroup := f.h.want(f.owner, http.MethodGet, groupPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPatch, groupPath, map[string]any{"requests_per_minute": 1}, withMatch(currentGroup, idem("limit-delegated-tool-group")), http.StatusOK)
	f.h.refresh()
	groupRateDenied := false
	for attempt := 0; attempt < 3 && !groupRateDenied; attempt++ {
		minute := time.Now().UTC().Truncate(time.Minute)
		groupRateSeed, _ := issueToolGrant(firstGroupKey["secret"].(string), assertion)
		seeded := callTool(groupRateSeed, assertion, fmt.Sprintf("group-rate-seed-%d", attempt))
		if seeded.Code == http.StatusOK {
			<-upstreamCalls
			<-signedTokens
		} else if seeded.Code != http.StatusTooManyRequests {
			t.Fatalf("could not fill the group request window: %d %s", seeded.Code, seeded.Body.String())
		}
		groupRateToken, _ := issueToolGrant(secondGroupKey["secret"].(string), assertion)
		denied := callTool(groupRateToken, assertion, fmt.Sprintf("group-rate-%d", attempt))
		select {
		case <-upstreamCalls:
			t.Fatal("group request limit refusal reached the upstream")
		default:
		}
		if denied.Code == http.StatusTooManyRequests {
			groupRateDenied = true
		} else if denied.Code != http.StatusForbidden || minute.Equal(time.Now().UTC().Truncate(time.Minute)) {
			t.Fatalf("group request limit did not refuse the call: %d %s", denied.Code, denied.Body.String())
		}
	}
	if !groupRateDenied {
		t.Fatal("group request limit did not refuse a call within three windows")
	}
	sessionResponse, sessionBody := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": delegating["secret"].(string)})
	if sessionResponse.StatusCode != http.StatusCreated {
		t.Fatalf("session delegation exchange: %d %s", sessionResponse.StatusCode, sessionBody)
	}
	var sessionGrant struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(sessionBody, &sessionGrant); err != nil || sessionGrant.Token == "" {
		t.Fatalf("session delegation missing token: %v", err)
	}
	sessionRPCFor := func(token, agentAssertion, session, method string, id *int, params json.RawMessage) *httptest.ResponseRecorder {
		rpc := httptest.NewRequest(http.MethodPost, "/mcp?toolset_id="+f.toolsetID, bytes.NewReader(mustJSON(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}{JSONRPC: "2.0", ID: id, Method: method, Params: params})))
		rpc.Header.Set("Authorization", "Bearer "+token)
		rpc.Header.Set("X-OLP-Agent-Identity", agentAssertion)
		rpc.Header.Set("Content-Type", "application/json")
		rpc.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			rpc.Header.Set("Mcp-Session-Id", session)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, rpc)
		return out
	}
	sessionRPC := func(token, session, method string, id *int, params json.RawMessage) *httptest.ResponseRecorder {
		return sessionRPCFor(token, assertion, session, method, id, params)
	}
	rpcID := 1
	initialize := mustJSON(mcpsdk.InitializeParams{Capabilities: &mcpsdk.ClientCapabilities{}, ClientInfo: &mcpsdk.Implementation{Name: "agent-fixture", Version: "1"}, ProtocolVersion: server.Catalog.Protocol})
	opened := sessionRPC(sessionGrant.Token, "", "initialize", &rpcID, initialize)
	sessionID := opened.Header().Get("Mcp-Session-Id")
	if opened.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("delegated MCP session did not initialize: %d %s", opened.Code, opened.Body.String())
	}
	initialized := sessionRPC(sessionGrant.Token, sessionID, "notifications/initialized", nil, nil)
	if initialized.Code != http.StatusAccepted {
		t.Fatalf("delegated MCP session could not continue: %d %s", initialized.Code, initialized.Body.String())
	}
	listed := sessionRPC(sessionGrant.Token, sessionID, "tools/list", &rpcID, nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), f.alias) {
		t.Fatalf("delegated MCP session lost its tools: %d %s", listed.Code, listed.Body.String())
	}
	createdAgain := sessionRPC(sessionGrant.Token, sessionID, "initialize", &rpcID, initialize)
	if createdAgain.Code != http.StatusUnauthorized {
		t.Fatalf("consumed grant created another session: %d %s", createdAgain.Code, createdAgain.Body.String())
	}
	otherResponse, otherBody := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": delegating["secret"].(string)})
	if otherResponse.StatusCode != http.StatusCreated {
		t.Fatalf("second session delegation: %d %s", otherResponse.StatusCode, otherBody)
	}
	var otherGrant struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(otherBody, &otherGrant); err != nil || otherGrant.Token == "" {
		t.Fatalf("second session delegation missing token: %v", err)
	}
	otherOpened := sessionRPC(otherGrant.Token, "", "initialize", &rpcID, initialize)
	otherSessionID := otherOpened.Header().Get("Mcp-Session-Id")
	if otherOpened.Code != http.StatusOK || otherSessionID == "" {
		t.Fatalf("second delegated MCP session did not initialize: %d %s", otherOpened.Code, otherOpened.Body.String())
	}
	otherInitialized := sessionRPC(otherGrant.Token, otherSessionID, "notifications/initialized", nil, nil)
	if otherInitialized.Code != http.StatusAccepted {
		t.Fatalf("second delegated MCP session could not continue: %d %s", otherInitialized.Code, otherInitialized.Body.String())
	}
	wrongSession := sessionRPC(otherGrant.Token, sessionID, "tools/list", &rpcID, nil)
	if wrongSession.Code == http.StatusOK {
		t.Fatalf("different delegation accessed existing session: %s", wrongSession.Body.String())
	}
	originalWrongSession := sessionRPC(sessionGrant.Token, otherSessionID, "tools/list", &rpcID, nil)
	if originalWrongSession.Code == http.StatusOK {
		t.Fatalf("original delegation accessed a different session: %s", originalWrongSession.Body.String())
	}
	openSession := func(token, agentAssertion string) string {
		t.Helper()
		opened := sessionRPCFor(token, agentAssertion, "", "initialize", &rpcID, initialize)
		id := opened.Header().Get("Mcp-Session-Id")
		if opened.Code != http.StatusOK || id == "" || sessionRPCFor(token, agentAssertion, id, "notifications/initialized", nil, nil).Code != http.StatusAccepted {
			t.Fatal("delegated session did not initialize")
		}
		return id
	}
	secondAgentSessionToken, _ := issueToolGrant(delegating["secret"].(string), secondAssertion)
	secondAgentSession := openSession(secondAgentSessionToken, secondAssertion)
	secondKeySessionToken, _ := issueToolGrant(secondKey["secret"].(string), assertion)
	secondKeySession := openSession(secondKeySessionToken, assertion)
	sessionCall := func(token, agentAssertion, session, callID string) *httptest.ResponseRecorder {
		return sessionRPCFor(token, agentAssertion, session, "tools/call", &rpcID, mustJSON(map[string]any{"name": f.alias, "arguments": map[string]any{}, "_meta": map[string]any{"olp.call_id": callID}}))
	}
	for _, test := range []struct{ token, agentAssertion, session, name string }{
		{sessionGrant.Token, assertion, sessionID, "resource"},
		{secondAgentSessionToken, secondAssertion, secondAgentSession, "agent"},
		{secondKeySessionToken, assertion, secondKeySession, "key"},
	} {
		if result := sessionCall(test.token, test.agentAssertion, test.session, "before-"+test.name); result.Code != http.StatusOK {
			t.Fatalf("%s session could not call tool before revocation: %d %s", test.name, result.Code, result.Body.String())
		}
		select {
		case <-upstreamCalls:
		default:
			t.Fatalf("%s session did not dispatch before revocation", test.name)
		}
		<-signedTokens
	}
	assertBudgetRefusal := func(reason string) {
		t.Helper()
		exchangeResponse, exchangeBody := f.h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": delegating["secret"].(string)})
		if exchangeResponse.StatusCode != http.StatusCreated {
			t.Fatalf("renew %s delegation: %d %s", reason, exchangeResponse.StatusCode, exchangeBody)
		}
		var renewed struct {
			Token      string `json:"token"`
			PairDigest string `json:"delegation_pair_digest"`
		}
		if err := json.Unmarshal(exchangeBody, &renewed); err != nil || renewed.Token == "" || renewed.PairDigest != grant.PairDigest {
			t.Fatalf("renew %s delegation lost its pair: %v", reason, err)
		}
		call := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(string(mustJSON(struct {
			ToolsetID string   `json:"toolset_id"`
			Name      string   `json:"name"`
			Arguments struct{} `json:"arguments"`
			CallID    string   `json:"call_id"`
		}{ToolsetID: f.toolsetID, Name: f.alias, CallID: "budget-" + reason}))))
		call.Header.Set("Authorization", "Bearer "+renewed.Token)
		call.Header.Set("X-OLP-Agent-Identity", assertion)
		call.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, call)
		if result.Code != http.StatusTooManyRequests {
			t.Fatalf("%s budget did not refuse the delegated tool: %d %s", reason, result.Code, result.Body.String())
		}
		select {
		case <-upstreamCalls:
			t.Fatalf("%s budget refusal dispatched to the upstream", reason)
		default:
		}
	}
	limit := pairCost.FloatString(12)
	templates := f.h.want(f.owner, http.MethodGet, templatesPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": limit}}}, etagHeader(templates), http.StatusOK)
	f.h.refresh()
	assertBudgetRefusal("agent")
	updatedTemplates := f.h.want(f.owner, http.MethodGet, templatesPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, templatesPath, map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100}}}, etagHeader(updatedTemplates), http.StatusOK)
	updatedPair := f.h.want(f.owner, http.MethodGet, pairPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.PairDigest: map[string]any{"daily_cost_limit": limit}}}}, etagHeader(updatedPair), http.StatusOK)
	f.h.refresh()
	assertBudgetRefusal("pair")
	limitedPair := f.h.want(f.owner, http.MethodGet, pairPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPut, pairPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.PairDigest: map[string]any{"daily_cost_limit": "1000"}}}}, etagHeader(limitedPair), http.StatusOK)
	keyPath := "/api/v1/api-keys/" + delegating["id"].(string)
	keyPolicy := f.h.want(f.owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPatch, keyPath, map[string]any{"daily_cost_limit": limit}, etagHeader(keyPolicy), http.StatusOK)
	f.h.refresh()
	assertBudgetRefusal("key")
	assertSessionRefused := func(reason, token, agentAssertion, session string) {
		t.Helper()
		result := sessionCall(token, agentAssertion, session, "after-"+reason)
		if result.Code != http.StatusUnauthorized && result.Code != http.StatusForbidden && result.Code != http.StatusNotFound {
			t.Fatalf("%s did not refuse the session before dispatch: %d %s", reason, result.Code, result.Body.String())
		}
		select {
		case <-upstreamCalls:
			t.Fatalf("%s session reached the upstream", reason)
		default:
		}
	}
	secondAgentPath := "/api/v1/api-keys/" + secondAgentID
	secondAgentPolicy := f.h.want(f.owner, http.MethodGet, secondAgentPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPost, secondAgentPath+"/revoke", nil, withMatch(secondAgentPolicy, idem("revoke-delegated-agent")), http.StatusOK)
	f.h.refresh()
	assertSessionRefused("agent", secondAgentSessionToken, secondAssertion, secondAgentSession)
	secondKeyPath := "/api/v1/api-keys/" + secondKey["id"].(string)
	secondKeyPolicy := f.h.want(f.owner, http.MethodGet, secondKeyPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPost, secondKeyPath+"/revoke", nil, withMatch(secondKeyPolicy, idem("revoke-delegating-key")), http.StatusOK)
	f.h.refresh()
	assertSessionRefused("key", secondKeySessionToken, assertion, secondKeySession)
	currentKey := f.h.want(f.owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	f.h.want(f.owner, http.MethodPatch, keyPath, map[string]any{"allowed_toolsets": []string{}}, withMatch(currentKey, idem("remove-delegated-toolset")), http.StatusOK)
	f.h.refresh()
	assertSessionRefused("toolset", sessionGrant.Token, assertion, sessionID)
}

func TestWorkloadAgentDelegatesKeyForA2A(t *testing.T) {
	h := newAccessHarness(t)
	h.Server.ClientIP = func(request *http.Request) string {
		if source := request.Header.Get("X-Test-Client-IP"); source != "" {
			return source
		}
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		return host
	}
	owner := h.owner()
	project := createProject(h, owner, "Delegated A2A")
	otherProject := createProject(h, owner, "Other delegated A2A")
	for _, address := range []string{"a2a-manager@example.com", "a2a-viewer@example.com", "a2a-other@example.com"} {
		h.invite(owner, address, "operator")
	}
	users := h.want(owner, http.MethodGet, "/api/v1/users", nil, nil, http.StatusOK)
	for _, entry := range users["items"].([]any) {
		user := entry.(map[string]any)
		email := user["email"].(string)
		if !slices.Contains([]string{"a2a-manager@example.com", "a2a-viewer@example.com", "a2a-other@example.com"}, email) {
			continue
		}
		id := user["id"].(string)
		h.want(owner, http.MethodPatch, "/api/v1/users/"+id, map[string]any{"access_scope": "assigned"}, etagHeader(user), http.StatusOK)
		if email == "a2a-other@example.com" {
			addMember(h, owner, otherProject, id, "manager")
		} else if email == "a2a-viewer@example.com" {
			addMember(h, owner, project, id, "viewer")
		} else {
			addMember(h, owner, project, id, "manager")
		}
	}
	manager := login(h, "a2a-manager@example.com")
	viewer := login(h, "a2a-viewer@example.com")
	other := login(h, "a2a-other@example.com")
	modelProvider := newOpenAIFixture(t, "")
	model := activeAzureProviderInProject(t, h, owner, "Delegated callback provider", modelProvider.URL, &project, []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}})
	draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{"project_id": project, "slug": "delegated-callback", "overall_timeout_ms": 10000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"}, "targets": []any{map[string]any{"provider_id": model["id"], "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}}}, idem("delegated-callback-route"), http.StatusCreated)
	path := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := h.want(owner, http.MethodPost, path+"/validate", nil, etagHeader(draft), http.StatusOK)
	h.want(owner, http.MethodPost, path+"/activate", nil, withMatch(validated, idem("delegated-callback-activate")), http.StatusOK)
	workloadProject(t, h, owner, project, map[string]any{"bootstrap": []string{"delegated-callback"}})
	templates := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "10"}}}, etagHeader(templates), http.StatusOK)
	h.refresh()
	routeRevision := h.Runtime.Release().Snapshot.Routes["delegated-callback"].RevisionID
	callbackStatus := make(chan int, 1)
	callbackCapability := make(chan string, 1)
	cancellation := make(chan string, 4)
	var watchCancellation atomic.Bool
	var cardUnavailable atomic.Bool
	var skipCallback atomic.Bool
	var requireOAuth atomic.Bool
	var originalOAuthCalls, replacementOAuthCalls atomic.Int32
	var remoteRequests atomic.Int32
	var holdRemote atomic.Bool
	holdStarted := make(chan struct{}, 1)
	holdRelease := make(chan struct{}, 2)
	oauthAuthority := testutil.NewOAuthServer(t)
	client, prefix, _ := acctKeyspace(t)
	limiter, err := limits.New(client, strings.TrimSuffix(prefix, ":"))
	if err != nil {
		t.Fatal(err)
	}
	h.Gateway.Admission = gateway.NewAdmission(limiter, nil, nil)
	h.A2A.Ledger = &a2a.Ledger{Pool: h.Pool, Store: &limits.DebitStore{Pool: h.Pool, Limiter: limiter}, Auth: h.AuthKey}
	var upstreamCalls atomic.Int32
	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(func(ctx context.Context, execution *a2asrv.ExecutorContext) iter.Seq2[protocol.Event, error] {
		return func(yield func(protocol.Event, error) bool) {
			upstreamCalls.Add(1)
			if holdRemote.Load() {
				holdStarted <- struct{}{}
				select {
				case <-holdRelease:
				case <-ctx.Done():
					return
				}
			}
			if yield(protocol.NewSubmittedTask(execution, execution.Message), nil) {
				yield(protocol.NewStatusUpdateEvent(execution, protocol.TaskStateInputRequired, nil), nil)
			}
		}
	})))
	cardFor := func(endpoint string) []byte {
		card, marshalErr := json.Marshal(protocol.AgentCard{
			Name: "Fixture agent", Description: "Delegated task", Version: "1",
			SupportedInterfaces: []*protocol.AgentInterface{{URL: endpoint, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: protocol.Version}},
			Capabilities:        protocol.AgentCapabilities{Streaming: true}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}, Skills: []protocol.AgentSkill{},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return card
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			remoteRequests.Add(1)
			if requireOAuth.Load() {
				identity, ok := oauthAuthority.Authorized(r)
				if !ok || identity.Account != "original-account" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				originalOAuthCalls.Add(1)
			}
			if watchCancellation.Load() {
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if decodeErr := json.NewDecoder(r.Body).Decode(&request); decodeErr != nil {
					return
				}
				cancellation <- request.Method
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"cancel unavailable"}}`, request.ID)
				return
			}
			if capability := r.Header.Get("X-OLP-Task-Capability"); capability != "" && !skipCallback.Load() {
				callbackCapability <- capability
				callback, callbackErr := http.NewRequestWithContext(r.Context(), http.MethodPost, h.HTTP.URL+"/v1/chat/completions", strings.NewReader(`{"model":"delegated-callback","messages":[{"role":"user","content":"hello"}]}`))
				if callbackErr != nil {
					callbackStatus <- 0
					return
				}
				callback.Header.Set("Content-Type", "application/json")
				callback.Header.Set("Authorization", "Bearer "+capability)
				callbackResponse, callbackErr := http.DefaultClient.Do(callback)
				if callbackErr != nil {
					callbackStatus <- 0
					return
				}
				callbackResponse.Body.Close()
				callbackStatus <- callbackResponse.StatusCode
			}
			handler.ServeHTTP(a2aContentTypeWriter{w}, r)
		case "/.well-known/agent-card.json":
			if cardUnavailable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	digest := sha256.Sum256(cardFor(upstream.URL + "/rpc"))
	pricingRevision := h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{"effective_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "prices": []any{map[string]any{"provider_kind": "azure_openai", "provider_id": model["id"], "model": "unused-fixture-model", "operation": "generation", "currency": "USD", "input_per_million": "0", "output_per_million": "0"}}}, idem("delegated-a2a-pricing"), http.StatusCreated)
	pricing := mcpExecString(t, pricingRevision, "id")
	created := h.want(owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"project_id": project, "name": "delegated target",
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{routeRevision}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "remote_work_bound_seconds": 120},
	}, idem("delegated-a2a-create"), http.StatusCreated)
	target := created["id"].(string)
	published := h.want(owner, http.MethodPost, "/api/v1/agents/"+target+"/publish", nil, withMatch(created, idem("delegated-a2a-publish")), http.StatusOK)
	issuer := newWorkloadFixture(t, h, owner, project, []string{"bootstrap"})
	issuerPath := "/api/v1/workload-issuers/" + issuer.issuer["id"].(string)
	currentIssuer := h.want(owner, http.MethodGet, issuerPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, issuerPath, map[string]any{
		"name": "Delegated agent", "issuer": issuer.endpoint.URL, "jwks_url": issuer.endpoint.URL,
		"enabled": true, "audiences": []string{"olp"}, "algorithms": []string{"EdDSA"}, "max_lifetime_seconds": 600, "disabled_key_ids": []string{},
		"mappings": []any{
			map[string]any{"name": "agent", "match": map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"bootstrap"}, "allowed_agents": []string{target}, "scopes": []string{"agents", "inference"}, "agent_principal": true},
			map[string]any{"name": "other", "match": map[string]string{"/sub": "other-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{"bootstrap"}, "allowed_agents": []string{target}, "scopes": []string{"agents", "inference"}, "agent_principal": true},
		},
	}, etagHeader(currentIssuer), http.StatusOK)
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "delegating A2A key", "project_id": project, "scopes": []string{"agents", "inference", "delegate"}, "allowed_agents": []string{target}, "allowed_routes": []string{"delegated-callback"}, "daily_cost_limit": "10"}, idem("delegated-a2a-key"), http.StatusCreated)
	h.refresh()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: issuer.private}, (&jose.SignerOptions{}).WithHeader("kid", issuer.kid))
	if err != nil {
		t.Fatal(err)
	}
	assertionFor := func(subject string) string {
		t.Helper()
		now := time.Now()
		claims, marshalErr := json.Marshal(map[string]any{"iss": issuer.endpoint.URL, "aud": "olp", "sub": subject, "client_id": "approved-client", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		signed, signErr := signer.Sign(claims)
		if signErr != nil {
			t.Fatal(signErr)
		}
		value, compactErr := signed.CompactSerialize()
		if compactErr != nil {
			t.Fatal(compactErr)
		}
		return value
	}
	assertion := assertionFor("approved-agent")
	bounds := map[string]any{"scopes": []string{"agents", "inference"}, "allowed_agents": []string{target}, "allowed_routes": []string{"delegated-callback"}}
	deniedResponse, deniedBody := h.do(nil, http.MethodPost, "/v1/delegations", map[string]any{"scopes": []string{"agents"}, "allowed_agents": []string{acctID(t)}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key["secret"].(string)})
	if deniedResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("agent delegation widened publication access: %d %s", deniedResponse.StatusCode, deniedBody)
	}
	cardGrantResponse, cardGrantBody := h.do(nil, http.MethodPost, "/v1/delegations", map[string]any{"scopes": []string{"agents"}, "allowed_agents": []string{target}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key["secret"].(string)})
	if cardGrantResponse.StatusCode != http.StatusCreated {
		t.Fatalf("agents-only grant refused: %d %s", cardGrantResponse.StatusCode, cardGrantBody)
	}
	var cardGrant struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(cardGrantBody, &cardGrant); err != nil || cardGrant.Token == "" {
		t.Fatalf("missing agents-only grant: %v", err)
	}
	cardResponse, cardBody := h.do(nil, http.MethodGet, "/a2a/"+target+"/.well-known/agent-card.json", nil, map[string]string{"Authorization": "Bearer " + cardGrant.Token, "X-OLP-Agent-Identity": assertion})
	if cardResponse.StatusCode != http.StatusOK {
		t.Fatalf("agents-only grant cannot discover its agent: %d %s", cardResponse.StatusCode, cardBody)
	}
	response, body := h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key["secret"].(string)})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("agent delegation exchange: %d %s", response.StatusCode, body)
	}
	var grant struct {
		Token string `json:"token"`
		Pair  string `json:"delegation_pair_digest"`
	}
	if err := json.Unmarshal(body, &grant); err != nil || grant.Token == "" || grant.Pair == "" {
		t.Fatalf("missing agent delegation token: %v", err)
	}
	budgetPath := "/api/v1/projects/" + project + "/attribution-budgets"
	budget := h.want(owner, http.MethodGet, budgetPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, budgetPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.Pair: map[string]any{"daily_cost_limit": "10"}}}}, etagHeader(budget), http.StatusOK)
	h.refresh()
	connection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := limits.ReconciliationSnapshots(t.Context(), connection.Conn(), time.Now())
	connection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if _, _, applyErr := limiter.ApplyCostSnapshot(t.Context(), snapshot); applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	messageContent := "private-a2a-message-" + uuid.NewString()
	request := struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  a2aTestParams `json:"params"`
	}{JSONRPC: "2.0", ID: 1, Method: "SendMessage", Params: a2aTestParams{Message: &protocol.Message{ID: "delegated", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart(messageContent)}}}}
	sendResponse, sendBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + grant.Token, "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if sendResponse.StatusCode != http.StatusOK {
		t.Fatalf("delegated send: %d %s", sendResponse.StatusCode, sendBody)
	}
	var result struct {
		Result struct {
			Task protocol.Task `json:"task"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(sendBody, &result); err != nil || len(result.Error) != 0 || result.Result.Task.ID == "" {
		t.Fatalf("delegated task missing: %s %v", sendBody, err)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one remote send, got %d", upstreamCalls.Load())
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("delegated task callback refused: %d", status)
	}
	capability := <-callbackCapability
	operatorTaskPath := "/api/v1/a2a-tasks/" + string(result.Result.Task.ID)
	operatorState := h.want(manager, http.MethodGet, operatorTaskPath+"/remote", nil, nil, http.StatusOK)
	if operatorState["id"] != string(result.Result.Task.ID) || operatorState["contextId"] != string(result.Result.Task.ID) || operatorState["status"].(map[string]any)["state"] != string(protocol.TaskStateInputRequired) {
		t.Fatalf("project operator could not read delegated task state: %v", operatorState)
	}
	for _, action := range []struct{ method, suffix string }{{http.MethodGet, "/remote"}, {http.MethodPost, "/cancel"}} {
		h.want(viewer, action.method, operatorTaskPath+action.suffix, nil, nil, http.StatusForbidden)
		h.want(other, action.method, operatorTaskPath+action.suffix, nil, nil, http.StatusNotFound)
	}
	pairBudget := h.want(owner, http.MethodGet, budgetPath, nil, nil, http.StatusOK)["usage"].(map[string]any)["delegation_pair"].(map[string]any)[grant.Pair].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	principals := h.want(owner, http.MethodGet, "/api/v1/api-keys", nil, nil, http.StatusOK)["items"].([]any)
	agentID := func() string {
		for _, item := range principals {
			principal := item.(map[string]any)
			if principal["workload_issuer_id"] == issuer.issuer["id"] && principal["workload_mapping"] == "agent" {
				return principal["id"].(string)
			}
		}
		t.Fatal("authenticated workload principal is absent")
		return ""
	}()
	operatorTask := h.want(owner, http.MethodGet, operatorTaskPath, nil, nil, http.StatusOK)
	if operatorTask["agent_principal_id"] != agentID || operatorTask["member_principal_id"] != nil || operatorTask["delegation_pair_id"] != grant.Pair {
		t.Fatalf("task lost initiating delegation identity: %v", operatorTask)
	}
	for _, private := range []string{grant.Token, assertion, capability, messageContent} {
		if strings.Contains(fmt.Sprint(operatorTask), private) {
			t.Fatal("task exposed private delegation or message content")
		}
	}
	keyBudget := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+key["id"].(string), nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	agentBudget := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+agentID, nil, nil, http.StatusOK)["budget"].(map[string]any)["daily"].(map[string]any)["accrued"].(string)
	pairAmount, pairOK := new(big.Rat).SetString(pairBudget)
	keyAmount, keyOK := new(big.Rat).SetString(keyBudget)
	agentAmount, agentOK := new(big.Rat).SetString(agentBudget)
	if !pairOK || !keyOK || !agentOK || pairAmount.Sign() <= 0 || pairAmount.Cmp(keyAmount) != 0 || pairAmount.Cmp(agentAmount) != 0 {
		t.Fatalf("agent message fee differs across caller, agent and pair: %s %s %s", keyBudget, agentBudget, pairBudget)
	}
	getGrant := func(identity string) string {
		t.Helper()
		exchangeResponse, exchangeBody := h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + identity, "X-OLP-Delegating-Key": key["secret"].(string)})
		if exchangeResponse.StatusCode != http.StatusCreated {
			t.Fatalf("renew agent delegation: %d %s", exchangeResponse.StatusCode, exchangeBody)
		}
		var next struct {
			Token string `json:"token"`
		}
		if decodeErr := json.Unmarshal(exchangeBody, &next); decodeErr != nil || next.Token == "" {
			t.Fatalf("missing renewed delegation: %v", decodeErr)
		}
		return next.Token
	}
	connection, err = h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err = limits.ReconciliationSnapshots(t.Context(), connection.Conn(), time.Now())
	connection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, ownerID := range []string{key["id"].(string), agentID, limits.AttributionBudgetID(project, "delegation_pair", grant.Pair)} {
		if !slices.ContainsFunc(snapshots, func(snapshot limits.CostSnapshot) bool {
			amount, ok := new(big.Rat).SetString(snapshot.DailyAccrued)
			return snapshot.CostOwnerID == ownerID && snapshot.Basis == limits.BasisCost && ok && amount.Cmp(pairAmount) == 0
		}) {
			t.Fatalf("reconciliation lost delegated A2A spend for %s", ownerID)
		}
	}
	for _, snapshot := range snapshots {
		if _, _, applyErr := limiter.ApplyCostSnapshot(t.Context(), snapshot); applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	assertBudgetRefusal := func(principal string) {
		t.Helper()
		before := upstreamCalls.Load()
		_, denied := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + getGrant(assertion), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
		if !bytes.Contains(denied, []byte(`"error"`)) || upstreamCalls.Load() != before {
			t.Fatalf("%s budget did not refuse A2A before dispatch: %s", principal, denied)
		}
	}
	limit := pairAmount.FloatString(12)
	limitedAgent := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": limit}}}, etagHeader(limitedAgent), http.StatusOK)
	h.refresh()
	assertBudgetRefusal("agent")
	restoreAgent := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "10"}}}, etagHeader(restoreAgent), http.StatusOK)
	limitedPair := h.want(owner, http.MethodGet, budgetPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, budgetPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.Pair: map[string]any{"daily_cost_limit": limit}}}}, etagHeader(limitedPair), http.StatusOK)
	h.refresh()
	assertBudgetRefusal("pair")
	restorePair := h.want(owner, http.MethodGet, budgetPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, budgetPath, map[string]any{"budgets": map[string]any{"delegation_pair": map[string]any{grant.Pair: map[string]any{"daily_cost_limit": "10"}}}}, etagHeader(restorePair), http.StatusOK)
	keyPath := "/api/v1/api-keys/" + key["id"].(string)
	limitedKey := h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, keyPath, map[string]any{"daily_cost_limit": limit}, etagHeader(limitedKey), http.StatusOK)
	h.refresh()
	assertBudgetRefusal("caller")
	restoreKey := h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, keyPath, map[string]any{"daily_cost_limit": "10"}, etagHeader(restoreKey), http.StatusOK)
	h.refresh()
	streamGrant := getGrant(assertion)
	streamRequest := request
	streamRequest.Method = "SendStreamingMessage"
	streamResponse, streamBody := h.do(nil, http.MethodPost, "/a2a/"+target, streamRequest, map[string]string{"Authorization": "Bearer " + streamGrant, "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if streamResponse.StatusCode != http.StatusOK || !strings.HasPrefix(streamResponse.Header.Get("Content-Type"), "text/event-stream") || !bytes.Contains(streamBody, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("delegated streaming send failed: %d %s", streamResponse.StatusCode, streamBody)
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("streaming task callback refused: %d", status)
	}
	<-callbackCapability
	streamJSON, err := json.Marshal(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/a2a/"+target, bytes.NewReader(streamJSON))
	if err != nil {
		t.Fatal(err)
	}
	replayRequest.Header.Set("Content-Type", "application/json")
	replayRequest.Header.Set("Authorization", "Bearer "+streamGrant)
	replayRequest.Header.Set("X-OLP-Agent-Identity", assertion)
	replayRequest.Header.Set("A2A-Version", "1.0")
	replayResponse, err := http.DefaultClient.Do(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	replayResponse.Body.Close()
	if replayResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("consumed streaming grant was replayed: %d", replayResponse.StatusCode)
	}
	_, subscriptionBody := h.do(nil, http.MethodPost, "/a2a/"+target, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SubscribeToTask", "params": map[string]any{"id": result.Result.Task.ID}}, map[string]string{"Authorization": "Bearer " + getGrant(assertion), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(subscriptionBody, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("delegated resubscription failed: %s", subscriptionBody)
	}
	group := h.want(owner, http.MethodPost, "/api/v1/budget-groups", map[string]any{"name": "A2A request budget", "project_id": project, "requests_per_minute": 1, "daily_cost_limit": "10"}, idem("a2a-rate-group"), http.StatusCreated)
	groupKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "group A2A key", "project_id": project, "budget_group_id": group["id"], "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("a2a-rate-key"), http.StatusCreated)
	h.refresh()
	var groupPair string
	groupGrant := func() string {
		t.Helper()
		response, body := h.do(nil, http.MethodPost, "/v1/delegations", map[string]any{"scopes": []string{"agents"}, "allowed_agents": []string{target}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": groupKey["secret"].(string)})
		var issued struct {
			Token string `json:"token"`
			Pair  string `json:"delegation_pair_digest"`
		}
		if response.StatusCode != http.StatusCreated || json.Unmarshal(body, &issued) != nil || issued.Token == "" {
			t.Fatalf("group delegation failed: %d %s", response.StatusCode, body)
		}
		groupPair = issued.Pair
		return issued.Token
	}
	skipCallback.Store(true)
	groupFirst, groupFirstBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + groupGrant(), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if groupFirst.StatusCode != http.StatusOK || !bytes.Contains(groupFirstBody, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("first group request failed: %d %s", groupFirst.StatusCode, groupFirstBody)
	}
	callsBeforeGroupLimit := upstreamCalls.Load()
	_, groupDenied := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + groupGrant(), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(groupDenied, []byte("Agent send rate exceeded")) || upstreamCalls.Load() != callsBeforeGroupLimit {
		t.Fatalf("group request limit did not stop the upstream send: %s", groupDenied)
	}
	groupConnection, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	groupSnapshots, err := limits.ReconciliationSnapshots(t.Context(), groupConnection.Conn(), time.Now())
	groupConnection.Release()
	if err != nil {
		t.Fatal(err)
	}
	for _, ownerID := range []string{group["id"].(string), groupKey["id"].(string), limits.AttributionBudgetID(project, "delegation_pair", groupPair)} {
		if !slices.ContainsFunc(groupSnapshots, func(snapshot limits.CostSnapshot) bool {
			amount, ok := new(big.Rat).SetString(snapshot.DailyAccrued)
			return snapshot.CostOwnerID == ownerID && snapshot.Basis == limits.BasisCost && ok && amount.Cmp(big.NewRat(1, 4)) == 0
		}) {
			t.Fatalf("reconciliation lost group A2A spend for %s", ownerID)
		}
	}
	for _, snapshot := range groupSnapshots {
		if _, _, applyErr := limiter.ApplyCostSnapshot(t.Context(), snapshot); applyErr != nil {
			t.Fatal(applyErr)
		}
	}
	groupPath := "/api/v1/budget-groups/" + group["id"].(string)
	groupPolicy := h.want(owner, http.MethodGet, groupPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, groupPath, map[string]any{"requests_per_minute": 100, "daily_cost_limit": "0.25"}, withMatch(groupPolicy, idem("limit-a2a-group-spend")), http.StatusOK)
	h.refresh()
	_, groupCostDenied := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + groupGrant(), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(groupCostDenied, []byte(`"error"`)) || upstreamCalls.Load() != callsBeforeGroupLimit {
		t.Fatalf("group cost limit did not stop the upstream send: %s", groupCostDenied)
	}
	rateKeyA := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A rate A", "project_id": project, "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("a2a-rate-a"), http.StatusCreated)
	rateKeyB := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A rate B", "project_id": project, "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("a2a-rate-b"), http.StatusCreated)
	rateGrant := func(key map[string]any) string {
		t.Helper()
		response, body := h.do(nil, http.MethodPost, "/v1/delegations", map[string]any{"scopes": []string{"agents"}, "allowed_agents": []string{target}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": key["secret"].(string)})
		var issued struct {
			Token string `json:"token"`
		}
		if response.StatusCode != http.StatusCreated || json.Unmarshal(body, &issued) != nil || issued.Token == "" {
			t.Fatalf("rate delegation failed: %d %s", response.StatusCode, body)
		}
		return issued.Token
	}
	rateSend := func(token string) []byte {
		t.Helper()
		_, body := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + token, "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
		return body
	}
	assertConcurrentLimit := func(dimension string, first, second map[string]any) {
		t.Helper()
		firstToken, secondToken := rateGrant(first), rateGrant(second)
		holdRemote.Store(true)
		firstResult := make(chan []byte, 1)
		go func() { firstResult <- rateSend(firstToken) }()
		select {
		case <-holdStarted:
		case <-time.After(10 * time.Second):
			holdRemote.Store(false)
			holdRelease <- struct{}{}
			t.Fatalf("%s concurrency test did not reach the remote agent", dimension)
		}
		before := upstreamCalls.Load()
		secondResult := make(chan []byte, 1)
		go func() { secondResult <- rateSend(secondToken) }()
		var denied []byte
		select {
		case denied = <-secondResult:
		case <-time.After(10 * time.Second):
			holdRemote.Store(false)
			holdRelease <- struct{}{}
			holdRelease <- struct{}{}
			t.Fatalf("%s concurrency allowed a second remote request", dimension)
		}
		after := upstreamCalls.Load()
		holdRemote.Store(false)
		holdRelease <- struct{}{}
		firstBody := <-firstResult
		if !bytes.Contains(denied, []byte("Agent send rate exceeded")) || after != before {
			t.Fatalf("%s concurrency did not refuse before dispatch: %s", dimension, denied)
		}
		if !bytes.Contains(firstBody, []byte("TASK_STATE_INPUT_REQUIRED")) {
			t.Fatalf("%s admitted request did not finish: %s", dimension, firstBody)
		}
	}
	concurrencyTemplates := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "10", "max_concurrency": 1}}}, etagHeader(concurrencyTemplates), http.StatusOK)
	h.refresh()
	assertConcurrentLimit("agent", rateKeyA, rateKeyB)
	concurrencyTemplates = h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "10"}}}, etagHeader(concurrencyTemplates), http.StatusOK)
	rateKeyAPath := "/api/v1/api-keys/" + rateKeyA["id"].(string)
	rateKeyAPolicy := h.want(owner, http.MethodGet, rateKeyAPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, rateKeyAPath, map[string]any{"max_concurrency": 1}, etagHeader(rateKeyAPolicy), http.StatusOK)
	h.refresh()
	assertConcurrentLimit("caller", rateKeyA, rateKeyA)
	concurrencyGroup := h.want(owner, http.MethodPost, "/api/v1/budget-groups", map[string]any{"name": "A2A concurrency group", "project_id": project, "max_concurrency": 1}, idem("a2a-concurrency-group"), http.StatusCreated)
	groupKeyA := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A group A", "project_id": project, "budget_group_id": concurrencyGroup["id"], "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("a2a-concurrency-a"), http.StatusCreated)
	groupKeyB := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "A2A group B", "project_id": project, "budget_group_id": concurrencyGroup["id"], "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("a2a-concurrency-b"), http.StatusCreated)
	h.refresh()
	assertConcurrentLimit("group", groupKeyA, groupKeyB)
	agentRateTemplates := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 1, "daily_cost_limit": "10"}}}, etagHeader(agentRateTemplates), http.StatusOK)
	h.refresh()
	beforeAgentRate := upstreamCalls.Load()
	if denied := rateSend(rateGrant(rateKeyB)); !bytes.Contains(denied, []byte("Agent send rate exceeded")) || upstreamCalls.Load() != beforeAgentRate {
		t.Fatalf("agent RPM did not refuse before dispatch: %s", denied)
	}
	resetAgentRate := h.want(owner, http.MethodGet, "/api/v1/projects/"+project+"/limit-templates", nil, nil, http.StatusOK)
	h.want(owner, http.MethodPut, "/api/v1/projects/"+project+"/limit-templates", map[string]any{"templates": map[string]any{"worker": map[string]any{"requests_per_minute": 100, "daily_cost_limit": "10"}}}, etagHeader(resetAgentRate), http.StatusOK)
	rateKeyBPath := "/api/v1/api-keys/" + rateKeyB["id"].(string)
	rateKeyBPolicy := h.want(owner, http.MethodGet, rateKeyBPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, rateKeyBPath, map[string]any{"requests_per_minute": 1}, etagHeader(rateKeyBPolicy), http.StatusOK)
	h.refresh()
	if accepted := rateSend(rateGrant(rateKeyB)); !bytes.Contains(accepted, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("first caller RPM request failed: %s", accepted)
	}
	beforeCallerRate := upstreamCalls.Load()
	if denied := rateSend(rateGrant(rateKeyB)); !bytes.Contains(denied, []byte("Agent send rate exceeded")) || upstreamCalls.Load() != beforeCallerRate {
		t.Fatalf("caller RPM did not refuse before dispatch: %s", denied)
	}
	skipCallback.Store(false)
	getTask := func(identity, token string) (int, []byte) {
		t.Helper()
		read := struct {
			JSONRPC string        `json:"jsonrpc"`
			ID      int           `json:"id"`
			Method  string        `json:"method"`
			Params  a2aTestParams `json:"params"`
		}{JSONRPC: "2.0", ID: 2, Method: "GetTask", Params: a2aTestParams{ID: result.Result.Task.ID}}
		readResponse, readBody := h.do(nil, http.MethodPost, "/a2a/"+target, read, map[string]string{"Authorization": "Bearer " + token, "X-OLP-Agent-Identity": identity, "A2A-Version": "1.0"})
		return readResponse.StatusCode, readBody
	}
	status, readBody := getTask(assertion, getGrant(assertion))
	if status != http.StatusOK || !bytes.Contains(readBody, []byte(result.Result.Task.ID)) {
		t.Fatalf("renewed delegation cannot read its task: %d %s", status, readBody)
	}
	otherAssertion := assertionFor("other-agent")
	otherStatus, otherBody := getTask(otherAssertion, getGrant(otherAssertion))
	if otherStatus == http.StatusOK && bytes.Contains(otherBody, []byte(result.Result.Task.ID)) {
		t.Fatalf("another agent read a task owned by the first: %s", otherBody)
	}
	otherToken := getGrant(otherAssertion)
	otherSend, otherSendBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + otherToken, "X-OLP-Agent-Identity": otherAssertion, "A2A-Version": "1.0"})
	var otherResult struct {
		Result struct {
			Task protocol.Task `json:"task"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if otherSend.StatusCode != http.StatusOK || json.Unmarshal(otherSendBody, &otherResult) != nil || len(otherResult.Error) != 0 || otherResult.Result.Task.ID == "" {
		t.Fatalf("second agent task failed: %d %s", otherSend.StatusCode, otherSendBody)
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("second agent callback refused: %d", status)
	}
	otherCapability := <-callbackCapability
	otherView := h.want(owner, http.MethodGet, "/api/v1/a2a-tasks/"+string(otherResult.Result.Task.ID), nil, nil, http.StatusOK)
	otherAgent := mcpExecString(t, otherView, "agent_principal_id")
	otherPair := mcpExecString(t, otherView, "delegation_pair_id")
	if otherView["api_key_id"] != key["id"] || otherAgent == agentID || otherPair == grant.Pair {
		t.Fatalf("tasks for two agents sharing a key were conflated: %v", otherView)
	}
	for _, private := range []string{otherToken, otherAssertion, otherCapability, messageContent} {
		if strings.Contains(fmt.Sprint(otherView), private) {
			t.Fatal("second task exposed private delegation or message content")
		}
	}
	h.want(owner, http.MethodPost, "/api/v1/a2a-tasks/"+string(otherResult.Result.Task.ID)+"/cancel", nil, nil, http.StatusOK)
	for _, method := range []string{"GetTask", "CancelTask"} {
		before := remoteRequests.Load()
		call := func(taskID string) []byte {
			t.Helper()
			_, body := h.do(nil, http.MethodPost, "/a2a/"+target, map[string]any{"jsonrpc": "2.0", "id": 99, "method": method, "params": map[string]any{"id": taskID}}, map[string]string{"Authorization": "Bearer " + getGrant(otherAssertion), "X-OLP-Agent-Identity": otherAssertion, "A2A-Version": "1.0"})
			return body
		}
		foreign := call(string(result.Result.Task.ID))
		missing := call(uuid.NewString())
		if !bytes.Equal(foreign, missing) || !bytes.Contains(foreign, []byte("Task not found")) || remoteRequests.Load() != before {
			t.Fatalf("%s distinguished or dispatched foreign and nonexistent tasks: %s %s", method, foreign, missing)
		}
	}
	continuationRequest := func(taskID protocol.TaskID) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "SendMessage", "params": a2aTestParams{Message: &protocol.Message{ID: "continuation", Role: protocol.MessageRoleUser, TaskID: taskID, Parts: protocol.ContentParts{protocol.NewTextPart("continue")}}}}
	}
	assertRevokedSends := func(principal string, newToken, continuationToken string, taskID protocol.TaskID) {
		t.Helper()
		before := remoteRequests.Load()
		for _, attempt := range []struct {
			token string
			body  any
		}{{newToken, request}, {continuationToken, continuationRequest(taskID)}} {
			call, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.HTTP.URL+"/a2a/"+target, bytes.NewReader(mustJSON(attempt.body)))
			if err != nil {
				t.Fatal(err)
			}
			call.Header.Set("Content-Type", "application/json")
			call.Header.Set("Authorization", "Bearer "+attempt.token)
			call.Header.Set("X-OLP-Agent-Identity", assertion)
			call.Header.Set("A2A-Version", "1.0")
			response, err := http.DefaultClient.Do(call)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s revocation retained send authority: %d", principal, response.StatusCode)
			}
		}
		if remoteRequests.Load() != before {
			t.Fatalf("%s revocation dispatched a new message or continuation", principal)
		}
	}
	revokedCaller := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "revoked A2A caller", "project_id": project, "scopes": []string{"agents", "delegate"}, "allowed_agents": []string{target}}, idem("revoked-a2a-caller"), http.StatusCreated)
	h.refresh()
	callerGrant := func() string {
		t.Helper()
		response, body := h.do(nil, http.MethodPost, "/v1/delegations", map[string]any{"scopes": []string{"agents"}, "allowed_agents": []string{target}}, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegating-Key": revokedCaller["secret"].(string)})
		var issued struct {
			Token string `json:"token"`
		}
		if response.StatusCode != http.StatusCreated || json.Unmarshal(body, &issued) != nil || issued.Token == "" {
			t.Fatalf("caller delegation failed: %d %s", response.StatusCode, body)
		}
		return issued.Token
	}
	skipCallback.Store(true)
	_, callerTaskBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + callerGrant(), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	skipCallback.Store(false)
	var callerTask struct {
		Result struct {
			Task protocol.Task `json:"task"`
		} `json:"result"`
	}
	if json.Unmarshal(callerTaskBody, &callerTask) != nil || callerTask.Result.Task.ID == "" {
		t.Fatalf("caller task was not created: %s", callerTaskBody)
	}
	skipCallback.Store(true)
	_, continuationBody := h.do(nil, http.MethodPost, "/a2a/"+target, continuationRequest(callerTask.Result.Task.ID), map[string]string{"Authorization": "Bearer " + callerGrant(), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	skipCallback.Store(false)
	if !bytes.Contains(continuationBody, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("caller task could not continue before revocation: %s", continuationBody)
	}
	callerNewToken, callerContinuationToken := callerGrant(), callerGrant()
	activeToken := callerGrant()
	skipCallback.Store(true)
	holdRemote.Store(true)
	activeResult := make(chan []byte, 1)
	go func() {
		_, body := h.do(nil, http.MethodPost, "/a2a/"+target, continuationRequest(callerTask.Result.Task.ID), map[string]string{"Authorization": "Bearer " + activeToken, "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
		activeResult <- body
	}()
	select {
	case <-holdStarted:
	case <-time.After(10 * time.Second):
		holdRelease <- struct{}{}
		t.Fatal("continuation did not reach the remote agent")
	}
	callerPolicy := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+revokedCaller["id"].(string), nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/api-keys/"+revokedCaller["id"].(string)+"/revoke", nil, withMatch(callerPolicy, idem("revoke-a2a-caller")), http.StatusOK)
	h.refresh()
	select {
	case body := <-activeResult:
		var response struct {
			Error *struct{} `json:"error"`
		}
		if err := json.Unmarshal(body, &response); err != nil || response.Error == nil {
			t.Fatalf("revoked continuation did not return a JSON-RPC error: %s", body)
		}
	case <-time.After(8 * time.Second):
		holdRelease <- struct{}{}
		t.Fatal("revoked continuation did not stop within the authority freshness interval")
	}
	holdRemote.Store(false)
	skipCallback.Store(false)
	var activeDebitState string
	if err := h.Pool.QueryRow(t.Context(), `SELECT d.state FROM olp.a2a_task_capabilities c JOIN olp.execution_debits d ON d.id=c.dispatch_id WHERE c.task_id=$1 ORDER BY d.created_at DESC LIMIT 1`, callerTask.Result.Task.ID).Scan(&activeDebitState); err != nil || activeDebitState != "outcome_unknown" {
		t.Fatalf("revoked in-flight continuation did not retain unknown exposure: %s %v", activeDebitState, err)
	}
	assertRevokedSends("caller", callerNewToken, callerContinuationToken, callerTask.Result.Task.ID)
	options := h.want(owner, http.MethodGet, "/api/v1/delegation-options?api_key_id="+key["id"].(string), nil, nil, http.StatusOK)
	eligibleAgent := false
	for _, item := range options["agents"].([]any) {
		option, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("invalid workload option: %v", item)
		}
		allowed, ok := option["allowed_agents"].([]any)
		if !ok {
			t.Fatalf("invalid agent options: %v", option)
		}
		if option["id"] == agentID && slices.Contains(allowed, any(target)) {
			eligibleAgent = true
			break
		}
	}
	if !eligibleAgent {
		t.Fatalf("member consent omitted an eligible A2A agent: %v", options)
	}
	consentCount := 0
	consentFor := func(keyID string) string {
		t.Helper()
		consentCount++
		source := fmt.Sprintf("192.0.2.%d", 1+(consentCount-1)/5)
		h.want(owner, http.MethodPost, "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "agent_delegation", "resource_id": keyID}, map[string]string{"X-Test-Client-IP": source}, http.StatusNoContent)
		consent := h.want(owner, http.MethodPost, "/api/v1/delegation-consents", map[string]any{"agent_id": agentID, "api_key_id": keyID, "scopes": []string{"agents", "inference"}, "allowed_agents": []string{target}, "allowed_routes": []string{"delegated-callback"}}, nil, http.StatusCreated)
		memberResponse, memberBody := h.do(nil, http.MethodPost, "/v1/delegations", bounds, map[string]string{"Authorization": "Bearer " + assertion, "X-OLP-Delegation-Consent": consent["consent"].(string)})
		if memberResponse.StatusCode != http.StatusCreated {
			t.Fatalf("member agent delegation exchange: %d %s", memberResponse.StatusCode, memberBody)
		}
		var next struct {
			Token string `json:"token"`
		}
		if decodeErr := json.Unmarshal(memberBody, &next); decodeErr != nil || next.Token == "" {
			t.Fatalf("missing member delegation: %v", decodeErr)
		}
		return next.Token
	}
	oauth := map[string]any{"client_id": "delegated-agent-client", "audience": "delegated-agent", "scopes": []string{"agent.send"}, "authorization_endpoint": oauthAuthority.URL + "/authorize", "token_endpoint": oauthAuthority.URL + "/token"}
	oauthDraft := h.want(owner, http.MethodPut, "/api/v1/agents/"+target, map[string]any{
		"project_id": project, "name": "delegated target", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": upstream.URL + "/rpc", "card_digest": hex.EncodeToString(digest[:]), "toolset_revisions": []string{}, "route_revisions": []string{routeRevision}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "remote_work_bound_seconds": 120, "oauth": oauth},
	}, withMatch(published, idem("delegated-oauth-draft")), http.StatusOK)
	oauthPublished := h.want(owner, http.MethodPost, "/api/v1/agents/"+target+"/publish", nil, withMatch(oauthDraft, idem("delegated-oauth-publish")), http.StatusOK)
	h.refresh()
	oauthAuthority.SignInAs(testutil.OAuthIdentity{Subject: "original-user", Account: "original-account"})
	handoffResponse := h.want(owner, http.MethodPost, "/api/v1/agents/"+target+"/grant-enrollments", map[string]any{"api_key_id": key["id"]}, nil, http.StatusOK)
	var handoff mcpgrants.Handoff
	if err := json.Unmarshal(mustJSON(handoffResponse), &handoff); err != nil {
		t.Fatal(err)
	}
	_, fragment, _ := strings.Cut(handoff.EnrollmentURL, "#")
	enrollmentID, handoffToken, _ := strings.Cut(fragment, ".")
	h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/claim", mcpPersonalClaimInput{EnrollmentID: enrollmentID, HandoffToken: handoffToken}, nil, http.StatusOK)
	authorizationResponse := h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/"+enrollmentID+"/authorize", personalOAuthAuthorizeInput{Flow: mcpgrants.AuthorizationCode}, nil, http.StatusOK)
	var authorization mcpgrants.Authorization
	if err := json.Unmarshal(mustJSON(authorizationResponse), &authorization); err != nil {
		t.Fatal(err)
	}
	redirectClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	redirectResponse, err := redirectClient.Get(authorization.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	redirectResponse.Body.Close()
	callbackURL, err := url.Parse(redirectResponse.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	h.want(owner, http.MethodPost, "/api/v1/mcp-grant-enrollments/"+enrollmentID+"/complete", personalOAuthCompletionInput{Code: callbackURL.Query().Get("code"), State: callbackURL.Query().Get("state")}, nil, http.StatusOK)
	requireOAuth.Store(true)
	memberResponse, memberBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + consentFor(key["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if memberResponse.StatusCode != http.StatusOK {
		t.Fatalf("member delegated send: %d %s", memberResponse.StatusCode, memberBody)
	}
	var memberResult struct {
		Result struct {
			Task protocol.Task `json:"task"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(memberBody, &memberResult); err != nil || len(memberResult.Error) != 0 || memberResult.Result.Task.ID == "" {
		t.Fatalf("member delegated task missing: %s %v", memberBody, err)
	}
	if memberResult.Result.Task.ID == result.Result.Task.ID {
		t.Fatal("member and key delegations shared a task")
	}
	memberTaskView := h.want(owner, http.MethodGet, "/api/v1/a2a-tasks/"+string(memberResult.Result.Task.ID), nil, nil, http.StatusOK)
	ownerProfile := h.want(owner, http.MethodGet, "/api/v1/profile", nil, nil, http.StatusOK)
	memberPair := mcpExecString(t, memberTaskView, "delegation_pair_id")
	if memberTaskView["agent_principal_id"] != agentID || memberTaskView["member_principal_id"] != ownerProfile["id"] || memberPair == grant.Pair {
		t.Fatalf("member task lost its initiating pair: %v", memberTaskView)
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("member delegated task callback refused: %d", status)
	}
	<-callbackCapability
	secondMemberResponse, secondMemberBody := h.do(nil, http.MethodPost, "/a2a/"+target, request, map[string]string{"Authorization": "Bearer " + consentFor(key["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	var secondMemberResult struct {
		Result struct {
			Task protocol.Task `json:"task"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if secondMemberResponse.StatusCode != http.StatusOK || json.Unmarshal(secondMemberBody, &secondMemberResult) != nil || len(secondMemberResult.Error) != 0 || secondMemberResult.Result.Task.ID == "" {
		t.Fatalf("second member task failed: %d %s", secondMemberResponse.StatusCode, secondMemberBody)
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("second member callback refused: %d", status)
	}
	<-callbackCapability
	replacementAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent-card.json" || r.Header.Get("Authorization") != "" {
			replacementOAuthCalls.Add(1)
		}
		if r.URL.Path == "/.well-known/agent-card.json" {
			_, _ = w.Write(cardFor("http://" + r.Host + "/rpc"))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(replacementAgent.Close)
	replacementDigest := sha256.Sum256(cardFor(replacementAgent.URL + "/rpc"))
	newEndpoint := h.want(owner, http.MethodPut, "/api/v1/agents/"+target, map[string]any{
		"project_id": project, "name": "delegated target", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacementAgent.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{routeRevision}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "remote_work_bound_seconds": 120},
	}, withMatch(oauthPublished, idem("delegated-endpoint-draft")), http.StatusOK)
	newEndpointPublished := h.want(owner, http.MethodPost, "/api/v1/agents/"+target+"/publish", nil, withMatch(newEndpoint, idem("delegated-endpoint-publish")), http.StatusOK)
	newOAuth := h.want(owner, http.MethodPut, "/api/v1/agents/"+target, map[string]any{
		"project_id": project, "name": "delegated target", "credential": nil,
		"agent": map[string]any{"protocol": "a2a-1.0.0", "endpoint": replacementAgent.URL + "/rpc", "card_digest": hex.EncodeToString(replacementDigest[:]), "toolset_revisions": []string{}, "route_revisions": []string{routeRevision}, "pricing_revision_id": pricing, "max_cost": "1", "message_price": "0.25", "remote_work_bound_seconds": 120, "oauth": oauth},
	}, withMatch(newEndpointPublished, idem("delegated-endpoint-oauth")), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/agents/"+target+"/publish", nil, withMatch(newOAuth, idem("delegated-endpoint-oauth-publish")), http.StatusOK)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.mcp_user_grants SET expires_at=now()-interval '1 second' WHERE agent_id=$1 AND binding->'identity'->>'id'=$2`, target, key["id"]); err != nil {
		t.Fatal(err)
	}
	replacement := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "replacement A2A key", "project_id": project, "scopes": []string{"agents", "inference", "delegate"}, "allowed_agents": []string{target}, "allowed_routes": []string{"delegated-callback"}}, idem("replacement-a2a-key"), http.StatusCreated)
	h.refresh()
	callbackAccounting := &captureSink{}
	h.Gateway.Sink = callbackAccounting
	_, rotatedContinuation := h.do(nil, http.MethodPost, "/a2a/"+target, continuationRequest(memberResult.Result.Task.ID), map[string]string{"Authorization": "Bearer " + consentFor(replacement["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(rotatedContinuation, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("replacement key could not continue its member's task: %s", rotatedContinuation)
	}
	if status := <-callbackStatus; status != http.StatusOK {
		t.Fatalf("rotated task callback refused: %d", status)
	}
	rotatedCapability := <-callbackCapability
	callbackEvent := callbackAccounting.last()
	var callbackRecords int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.a2a_task_callback_accounting WHERE kind='inference' AND accounting_id=$1`, callbackEvent.AccountingID).Scan(&callbackRecords); err != nil || callbackRecords != 1 || callbackEvent.KeyID != key["id"] || callbackEvent.AgentPrincipalID != agentID || callbackEvent.DelegationPairDigest != memberPair {
		t.Fatalf("rotated callback lost original accounting owner or was counted more than once: %+v records=%d err=%v", callbackEvent, callbackRecords, err)
	}
	currentOriginal := h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, keyPath, map[string]any{"requests_per_minute": 1}, etagHeader(currentOriginal), http.StatusOK)
	h.refresh()
	callbackInput := map[string]any{"model": "delegated-callback", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}
	firstCallback, firstCallbackBody := h.do(nil, http.MethodPost, "/v1/chat/completions", callbackInput, map[string]string{"Authorization": "Bearer " + rotatedCapability})
	if firstCallback.StatusCode != http.StatusOK {
		t.Fatalf("original owner callback did not use its remaining request allowance: %d %s", firstCallback.StatusCode, firstCallbackBody)
	}
	modelCalls := modelProvider.chatCalls.Load()
	_, limitedContinuation := h.do(nil, http.MethodPost, "/a2a/"+target, continuationRequest(memberResult.Result.Task.ID), map[string]string{"Authorization": "Bearer " + consentFor(replacement["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(limitedContinuation, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("replacement key could not continue after original owner reached its callback limit: %s", limitedContinuation)
	}
	if status := <-callbackStatus; status != http.StatusTooManyRequests || modelProvider.chatCalls.Load() != modelCalls {
		t.Fatalf("rotated callback bypassed original owner budget: status=%d model calls=%d", status, modelProvider.chatCalls.Load()-modelCalls)
	}
	<-callbackCapability
	currentOriginal = h.want(owner, http.MethodGet, keyPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPatch, keyPath, map[string]any{"requests_per_minute": nil}, etagHeader(currentOriginal), http.StatusOK)
	h.refresh()
	memberRead := struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  a2aTestParams `json:"params"`
	}{JSONRPC: "2.0", ID: 3, Method: "GetTask", Params: a2aTestParams{ID: memberResult.Result.Task.ID}}
	memberReadResponse, memberReadBody := h.do(nil, http.MethodPost, "/a2a/"+target, memberRead, map[string]string{"Authorization": "Bearer " + consentFor(replacement["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if memberReadResponse.StatusCode != http.StatusOK || !bytes.Contains(memberReadBody, []byte(memberResult.Result.Task.ID)) {
		t.Fatalf("member delegation cannot read its task after key replacement: %d %s", memberReadResponse.StatusCode, memberReadBody)
	}
	_, memberSubscription := h.do(nil, http.MethodPost, "/a2a/"+target, map[string]any{"jsonrpc": "2.0", "id": 4, "method": "SubscribeToTask", "params": map[string]any{"id": memberResult.Result.Task.ID}}, map[string]string{"Authorization": "Bearer " + consentFor(replacement["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(memberSubscription, []byte("TASK_STATE_INPUT_REQUIRED")) {
		t.Fatalf("replacement key could not subscribe to its member's task: %s", memberSubscription)
	}
	_, memberCanceled := h.do(nil, http.MethodPost, "/a2a/"+target, map[string]any{"jsonrpc": "2.0", "id": 5, "method": "CancelTask", "params": map[string]any{"id": memberResult.Result.Task.ID}}, map[string]string{"Authorization": "Bearer " + consentFor(replacement["id"].(string)), "X-OLP-Agent-Identity": assertion, "A2A-Version": "1.0"})
	if !bytes.Contains(memberCanceled, []byte("TASK_STATE_CANCELED")) || originalOAuthCalls.Load() != 7 || replacementOAuthCalls.Load() != 0 || len(oauthAuthority.Issued()) < 2 {
		t.Fatalf("replacement key did not use the original rotated grant and endpoint: %s", memberCanceled)
	}
	memberTaskPath := "/api/v1/a2a-tasks/" + string(secondMemberResult.Result.Task.ID)
	canceled := h.want(manager, http.MethodPost, memberTaskPath+"/cancel", nil, nil, http.StatusOK)
	if canceled["id"] != string(secondMemberResult.Result.Task.ID) || canceled["contextId"] != string(secondMemberResult.Result.Task.ID) || canceled["status"].(map[string]any)["state"] != string(protocol.TaskStateCanceled) || originalOAuthCalls.Load() != 8 || replacementOAuthCalls.Load() != 0 {
		t.Fatalf("project operator could not cancel delegated task: %v", canceled)
	}
	var stored string
	if err := h.Pool.QueryRow(t.Context(), `SELECT COALESCE(string_agg(value,E'\n'),'') FROM (
		SELECT row_to_json(t)::text value FROM olp.a2a_tasks t
		UNION ALL SELECT row_to_json(t)::text FROM olp.a2a_task_admissions t
		UNION ALL SELECT row_to_json(t)::text FROM olp.a2a_task_capabilities t
		UNION ALL SELECT row_to_json(t)::text FROM olp.execution_debits t
		UNION ALL SELECT row_to_json(t)::text FROM olp.agent_delegations t
		UNION ALL SELECT row_to_json(t)::text FROM olp.mcp_user_grants t
	) records`).Scan(&stored); err != nil || stored == "" {
		t.Fatalf("cannot inspect durable task records: %v", err)
	}
	export := h.want(owner, http.MethodGet, "/api/v1/configuration/export", nil, nil, http.StatusOK)
	exported, err := json.Marshal(export)
	if err != nil || len(exported) == 0 {
		t.Fatal("configuration export is missing")
	}
	for name, secret := range map[string]string{"delegation grant": grant.Token, "workload assertion": assertion, "task capability": capability, "message content": messageContent} {
		control, err := json.Marshal(map[string]string{"value": secret})
		if secret == "" || err != nil || !strings.Contains(string(control), secret) {
			t.Fatalf("%s was not detected in the positive control", name)
		}
		if strings.Contains(stored, secret) || strings.Contains(string(exported), secret) {
			t.Fatalf("%s appeared in durable task records or export", name)
		}
	}
	agentPath := "/api/v1/api-keys/" + agentID
	agentNewToken, agentContinuationToken := getGrant(assertion), getGrant(assertion)
	agentKey := h.want(owner, http.MethodGet, agentPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, agentPath+"/revoke", nil, withMatch(agentKey, idem("revoke-delegated-a2a-agent")), http.StatusOK)
	h.refresh()
	assertRevokedSends("agent", agentNewToken, agentContinuationToken, result.Result.Task.ID)
	callbackResponse, callbackBody := h.do(nil, http.MethodPost, "/v1/chat/completions", map[string]any{"model": "delegated-callback", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, map[string]string{"Authorization": "Bearer " + capability})
	if callbackResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked agent retained callback authority: %d %s", callbackResponse.StatusCode, callbackBody)
	}
	requireOAuth.Store(false)
	watchCancellation.Store(true)
	cardUnavailable.Store(true)
	monitorContext, stopMonitor := context.WithCancel(t.Context())
	defer stopMonitor()
	go (&a2a.TaskMonitor{Server: h.A2A}).Run(monitorContext)
	select {
	case method := <-cancellation:
		if method != "CancelTask" {
			t.Fatalf("revocation sent %q instead of cancellation", method)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("revoked agent task was not canceled while its card was unavailable")
	}
}
