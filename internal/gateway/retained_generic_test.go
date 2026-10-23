package gateway

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestRetainedUpstreamPath(t *testing.T) {
	byPattern := map[string]retainedapi.Endpoint{}
	for _, e := range retainedapi.Endpoints() {
		byPattern[e.Method+" "+e.Pattern] = e
	}
	cases := []struct {
		key  string
		ids  map[string]string
		want string
	}{
		{"POST /v1/fine_tuning/jobs", nil, "fine_tuning/jobs"},
		{"GET /v1/fine_tuning/jobs/{id}", map[string]string{"id": "ft_1"}, "fine_tuning/jobs/ft_1"},
		{"POST /v1/fine_tuning/jobs/{id}/cancel", map[string]string{"id": "ft_1"}, "fine_tuning/jobs/ft_1/cancel"},
		{"POST /v1/vector_stores/{parent0}/files", map[string]string{"parent0": "vs_1"}, "vector_stores/vs_1/files"},
		{"GET /v1/vector_stores/{parent0}/file_batches/{id}", map[string]string{"parent0": "vs_1", "id": "b_1"}, "vector_stores/vs_1/file_batches/b_1"},
		{"POST /anthropic/v1/skills/{parent0}/versions", map[string]string{"parent0": "sk_1"}, "skills/sk_1/versions"},
		{"POST /gemini/v1beta/cachedContents", nil, "cachedContents"},
		{"GET /gemini/v1beta/cachedContents/{id}", map[string]string{"id": "cachedContents/c_1"}, "cachedContents/c_1"},
		{"POST /native/vertex-tuning/jobs", nil, "tuningJobs"},
		{"GET /native/vertex-tuning/jobs/{id}", map[string]string{"id": "projects/p/locations/l/tuningJobs/j"}, "tuningJobs/j"},
		{"POST /native/vertex-tuning/jobs/{id}/cancel", map[string]string{"id": "projects/p/locations/l/tuningJobs/j"}, "tuningJobs/j:cancel"},
	}
	for _, tc := range cases {
		e, ok := byPattern[tc.key]
		if !ok {
			t.Fatalf("endpoint not registered: %s", tc.key)
		}
		if got := retainedUpstreamPath(e, tc.ids); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.key, got, tc.want)
		}
	}
}

func TestRetainedBodyContracts(t *testing.T) {
	s := &Server{cfg: Config{MaxBodyBytes: 4096}}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "skill"}, Action: "create", Method: http.MethodPost}
	build := func(boundary, payload string) ([]byte, string) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.SetBoundary(boundary); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("files[]", "SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(part, payload)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return body.Bytes(), writer.FormDataContentType()
	}
	body, contentType := build("first-boundary", "skill content")
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	decoded, _, forwardedType, failure := s.readRetainedBody(req, e)
	if failure != nil || !bytes.Equal(decoded, body) || forwardedType != contentType {
		t.Fatalf("multipart changed: %s %v", decoded, failure)
	}
	e.Action = "cancel"
	_, _, _, failure = s.readRetainedBody(httptest.NewRequest(http.MethodPost, "/", nil), e)
	if failure != nil {
		t.Fatalf("bodyless cancellation: %v", failure)
	}
	s.Runtime = retainedDigestRuntime{key: secrets.NewAuthKey(bytes.Repeat([]byte{1}, 32), "installation")}
	e.Action = "create"
	digest, failure := s.retainedBodyDigest(e, "route", body, contentType)
	if failure != nil {
		t.Fatal(failure)
	}
	other, otherType := build("second-boundary", "skill content")
	retry, failure := s.retainedBodyDigest(e, "route", other, otherType)
	if failure != nil || !bytes.Equal(digest, retry) {
		t.Fatal("multipart boundary changed submission identity")
	}
	other, otherType = build("second-boundary", "different content")
	retry, failure = s.retainedBodyDigest(e, "route", other, otherType)
	if failure != nil || bytes.Equal(digest, retry) {
		t.Fatal("different upload retained the submission identity")
	}
}

