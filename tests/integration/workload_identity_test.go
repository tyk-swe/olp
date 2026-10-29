//go:build integration && oidctest

package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	agentworkflows "github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"

	"github.com/coder/websocket"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
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
		t.Fatalf("delegation grant missing: %s %v", body, err)
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
	h.refresh()
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
		}
		if err := json.Unmarshal(raw, &grant); err != nil || grant.Token == "" {
			t.Fatalf("delegation token: %s %v", raw, err)
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
