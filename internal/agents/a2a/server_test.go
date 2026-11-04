package a2a_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type runtimeFixture struct {
	authority access.Authority
	snapshot  *release.Release
}

func (r runtimeFixture) Authenticate(string) (*access.Authority, error) { return &r.authority, nil }
func (r runtimeFixture) ReauthenticateDelegation(context.Context, string, string) (*access.Authority, error) {
	return &r.authority, nil
}
func (r runtimeFixture) Release() *release.Release { return r.snapshot }
func (runtimeFixture) AuthorizeAgentResource(access.Authority, string, string, string, string) error {
	return nil
}

type taskFixture struct {
	agents.TaskStore
	dispatch *dispatchFixture
}

func (s taskFixture) Prepare(context.Context, tasks.SendIntent) (tasks.Dispatch, error) {
	return s.dispatch, nil
}

type dispatchFixture struct {
	tasks.Dispatch
	settled   chan tasks.Outcome
	uncertain chan struct{}
}

func (d *dispatchFixture) Capability() string                   { return "callback-capability" }
func (d *dispatchFixture) MarkDispatched(context.Context) error { return nil }
func (d *dispatchFixture) Settle(_ context.Context, outcome tasks.Outcome) (string, error) {
	d.settled <- outcome
	return "public-task", nil
}
func (d *dispatchFixture) Finish(context.Context)  {}
func (d *dispatchFixture) Unknown(context.Context) { d.uncertain <- struct{}{} }
func cardBody(t *testing.T, endpoint, version string) []byte {
	t.Helper()
	card := &protocol.AgentCard{Name: "fixture", Version: "test", Capabilities: protocol.AgentCapabilities{Streaming: true}, SupportedInterfaces: []*protocol.AgentInterface{{URL: endpoint, ProtocolBinding: protocol.TransportProtocolJSONRPC, ProtocolVersion: func() protocol.ProtocolVersion {
		if version == "a2a-0.3.0" {
			return a2av0.Version
		}
		return protocol.Version
	}()}}}
	if version == "a2a-0.3.0" {
		converted, err := a2av0.NewStaticAgentCardProducer(card).(a2asrv.AgentCardJSONProducer).CardJSON(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return converted
	}
	data, err := json.Marshal(card)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestA2AHTTPVersionsAndSingleCredentialResolution(t *testing.T) {
	for _, version := range []string{"a2a-0.3.0", "a2a-1.0.0"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", version, streaming), func(t *testing.T) {
				sends, credentials := make(chan string, 4), make(chan struct{}, 4)
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Error("missing upstream authentication")
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/.well-known/agent-card.json" {
						_, _ = w.Write(cardBody(t, "http://"+r.Host+"/rpc", version))
						return
					}
					var request struct {
						ID     json.RawMessage `json:"id"`
						Method string          `json:"method"`
						Params json.RawMessage `json:"params"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("invalid upstream request")
						return
					}
					if r.Header.Get("X-OLP-Task-Capability") != "callback-capability" {
						t.Error("missing callback capability")
					}
					sends <- request.Method
					task := &protocol.Task{ID: "private-task", ContextID: "private-context", Status: protocol.TaskStatus{State: protocol.TaskStateCompleted}}
					result := func() any {
						if version == "a2a-0.3.0" {
							return a2av0.FromV1Task(task)
						}
						return protocol.StreamResponse{Event: task}
					}()
					response, err := json.Marshal(struct {
						JSONRPC string          `json:"jsonrpc"`
						ID      json.RawMessage `json:"id"`
						Result  any             `json:"result"`
					}{"2.0", request.ID, result})
					if err != nil {
						t.Error(err)
						return
					}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", response)
						return
					}
					_, _ = w.Write(response)
				}))
				defer remote.Close()
				endpoint := remote.URL + "/rpc"
				digest := sha256.Sum256(cardBody(t, endpoint, version))
				project, credential := "project", "credential"
				revision := agentcontract.Revision{ID: "revision", PublicationID: "publication", ProjectID: project, Kind: "agent", Current: true, Definition: agentcontract.Draft{ProjectID: project, Agent: &agentcontract.Definition{Protocol: version, Endpoint: endpoint, CardDigest: hex.EncodeToString(digest[:]), CredentialID: &credential}}}
				authority := access.Authority{ID: "owner", ProjectID: &project, Policy: access.KeyPolicy{Scopes: []string{"agents"}}}
				runtime := runtimeFixture{authority: authority, snapshot: &release.Release{ID: "release", Snapshot: &routing.Snapshot{AgentPublications: []agentcontract.Revision{revision}}}}
				peers := a2a.NewPeers(&egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, func(context.Context, string) ([]byte, error) {
					credentials <- struct{}{}
					return []byte("upstream-secret"), nil
				})
				dispatch := &dispatchFixture{settled: make(chan tasks.Outcome, 4), uncertain: make(chan struct{}, 4)}
				executor := agents.New(agents.Options{Tasks: taskFixture{dispatch: dispatch}, Peers: peers})
				server := a2a.New(a2a.Options{Runtime: runtime, Executor: executor, Peers: peers})
				mux := http.NewServeMux()
				server.Register(mux)
				method := func() string {
					if version == "a2a-0.3.0" {
						if streaming {
							return "message/stream"
						}
						return "message/send"
					}
					if streaming {
						return "SendStreamingMessage"
					}
					return "SendMessage"
				}()
				params := func() []byte {
					message := &protocol.Message{ID: "message", Role: protocol.MessageRoleUser, Parts: protocol.ContentParts{protocol.NewTextPart("hello")}}
					if version == "a2a-0.3.0" {
						return []byte(`{"message":{"kind":"message","messageId":"message","role":"user","parts":[{"kind":"text","text":"hello"}]}}`)
					}
					encoded, err := json.Marshal(protocol.SendMessageRequest{Message: message})
					if err != nil {
						t.Fatal(err)
					}
					return encoded
				}()
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"request","method":%q,"params":%s}`, method, params)
				request := httptest.NewRequest(http.MethodPost, "/a2a/publication", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer consumer-secret")
				request.Header.Set("A2A-Version", strings.TrimPrefix(version, "a2a-"))
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte("public-task")) || bytes.Contains(response.Body.Bytes(), []byte("private-task")) || bytes.Contains(response.Body.Bytes(), []byte("private-context")) {
					t.Fatalf("incorrect public response (%d): %s", response.Code, response.Body.String())
				}
				if len(sends) != 1 || len(credentials) != 1 || len(dispatch.settled) != 1 || len(dispatch.uncertain) != 0 {
					t.Fatalf("send did not use one credential and one settled dispatch: sends=%d credentials=%d settled=%d unknown=%d", len(sends), len(credentials), len(dispatch.settled), len(dispatch.uncertain))
				}
				if (<-sends) != method {
					t.Fatal("wrong upstream method for protocol")
				}
				if streaming && response.Header().Get("Content-Type") != "text/event-stream" {
					t.Fatal("stream was not returned as SSE")
				}
			})
		}
	}
}