func TestRetainedCreateIdentitiesAndProjection(t *testing.T) {
	for _, tc := range []struct {
		family retainedapi.Family
		body   string
		ids    []string
	}{
		{retainedapi.Family{Kind: "context_cache", Identity: "name"}, `{"name":"cachedContents/cache-1"}`, []string{"cachedContents/cache-1"}},
		{retainedapi.Family{Kind: "conversation_item", Identity: "id", Collection: "data"}, `{"data":[{"id":"item-1"},{"id":"item-2"}]}`, []string{"item-1", "item-2"}},
	} {
		ids, err := retainedCreateIDs(retainedapi.Endpoint{Family: tc.family, Action: "create"}, []byte(tc.body))
		if err != nil || !reflect.DeepEqual(ids, tc.ids) {
			t.Fatalf("identities=%v error=%v", ids, err)
		}
	}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "fine_tuning", Kind: "fine_tuning_job", Identity: "id"}, Action: "get"}
	r := &resources.Resource{Kind: e.Kind, ID: "fine_tuning_job_local", UpstreamID: "ft-native"}
	s := &Server{}
	body, err := s.projectRetainedGeneric(t.Context(), r, e, []byte(`{"id":"ft-native","organization_id":"org-native","status":"succeeded"}`))
	if err != nil || !bytes.Contains(body, []byte(`"organization_id":"org-native"`)) || !bytes.Contains(body, []byte(r.ID)) {
		t.Fatalf("projected=%s error=%v", body, err)
	}
	body, err = s.projectRetainedGeneric(t.Context(), r, e, []byte(`{"id":"ft-native","deleted":true,"object":"fine_tuning.job.deleted"}`))
	if err != nil || !bytes.Contains(body, []byte(`"deleted":true`)) || !bytes.Contains(body, []byte(r.ID)) {
		t.Fatalf("deletion=%s error=%v", body, err)
	}
	for _, invalid := range []string{`not json`, `{"id":"ft-native","foreign_id":"native-secret"}`} {
		if _, err := s.projectRetainedGeneric(t.Context(), r, e, []byte(invalid)); err == nil {
			t.Fatalf("unprojected response accepted: %s", invalid)
		}
	}
	search := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "retrieval"}}
	if _, err := s.projectRetainedGeneric(t.Context(), nil, search, []byte(`{"data":[{"file_id":"foreign-native"}]}`)); err == nil {
		t.Fatal("search leaked an unbound file identity")
	}
}

func TestRetainedModelAndReferenceCodecs(t *testing.T) {
	for _, tc := range []struct{ operation, kind, field string }{
		{"fine_tuning", "fine_tuning_job", "model"}, {"compaction", "", "model"}, {"fine_tuning", "vertex_tuning_job", "baseModel"},
	} {
		source, _ := oif.ParseJSON([]byte(`{"`+tc.field+`":"unauthorized-model"}`), oif.Limits{})
		bound, err := bindRetainedModel(source, retainedapi.Endpoint{Family: retainedapi.Family{Operation: tc.operation, Kind: tc.kind}, Action: "create"}, &pin{model: "allowed-model"})
		if err != nil || !bytes.Contains(bound.Bytes(), []byte(`"allowed-model"`)) || bytes.Contains(bound.Bytes(), []byte("unauthorized")) {
			t.Fatalf("model binding: %s %v", bound.Bytes(), err)
		}
	}
	for _, op := range []string{"container", "compaction"} {
		refs := retainedRequestRefs(retainedapi.Endpoint{Family: retainedapi.Family{Operation: op}})
		if refs["file_id"] != resources.KindFile {
			t.Fatalf("%s missing attachment reference", op)
		}
		if op == "compaction" && refs["previous_response_id"] != resources.KindResponse {
			t.Fatal("compaction missing continuation reference")
		}
	}
}

func TestRetainedAccountingPolicy(t *testing.T) {
	now := time.Now()
	project := "project"
	price := usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: "model", Operation: "generation"}, EffectiveAt: now.Add(-time.Minute)}
	s := &Server{Runtime: &fakeRuntime{inputs: &usage.RoutingInputs{RefreshedAt: now, Prices: []usage.RoutingPrice{price}, RateCards: []usage.RoutingRateCard{{ProjectID: &project, Revisions: []usage.RoutingCardRevision{{ID: "card", EffectiveAt: now.Add(-time.Minute)}}}}}}}
	x := &call{operationOverride: "generation", request: request{startedAt: now}}
	p := &pin{provider: routing.Provider{Kind: "openai"}, model: "model"}
	route := &routing.Route{ProjectID: &project}
	if err := s.retainedAccountingPolicy(x, p, route, false); err != nil || p.attempt.Price == nil {
		t.Fatalf("priced immediate operation rejected: %v", err)
	}
	if err := s.retainedAccountingPolicy(x, p, route, true); err == nil || err.Code != "unsupported_metered_operation" {
		t.Fatalf("unowned deferred usage dispatched: %v", err)
	}
}

