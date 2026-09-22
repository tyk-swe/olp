package gateway

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestConfiguredResourceAndRealtimeUseThePinnedConnection(t *testing.T) {
	h := newHarness(t, Config{})
	seen := make(chan string, 4)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != secretA || r.Header.Get("OpenAI-Beta") != "fixture-v1" {
			t.Error("profile headers/auth missing")
		}
		seen <- r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/realtime") {
			connection, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.CloseNow()
			kind, data, err := connection.Read(r.Context())
			if err == nil {
				_ = connection.Write(r.Context(), kind, data)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp-owned","status":"completed"}`)
	}))
	defer upstream.Close()
	var provider runtime.Provider
	for _, value := range h.rt.release.Snapshot.Providers {
		if value.Name == "a" {
			provider = value
		}
	}
	provider.Kind = "azure_openai"
	provider.ProfileID = "azure-v1-responses"
	provider.ProfileRevision = "1"
	provider.Endpoint = upstream.URL
	provider.SemanticHeaders = map[string]string{"OpenAI-Beta": "fixture-v1"}
	provider.Network = &egress.ConnectionOptions{TrustRootsPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))}
	x := &execution{request: request{release: h.rt.release, startedAt: time.Now()}, family: openai.FamilyResponses, mode: "unary"}
	p := &pin{provider: provider, slot: provider.Slots[0], model: modelA, attempt: runtime.Attempt{ProviderID: provider.ID, ProviderRevisionID: provider.RevisionID, UpstreamModel: modelA}}
	endpoint, err := provider.Connector().ResourceURL(modelA, "responses/resp-owned", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, failure := h.gateway.pinnedDo(t.Context(), x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		t.Fatalf("pinned resource TLS failed: %+v", failure)
	}
	response.Body.Close()
	if path := <-seen; path != "/openai/v1/responses/resp-owned" {
		t.Fatal(path)
	}
	endpoint, err = provider.Connector().RealtimeURL(modelA)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	connection, problem := realtimeDial(ctx, h.gateway, x, p, endpoint)
	if problem != nil {
		t.Fatal(problem.Code)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update"}`)); err != nil {
		t.Fatal(err)
	}
	_, data, err := connection.Read(ctx)
	if err != nil || string(data) != `{"type":"session.update"}` {
		t.Fatalf("duplex data=%s err=%v", data, err)
	}
	if path := <-seen; path != "/openai/v1/realtime" {
		t.Fatal(path)
	}
	h.gateway.connections.CloseIdleConnections()
}

func TestConfiguredDefaultBoundsReachSharedAdmission(t *testing.T) {
	h := newHarness(t, Config{})
	var provider runtime.Provider
	for _, value := range h.rt.release.Snapshot.Providers {
		if value.Name == "a" {
			provider = value
		}
	}
	provider.ProfileID = "compatible-chat"
	provider.ProfileRevision = "1"
	provider.OperationDefaults = map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage(`9000`)}}}
	request, err := openai.Parse(openai.FamilyChat, []byte(`{"model":"team-chat","messages":[{"role":"user","content":"abcd"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	x := &execution{parsed: request, attempts: []runtime.Attempt{{ProviderID: provider.ID, UpstreamModel: modelA}}}
	if got := x.providerEstimate(&provider); got < 9001 {
		t.Fatalf("configured bound omitted from shared reservation: %d", got)
	}
	prepared, err := x.preparedProvider(&provider, modelA)
	if err != nil {
		t.Fatal(err)
	}
	field, _ := prepared.invocation.Prepared.Document().Lookup("/max_tokens")
	if field.Raw() != "9000" {
		t.Fatal("admission and dispatch use different controls")
	}
}
