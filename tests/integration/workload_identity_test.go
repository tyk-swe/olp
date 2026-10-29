//go:build integration && oidctest

package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	agentworkflows "github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
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
	h.want(owner, "DELETE", "/api/v1/projects/"+project+"/members/"+memberID, nil, projectEtag(h, owner, project), http.StatusNoContent)
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
	upstreamCalls := make(chan struct{}, 8)
	signedExpiries := make(chan time.Time, 8)
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
		signedExpiries <- claims.ExpiresAt.Time
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
		"mappings": []any{map[string]any{"name": "agent", "match": map[string]string{"/sub": "approved-agent", "/client_id": "approved-client"}, "project_id": project, "limit_template": "worker", "route_groups": []string{}, "allowed_toolsets": []string{f.toolsetID}, "scopes": []string{"tools"}, "agent_principal": true}},
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
	if signedExpiry := <-signedExpiries; signedExpiry.After(grant.ExpiresAt) {
		t.Fatalf("downstream signature outlived the delegation: %s > %s", signedExpiry, grant.ExpiresAt)
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
		Token string `json:"token"`
	}
	if err := json.Unmarshal(memberBody, &memberGrant); err != nil || memberGrant.Token == "" {
		t.Fatalf("member tool delegation missing token: %v", err)
	}
	memberCall := httptest.NewRequest(http.MethodPost, "/v1/mcp/tools/call", strings.NewReader(string(mustJSON(struct {
		ToolsetID string   `json:"toolset_id"`
		Name      string   `json:"name"`
		Arguments struct{} `json:"arguments"`
		CallID    string   `json:"call_id"`
	}{ToolsetID: f.toolsetID, Name: f.alias, CallID: "member-tool"}))))
	memberCall.Header.Set("Authorization", "Bearer "+memberGrant.Token)
	memberCall.Header.Set("X-OLP-Agent-Identity", assertion)
	memberCall.Header.Set("Content-Type", "application/json")
	memberResult := httptest.NewRecorder()
	handler.ServeHTTP(memberResult, memberCall)
	if memberResult.Code != http.StatusOK {
		t.Fatalf("member tool delegation failed: %d %s", memberResult.Code, memberResult.Body.String())
	}
	<-upstreamCalls
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
	sessionRPC := func(token, session, method string, id *int, params json.RawMessage) *httptest.ResponseRecorder {
		rpc := httptest.NewRequest(http.MethodPost, "/mcp?toolset_id="+f.toolsetID, bytes.NewReader(mustJSON(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id,omitempty"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params,omitempty"`
		}{JSONRPC: "2.0", ID: id, Method: method, Params: params})))
		rpc.Header.Set("Authorization", "Bearer "+token)
		rpc.Header.Set("X-OLP-Agent-Identity", assertion)
		rpc.Header.Set("Content-Type", "application/json")
		rpc.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			rpc.Header.Set("Mcp-Session-Id", session)
		}
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, rpc)
		return out
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
	templatesPath := "/api/v1/projects/" + project + "/limit-templates"
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
}
