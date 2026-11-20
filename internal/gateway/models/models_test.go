package models

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

const readSecret = "olp_read_key"

type plainKeys struct {
	bySecret map[string]keyauthority.Authority
}

func (k plainKeys) Authenticate(secret string) (*keyauthority.Authority, error) {
	authority, ok := k.bySecret[secret]
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return &authority, nil
}

func testHandler(t *testing.T) *Handler {
	t.Helper()
	routeID := uuid.NewString()
	credential := uuid.NewString()
	version := 1
	provider := routing.Provider{
		ID: uuid.NewString(), Name: "a", Kind: "openai_compatible", Enabled: true,
		ActiveCredential: &credential, RevisionID: uuid.NewString(),
		Capabilities: []routing.Capability{{Model: "model-a", Operation: "generation", Surface: "openai", Mode: "unary"}},
		Endpoint:     "http://127.0.0.1/a/v1", AuthMode: "api_key",
		Slots: []routing.Slot{{ID: uuid.NewString(), Name: "default", Enabled: true, Weight: 1, CredentialID: &credential, CredentialVersion: &version}},
	}
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]routing.Provider{provider.ID: provider},
		Routes: map[string]routing.Route{"team-chat": {
			ID: routeID, Slug: "team-chat", Operations: []string{"generation"}, OverallTimeout: 5000,
			MaxAttempts: 1, RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1,
			PublishedAt: time.Now(), Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets: []routing.Target{{ID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: "model-a", Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}},
		}},
	}
	rel, err := release.NewRelease(uuid.NewString(), 1, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := plainKeys{bySecret: map[string]keyauthority.Authority{
		readSecret: {ID: uuid.NewString(), Policy: keyauthority.KeyPolicy{Scopes: []string{"models_read"}}},
	}}
	gate := ingress.NewGate(keys, admission.NewPool(1), nil, 0, time.Now)
	return New(Dependencies{
		Gate:  gate,
		Begin: func(http.ResponseWriter, *http.Request) *release.Release { return rel },
		Now:   time.Now,
		Workflow: func(context.Context, *release.Release, keyauthority.Authority, routing.Route, string) *routing.WorkflowCapability {
			return nil
		},
	})
}

func readRequest(path string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+readSecret)
	return r
}

func TestListReturnsPublishedModels(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	testHandler(t).List(recorder, readRequest("/v1/models"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "list" || len(got.Data) != 1 || got.Data[0].ID != "team-chat" {
		t.Fatalf("list = %+v", got)
	}
}

func TestGetReturnsPublishedModel(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	r := readRequest("/v1/models/team-chat")
	r.SetPathValue("model", "team-chat")
	testHandler(t).Get(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	var got struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "team-chat" || got.Object != "model" {
		t.Fatalf("model = %+v", got)
	}
}

func TestNativeListUsesAnthropicEnvelope(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	testHandler(t).Native(recorder, readRequest("/anthropic/v1/models"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
	var got struct {
		HasMore bool `json:"has_more"`
		Data    []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 1 || got.Data[0].ID != "team-chat" || got.Data[0].Type != "model" {
		t.Fatalf("native list = %+v", got)
	}
}

func TestGetUnknownModelIsNotFound(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	r := readRequest("/v1/models/nope")
	r.SetPathValue("model", "nope")
	testHandler(t).Get(recorder, r)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
}

func TestListRequiresAKey(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	testHandler(t).List(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %s", recorder.Code, recorder.Body)
	}
}
