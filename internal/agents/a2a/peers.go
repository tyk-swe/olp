package a2a

import (
	"context"
	"errors"
	"iter"
	"maps"
	"net/http"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/egress"
)

type Peers struct {
	egress      *egress.Policy
	credentials func(context.Context, string) ([]byte, error)
}

func NewPeers(policy *egress.Policy, credentials func(context.Context, string) ([]byte, error)) *Peers {
	return &Peers{egress: policy, credentials: credentials}
}
func (p *Peers) credential(ctx context.Context, definition agentcontract.Definition, oauth []byte) ([]byte, error) {
	if len(oauth) > 0 {
		return append([]byte(nil), oauth...), nil
	}
	if definition.OAuth != nil || definition.CredentialID == nil {
		return nil, nil
	}
	if p.credentials == nil {
		return nil, agents.ErrCredential
	}
	credential, err := p.credentials(ctx, *definition.CredentialID)
	if err != nil || len(credential) == 0 {
		clear(credential)
		return nil, agents.ErrCredential
	}
	return credential, nil
}
func (p *Peers) Card(ctx context.Context, definition agentcontract.Definition) (protocol.AgentCard, error) {
	credential, err := p.credential(ctx, definition, nil)
	defer clear(credential)
	if err != nil {
		return protocol.AgentCard{}, err
	}
	return p.loadCard(ctx, definition, credential)
}
func (p *Peers) Open(ctx context.Context, definition agentcontract.Definition, oauth []byte, validateCard bool) (agents.Peer, error) {
	if !supportedProtocol(definition.Protocol) || p.egress == nil {
		return nil, agents.ErrInternal
	}
	if definition.OAuth != nil && len(oauth) == 0 {
		return nil, agents.ErrCredential
	}
	if _, err := p.egress.ValidateEndpoint(definition.Endpoint); err != nil {
		return nil, err
	}
	credential, err := p.credential(ctx, definition, oauth)
	defer clear(credential)
	if err != nil {
		return nil, err
	}
	card, cardErr := func() (protocol.AgentCard, error) {
		if !validateCard {
			return protocol.AgentCard{}, nil
		}
		return p.loadCard(ctx, definition, credential)
	}()
	if cardErr != nil {
		return nil, cardErr
	}
	base := p.egress.Client(30 * time.Second)
	client := &http.Client{Transport: limitedTransport{Base: base.Transport}, CheckRedirect: base.CheckRedirect, Jar: base.Jar, Timeout: base.Timeout}
	return &peer{transport: upstreamTransport(definition.Protocol, definition.Endpoint, client), version: definition.Protocol, credential: string(credential), streaming: card.Capabilities.Streaming}, nil
}

type peer struct {
	transport  a2aclient.Transport
	version    string
	credential string
	streaming  bool
}

func (p *peer) headers(capability string) a2aclient.ServiceParams {
	return a2aclient.ServiceParams(maps.Collect[string, []string](func(yield func(string, []string) bool) {
		if !yield("A2A-Version", []string{string(cardVersion(p.version))}) {
			return
		}
		if p.credential != "" && !yield("Authorization", []string{"Bearer " + p.credential}) {
			return
		}
		if capability != "" {
			yield("X-OLP-Task-Capability", []string{capability})
		}
	}))
}
func (p *peer) Streaming() bool { return p.streaming }
func (p *peer) Close()          { _ = p.transport.Destroy() }
func (p *peer) Send(ctx context.Context, request *protocol.SendMessageRequest, capability string) (protocol.SendMessageResult, error) {
	return p.transport.SendMessage(ctx, p.headers(capability), request)
}
func (p *peer) Stream(ctx context.Context, request *protocol.SendMessageRequest, capability string) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		next, stop := iter.Pull2(p.transport.SendStreamingMessage(ctx, p.headers(capability), request))
		defer stop()
		event, err, ok := next()
		if !ok {
			return
		}
		if err != nil {
			yield(nil, err)
			return
		}
		if p.version == currentProtocol && request.Message.TaskID == "" {
			switch event.(type) {
			case *protocol.Task, *protocol.Message:
			default:
				yield(nil, agents.ErrInvalidStream)
				return
			}
		}
		if !yield(event, nil) {
			return
		}
		for {
			nextEvent, nextErr, more := next()
			if !more {
				return
			}
			if !yield(nextEvent, nextErr) || nextErr != nil {
				return
			}
		}
	}
}
func (p *peer) Get(ctx context.Context, request *protocol.GetTaskRequest) (*protocol.Task, error) {
	return p.transport.GetTask(ctx, p.headers(""), request)
}
func (p *peer) Cancel(ctx context.Context, request *protocol.CancelTaskRequest) (*protocol.Task, error) {
	return p.transport.CancelTask(ctx, p.headers(""), request)
}
func (p *peer) Subscribe(ctx context.Context, request *protocol.SubscribeToTaskRequest) iter.Seq2[protocol.Event, error] {
	return p.transport.SubscribeToTask(ctx, p.headers(""), request)
}

var errMethodNotDeclared = errors.New("method not declared")
