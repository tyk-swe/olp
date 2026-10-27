package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/usage"
)

type bedrockPeer struct {
	stream *connectors.BedrockStream
}

func (p *bedrockPeer) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	payload, err := p.stream.Receive(ctx)
	if err != nil {
		return 0, nil, err
	}
	return websocket.MessageText, payload, nil
}

func (p *bedrockPeer) Write(ctx context.Context, _ websocket.MessageType, payload []byte) error {
	return p.stream.Send(ctx, payload)
}

func (*bedrockPeer) Ping(context.Context) error { return nil }

func (p *bedrockPeer) Close(websocket.StatusCode, string) error { return p.stream.Close() }
func (p *bedrockPeer) CloseNow() error                          { return p.stream.Close() }

func (s *Server) openBedrockStream(ctx context.Context, x *call, p *pin) (duplexPeer, error) {
	secret, err := s.slotSecret(ctx, x, p.slot, &p.provider, p.model)
	if err != nil {
		return nil, err
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, err
	}
	stream, applied, err := s.auth.OpenBedrockDuplex(ctx, p.provider.Connector(), secret, p.model, client)
	x.sensitive.Include(applied)
	if err != nil {
		return nil, err
	}
	return &bedrockPeer{stream: stream}, nil
}

func (s *Server) bedrockDuplexDial(ctx context.Context, x *call, p *pin) (duplexPeer, *Error) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1, &p.provider)
	fact.Mode = "realtime"
	if p.hold != nil {
		fact.Budgets = p.hold.budgets
	}
	finish := func(class string, e *Error) (duplexPeer, *Error) {
		fact.Class = class
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(false)
		x.facts = append(x.facts, fact)
		return nil, e
	}
	endpoint, err := p.provider.Connector().BedrockDuplexEndpoint()
	if err != nil {
		return finish(classCredential, serverError(http.StatusBadGateway, "upstream_error", "The Bedrock duplex profile cannot address this model."))
	}
	if _, err := s.egress.ValidateEndpoint(endpoint); err != nil {
		return finish(classConnect, serverError(http.StatusBadGateway, "upstream_error", "The Bedrock provider address is not permitted."))
	}
	opener := s.bedrockStream
	if opener == nil {
		opener = s.openBedrockStream
	}
	dialCtx, cancel := context.WithCancel(ctx)
	dialTimer := time.AfterFunc(p.attempt.Timeout, cancel)
	peer, err := opener(dialCtx, x, p)
	dialTimer.Stop()
	cancel()
	if err != nil {
		class := classConnect
		if errors.Is(err, connectors.ErrCredentialRejected) {
			class = classCredential
		}
		return finish(class, serverError(http.StatusBadGateway, "upstream_error", "The Bedrock bidirectional stream could not be opened."))
	}
	fact.Class = "success"
	fact.Committed = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamAccepted
	}
	fact.Duration = s.now().Sub(fact.StartedAt)
	fact.recordEvidence(true)
	x.facts = append(x.facts, fact)
	x.dispatched = true
	if p.hold != nil {
		x.spendCaps(p.hold.budgets, p.attempt)
	}
	return peer, nil
}