func TestRetainedCompactionDispatch(t *testing.T) {
	h := newHarness(t, Config{})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	h.rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now()}
	for id, provider := range h.rt.release.Snapshot.Providers {
		provider.Kind = "openai"
		h.rt.release.Snapshot.Providers[id] = provider
	}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/a/v1/responses/compact" || !bytes.Contains(body, []byte(`"model":"`+modelA+`"`)) || bytes.Contains(body, []byte("unauthorized")) {
			t.Errorf("upstream request=%s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"cmp_1","object":"response.compaction","output":[{"id":"msg_1","type":"message"}],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`)
	})
	resp := probeRetainedCompact(h, `{"model":"unauthorized-model","input":"hello"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("compaction: %d %s", resp.Code, resp.Body.String())
	}
	envelope := h.sink.await(t, 1)[0]
	if len(envelope.Attempts) != 1 || envelope.Attempts[0].Usage == nil || envelope.Attempts[0].Usage.TotalTokens != 12 {
		t.Fatalf("usage lost: %+v", envelope)
	}
}

func TestRetainedCompactionCurrentGuardrails(t *testing.T) {
	h := newHarness(t, Config{})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	installTestGuardrail(t, h, guardrails.Output, guardrails.Block, "builtin.keywords", `{"words":["secret"]}`)
	resp := probeRetainedCompact(h, `{"model":"m","input":"hello"}`)
	if resp.Code < 400 || !strings.Contains(resp.Body.String(), "guardrail_surface_unavailable") || h.mock.count("a") != 0 {
		t.Fatalf("guardrail bypass: %d %s", resp.Code, resp.Body.String())
	}
}

func TestRetainedCompactionRejectsUnboundReferences(t *testing.T) {
	h := newHarness(t, Config{})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	for id, provider := range h.rt.release.Snapshot.Providers {
		provider.Kind = "openai"
		h.rt.release.Snapshot.Providers[id] = provider
	}
	for _, body := range []string{
		`{"model":"m","previous_response_id":"resp_native","input":"hello"}`,
		`{"model":"m","input":[{"type":"input_file","file_id":"file_native"}]}`,
	} {
		resp := probeRetainedCompact(h, body)
		if resp.Code != 400 || !strings.Contains(resp.Body.String(), "invalid_resource_reference") || h.mock.count("a") != 0 {
			t.Fatalf("unbound reference dispatched: %d %s", resp.Code, resp.Body.String())
		}
	}
}

func TestRetainedDispatchReadDeadline(t *testing.T) {
	h := newHarness(t, Config{})
	route := h.rt.release.Snapshot.Routes[routeSlug]
	target := route.Targets[0]
	provider := h.rt.release.Snapshot.Providers[target.ProviderID]
	p := &pin{provider: provider, slot: provider.Slots[0], target: target, model: target.ProviderModel,
		attempt: routing.Attempt{ProviderID: provider.ID, TargetID: target.ID, UpstreamModel: target.ProviderModel}}
	x := &call{request: request{release: h.rt.release, startedAt: time.Now()}, mode: "unary", routeLeg: routeLeg{route: &route}}
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	var failure *Error
	started := time.Now()
	code := h.gateway.retainedDispatch(w, req, x, p, retainedapi.Endpoint{Action: "content", Method: http.MethodGet}, nil, provider.Endpoint+"/slow", nil, "", false, func(err *Error) { failure = err })
	if code != 502 || failure == nil || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("deadline not enforced: code=%d failure=%v duration=%s", code, failure, time.Since(started))
	}
}

func TestRetainedRequestRefs(t *testing.T) {
	refs := retainedRequestRefs(retainedapi.Endpoint{Family: retainedapi.Family{Operation: "fine_tuning"}})
	if refs["training_file"] == "" || refs["validation_file"] == "" {
		t.Fatalf("fine-tuning file refs missing: %v", refs)
	}
	if refs := retainedRequestRefs(retainedapi.Endpoint{Family: retainedapi.Family{Operation: "skill"}}); len(refs) != 0 {
		t.Fatalf("skill refs should be empty: %v", refs)
	}
}

func TestRetainedCompactionRejectsItemReferences(t *testing.T) {
	h := newHarness(t, Config{})
	authority := h.rt.keys[fullKey]
	authority.Policy.AllowProviderState = true
	h.rt.keys[fullKey] = authority
	for id, provider := range h.rt.release.Snapshot.Providers {
		provider.Kind = "openai"
		h.rt.release.Snapshot.Providers[id] = provider
	}
	for _, body := range []string{
		`{"model":"m","input":[{"type":"item_reference","id":"msg_native"}]}`,
		`{"model":"m","input":[{"id":"msg_native"}]}`,
		`{"model":"m","input":[{"type":null,"id":"msg_native"}]}`,
	} {
		resp := probeRetainedCompact(h, body)
		if resp.Code != 400 || !strings.Contains(resp.Body.String(), "invalid_resource_reference") || h.mock.count("a") != 0 {
			t.Fatalf("item_reference dispatched: %d %s", resp.Code, resp.Body.String())
		}
	}
	if !samePin(&resources.Resource{ProviderID: "p", TargetID: "t", SlotID: "s"}, &resources.Resource{ProviderID: "p", TargetID: "t", SlotID: "s"}) ||
		samePin(&resources.Resource{ProviderID: "p", TargetID: "t", SlotID: "s"}, &resources.Resource{ProviderID: "p", TargetID: "t", SlotID: "other"}) {
		t.Fatal("pin agreement check broken")
	}
}

func TestRetainedSkillContracts(t *testing.T) {
	// The pinned SDK identifies a Skill Version by id, and every Skill
	// response carries its latest version as a child reference.
	var versionEndpoint retainedapi.Endpoint
	for _, e := range retainedapi.Endpoints() {
		if e.Kind == "skill_version" && e.Action == "create" {
			versionEndpoint = e
		}
	}
	if versionEndpoint.Identity != "id" {
		t.Fatalf("skill_version identity=%q", versionEndpoint.Identity)
	}
	ids, err := retainedCreateIDs(versionEndpoint, []byte(`{"id":"ver_native","skill_id":"skill_native"}`))
	if err != nil || len(ids) != 1 || ids[0] != "ver_native" {
		t.Fatalf("skill version identities=%v error=%v", ids, err)
	}
	skillGet := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "skill", Kind: "skill", Identity: "id"}, Action: "get"}
	source, _ := oif.ParseJSON([]byte(`{"id":"skill_native","latest_version_id":"ver_native"}`), oif.Limits{})
	if refs := retainedResponseRefs(skillGet, source); refs["latest_version_id"] != "skill_version" {
		t.Fatalf("latest version not a child reference: %v", refs)
	}
	// Only the Skills beta flag and opaque page token reach the provider.
	s := &Server{}
	query, queryErr := s.retainedQuery(t.Context(), &call{}, retainedapi.Endpoint{Family: retainedapi.Family{Operation: "skill", Kind: "skill"}, Action: "list"}, nil, url.Values{"beta": {"true"}, "page": {"tok-1"}, "limit": {"10"}})
	if queryErr != nil || query.Get("beta") != "true" || query.Get("page") != "tok-1" {
		t.Fatalf("skills query rejected: %v %v", query, queryErr)
	}
	if _, queryErr := s.retainedQuery(t.Context(), &call{}, retainedapi.Endpoint{Family: retainedapi.Family{Operation: "vector_store", Kind: "vector_store"}, Action: "list"}, nil, url.Values{"beta": {"true"}}); queryErr == nil {
		t.Fatal("beta flag admitted outside skills")
	}
}

func TestRetainedConversationItemDeleteIdentity(t *testing.T) {
	e := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "conversation", Kind: "conversation_item", Identity: "id"}, Action: "delete"}
	source, _ := oif.ParseJSON([]byte(`{"id":"conv_native","object":"conversation"}`), oif.Limits{})
	if refs := retainedResponseRefs(e, source); refs["/id"] != "conversation" {
		t.Fatalf("delete root identity=%v", refs)
	}
}

func probeRetainedCompact(h *harness, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(stateRouteHeader, routeSlug)
	response := httptest.NewRecorder()
	h.gateway.retainedCompact(response, req)
	return response
}

func TestRetainedPerRequestBoundAndFileIDs(t *testing.T) {
	x := &call{operationOverride: "vector_store", request: request{startedAt: time.Now()}}
	price := &usage.RoutingPrice{Price: usage.Price{ProviderKind: "openai", Model: modelA, Operation: "vector_store", PerRequest: new("0.01")}}
	bound, ok := x.targetCostBound(routing.Provider{}, routing.Target{}, price, nil)
	if !ok || bound.String() != "0.01" {
		t.Fatalf("retained fixed-fee bound: %s ok=%v", bound.String(), ok)
	}
	source, _ := oif.ParseJSON([]byte(`{"input":[{"type":"input_file","file_id":"file_1"},{"type":"input_file","file_id":"file_1"},{"type":"input_file","file_id":"file_2"}]}`), oif.Limits{})
	if ids := retainedFileIDs(source.Root()); len(ids) != 2 || ids[0] != "file_1" || ids[1] != "file_2" {
		t.Fatalf("file ids=%v", ids)
	}
}
